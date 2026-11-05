package search

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// BackfillOptions controls one resumable epoch build.  A new target version is
// expected for each epoch; activation is deliberately opt-in so operators can
// run shadow checks before changing reads.
type BackfillOptions struct {
	TargetVersion string
	IndexEpoch    int64
	BatchSize     int
	Activate      bool
	Dimension     int64
}

type BackfillReport struct {
	TargetVersion   string
	IndexEpoch      int64
	FileCursor      uuid.UUID
	ContentCursor   uuid.UUID
	FileCount       int
	ContentCount    int
	FileChecksum    string
	ContentChecksum string
	Activated       bool
	StartedAt       time.Time
	FinishedAt      time.Time
}

// ActivateVersion exposes the explicit cutover operation to operator tools
// without leaking the PostgresIndex implementation outside this package.
func (p *Projector) ActivateVersion(ctx context.Context, id uuid.UUID) error {
	if p == nil || p.index == nil {
		return ErrInvalidIndexVersion
	}
	return p.index.ActivateVersion(ctx, id)
}

func (p *Projector) RollbackVersion(ctx context.Context, backend, projectionType, targetVersion string) error {
	if p == nil || p.index == nil {
		return ErrInvalidIndexVersion
	}
	return p.index.RollbackVersion(ctx, backend, projectionType, targetVersion)
}

// Backfill rebuilds canonical PostgreSQL projections and the optional
// external writers using keyset pagination.  Every batch commits its cursor in
// search_index_versions, so a process restart resumes without a second scan
// and all writes remain safe to replay.
func (p *Projector) Backfill(ctx context.Context, options BackfillOptions) (BackfillReport, error) {
	started := time.Now().UTC()
	if p == nil || p.db == nil || p.index == nil {
		return BackfillReport{}, errors.New("search: nil projector")
	}
	if options.TargetVersion == "" {
		options.TargetVersion = p.target
	}
	if options.IndexEpoch <= 0 {
		options.IndexEpoch = p.indexEpoch
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 100
	}
	if options.TargetVersion == "" || options.IndexEpoch <= 0 {
		return BackfillReport{}, ErrInvalidIndexVersion
	}
	worker := *p
	worker.target = options.TargetVersion
	worker.indexEpoch = options.IndexEpoch
	if preparer, ok := worker.external.(EpochPreparer); ok {
		if err := preparer.PrepareEpoch(ctx, worker.target, options.Dimension); err != nil {
			return BackfillReport{}, err
		}
	}
	versions, err := worker.ensureBackfillVersions(ctx)
	if err != nil {
		return BackfillReport{}, err
	}
	fileVersion := versions.canonical[ProjectionFile]
	contentVersion := versions.canonical[ProjectionContent]
	if fileVersion.State != "building" || contentVersion.State != "building" {
		return BackfillReport{}, fmt.Errorf("search: target version is not building")
	}

	fileCursor := uuid.Nil
	contentCursor := uuid.Nil
	fileCount := int64(0)
	contentCount := int64(0)
	if fileVersion.SnapshotCursor != nil {
		fileCursor = *fileVersion.SnapshotCursor
	}
	if contentVersion.SnapshotCursor != nil {
		contentCursor = *contentVersion.SnapshotCursor
	}
	if fileVersion.SnapshotWatermark != nil {
		fileCount = *fileVersion.SnapshotWatermark
	}
	if contentVersion.SnapshotWatermark != nil {
		contentCount = *contentVersion.SnapshotWatermark
	}

	for {
		next, count, snapshotErr := worker.Snapshot(ctx, fileCursor, options.BatchSize)
		if snapshotErr != nil {
			worker.failBackfillVersions(ctx, versions.all, snapshotErr)
			return BackfillReport{}, snapshotErr
		}
		if count == 0 {
			break
		}
		fileCursor = next
		fileCount += int64(count)
		if err := worker.persistBackfillProgress(ctx, versions.canonical[ProjectionFile].ID, fileCursor, fileCount); err != nil {
			worker.failBackfillVersions(ctx, versions.all, err)
			return BackfillReport{}, err
		}
	}
	for {
		next, count, snapshotErr := worker.SnapshotContent(ctx, contentCursor, options.BatchSize)
		if snapshotErr != nil {
			worker.failBackfillVersions(ctx, versions.all, snapshotErr)
			return BackfillReport{}, snapshotErr
		}
		if count == 0 {
			break
		}
		contentCursor = next
		contentCount += int64(count)
		if err := worker.persistBackfillProgress(ctx, versions.canonical[ProjectionContent].ID, contentCursor, contentCount); err != nil {
			worker.failBackfillVersions(ctx, versions.all, err)
			return BackfillReport{}, err
		}
	}

	fileChecksum, err := worker.ProjectionChecksum(ctx, ProjectionFile)
	if err != nil {
		worker.failBackfillVersions(ctx, versions.all, err)
		return BackfillReport{}, err
	}
	contentChecksum, err := worker.ProjectionChecksum(ctx, ProjectionContent)
	if err != nil {
		worker.failBackfillVersions(ctx, versions.all, err)
		return BackfillReport{}, err
	}
	if err := worker.finishBackfillProgress(ctx, versions, fileCursor, contentCursor, fileCount, contentCount, fileChecksum, contentChecksum); err != nil {
		worker.failBackfillVersions(ctx, versions.all, err)
		return BackfillReport{}, err
	}
	if options.Activate {
		if err := worker.ActivateTarget(ctx, worker.target); err != nil {
			return BackfillReport{}, err
		}
	}
	return BackfillReport{TargetVersion: options.TargetVersion, IndexEpoch: options.IndexEpoch, FileCursor: fileCursor, ContentCursor: contentCursor, FileCount: int(fileCount), ContentCount: int(contentCount), FileChecksum: fileChecksum, ContentChecksum: contentChecksum, Activated: options.Activate, StartedAt: started, FinishedAt: time.Now().UTC()}, nil
}

type backfillVersions struct {
	canonical map[string]IndexVersion
	all       []IndexVersion
}

func (p *Projector) ensureBackfillVersions(ctx context.Context) (backfillVersions, error) {
	backends := []string{BackendPostgres}
	if p.external != nil {
		if provider, ok := p.external.(interface{ Backends() []string }); ok {
			backends = append(backends, provider.Backends()...)
		} else {
			backends = append(backends, BackendES, BackendQdrant)
		}
	}
	result := backfillVersions{canonical: make(map[string]IndexVersion, 2)}
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []IndexVersion
		if err := tx.Where("target_version = ?", p.target).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 0 {
			expected := make(map[string]bool)
			for _, backend := range backends {
				expected[backend] = true
			}
			if len(existing) != 2*len(expected) {
				return ErrIndexVersionConflict
			}
			for _, version := range existing {
				if !expected[version.Backend] {
					return ErrIndexVersionConflict
				}
			}
		}
		index := NewPostgresIndex(tx)
		for _, projectionType := range []string{ProjectionFile, ProjectionContent} {
			var selected IndexVersion
			for _, backend := range backends {
				version, err := index.StartVersion(ctx, backend, projectionType, p.target, p.indexEpoch)
				if err != nil {
					return err
				}
				result.all = append(result.all, version)
				if backend == BackendPostgres {
					selected = version
				}
			}
			result.canonical[projectionType] = selected
		}
		return nil
	})
	return result, err
}

func (p *Projector) persistBackfillProgress(ctx context.Context, id uuid.UUID, cursor uuid.UUID, count int64) error {
	checksum := fmt.Sprintf("cursor:%s;count:%d", cursor, count)
	return p.index.UpdateVersionCursor(ctx, id, &cursor, &count, &checksum)
}

func (p *Projector) finishBackfillProgress(ctx context.Context, versions backfillVersions, fileCursor, contentCursor uuid.UUID, fileCount, contentCount int64, fileChecksum, contentChecksum string) error {
	for projection, version := range versions.canonical {
		cursor, count, checksum := fileCursor, fileCount, fileChecksum
		if projection == ProjectionContent {
			cursor, count, checksum = contentCursor, contentCount, contentChecksum
		}
		if cursor == uuid.Nil {
			if err := p.index.UpdateVersionProgress(ctx, version.ID, &count, &checksum); err != nil {
				return err
			}
		} else if err := p.index.UpdateVersionCursor(ctx, version.ID, &cursor, &count, &checksum); err != nil {
			return err
		}
	}
	// External versions are control-plane records for the same snapshot. They
	// share the canonical cursor/checksum and are activated together below.
	for _, version := range versions.all {
		if version.Backend == BackendPostgres {
			continue
		}
		cursor, count, checksum := fileCursor, fileCount, fileChecksum
		if version.ProjectionType == ProjectionContent {
			cursor, count, checksum = contentCursor, contentCount, contentChecksum
		}
		if cursor == uuid.Nil {
			if err := p.index.UpdateVersionProgress(ctx, version.ID, &count, &checksum); err != nil {
				return err
			}
		} else if err := p.index.UpdateVersionCursor(ctx, version.ID, &cursor, &count, &checksum); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) failBackfillVersions(ctx context.Context, versions []IndexVersion, cause error) {
	message := limitErrorText(cause)
	for _, version := range versions {
		// A timeout or temporary outage must retain the resumable cursor.
		// The final checksum is written only after both scans finish.
		updates := map[string]any{"last_error": message, "updated_at": time.Now().UTC()}
		if errors.Is(cause, ErrPermanent) {
			updates["state"] = "failed"
		}
		_ = p.db.WithContext(ctx).Model(&IndexVersion{}).Where("id = ? AND state = 'building'", version.ID).Updates(updates).Error
	}
}

// ActivateTarget changes the complete backend/projection set in one SQL
// transaction. This is control-plane metadata; query processes still select
// their target through SEARCH_INDEX_EPOCH.
func (p *Projector) ActivateTarget(ctx context.Context, target string) error {
	if p == nil || p.db == nil || target == "" {
		return ErrInvalidIndexVersion
	}
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var versions []IndexVersion
		if err := tx.Where("target_version = ?", target).Order("backend, projection_type").Find(&versions).Error; err != nil {
			return err
		}
		if len(versions) == 0 {
			return gorm.ErrRecordNotFound
		}
		epoch, err := completeTarget(versions)
		if err != nil {
			return err
		}
		worker := *p
		worker.target, worker.indexEpoch = target, epoch
		var unfinished int64
		if err := tx.Model(&ProjectionCheckpoint{}).Where("consumer_name = ? AND state IN ?", worker.consumerName(), []string{"pending", "publishing", "failed"}).Count(&unfinished).Error; err != nil {
			return err
		}
		if unfinished > 0 {
			return fmt.Errorf("%w: unfinished external publications", ErrIndexVersionConflict)
		}
		index := NewPostgresIndex(tx)
		for _, version := range versions {
			if version.State == "active" {
				continue
			}
			if err := index.ActivateVersion(ctx, version.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func completeTarget(versions []IndexVersion) (int64, error) {
	if len(versions) == 0 {
		return 0, gorm.ErrRecordNotFound
	}
	members := make(map[string]int)
	epoch := versions[0].IndexEpoch
	for _, version := range versions {
		if version.IndexEpoch != epoch {
			return 0, ErrIndexVersionConflict
		}
		members[version.Backend]++
	}
	if members[BackendPostgres] != 2 {
		return 0, ErrIndexVersionConflict
	}
	for _, count := range members {
		if count != 2 {
			return 0, ErrIndexVersionConflict
		}
	}
	return epoch, nil
}

// RollbackTarget changes metadata for the complete target atomically. The
// operator must first catch up its data and separately switch query config.
func (p *Projector) RollbackTarget(ctx context.Context, target string) error {
	if p == nil || p.db == nil || target == "" {
		return ErrInvalidIndexVersion
	}
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var versions []IndexVersion
		if err := tx.Where("target_version = ?", target).Order("backend, projection_type").Find(&versions).Error; err != nil {
			return err
		}
		if _, err := completeTarget(versions); err != nil {
			return err
		}
		index := NewPostgresIndex(tx)
		for _, version := range versions {
			if err := index.RollbackVersion(ctx, version.Backend, version.ProjectionType, target); err != nil {
				return err
			}
		}
		return nil
	})
}

type ProjectionStatus struct {
	TargetVersion string
	ConsumerName  string
	Checkpoints   map[string]int64
}

// Status reports publication attempts for the selected generation/writer set.
// Empty checkpoints do not prove that the broker or source has been drained.
func (p *Projector) Status(ctx context.Context) (ProjectionStatus, error) {
	if p == nil || p.db == nil {
		return ProjectionStatus{}, ErrInvalidIndexVersion
	}
	var rows []struct {
		State string
		Count int64
	}
	if err := p.db.WithContext(ctx).Model(&ProjectionCheckpoint{}).Select("state, count(*) AS count").Where("consumer_name = ?", p.consumerName()).Group("state").Scan(&rows).Error; err != nil {
		return ProjectionStatus{}, err
	}
	status := ProjectionStatus{TargetVersion: p.target, ConsumerName: p.consumerName(), Checkpoints: map[string]int64{}}
	for _, row := range rows {
		status.Checkpoints[row.State] = row.Count
	}
	return status, nil
}

// ProjectionChecksum hashes stable IDs and version-bearing fields from the
// canonical projection. It is intentionally independent of external response
// ordering and can be compared after a resumed run.
func (p *Projector) ProjectionChecksum(ctx context.Context, projectionType string) (string, error) {
	if p == nil || p.db == nil || (projectionType != ProjectionFile && projectionType != ProjectionContent) {
		return "", ErrInvalidIndexVersion
	}
	h := sha256.New()
	if projectionType == ProjectionFile {
		rows, err := p.db.WithContext(ctx).Model(&fileRow{}).Where("backend = ? AND target_version = ?", BackendPostgres, p.target).Order("id ASC").Rows()
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var row fileRow
			if err := p.db.ScanRows(rows, &row); err != nil {
				return "", err
			}
			writeChecksum(h, row.ID, row.ObjectID, row.OwnerID, row.FolderID, row.FileVersion, row.ACLVersion, row.IndexEpoch, row.Tombstone, row.Name, row.MIMEType)
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
	} else {
		rows, err := p.db.WithContext(ctx).Model(&contentRow{}).Where("backend = ? AND target_version = ?", BackendPostgres, p.target).Order("id ASC").Rows()
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var row contentRow
			if err := p.db.ScanRows(rows, &row); err != nil {
				return "", err
			}
			writeChecksum(h, row.ID, row.ObjectID, row.DocumentVersionID, int64(row.Ordinal), row.IndexEpoch, row.State == "tombstoned", row.Content, row.PipelineVersion, row.ModelVersion, row.VectorRef)
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func writeChecksum(h hash.Hash, id, objectID uuid.UUID, values ...any) {
	// JSON frames strings and nils unambiguously; separators inside source
	// content must not make two different rows hash as the same byte stream.
	_ = json.NewEncoder(h).Encode([]any{id, objectID, values})
}

// IndexVersions returns control-plane rows for an operator status report.
func (p *Projector) IndexVersions(ctx context.Context, backend, target string) ([]IndexVersion, error) {
	if p == nil || p.db == nil || backend == "" || target == "" {
		return nil, ErrInvalidIndexVersion
	}
	var rows []IndexVersion
	err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ?", backend, target).Order("projection_type ASC").Find(&rows).Error
	return rows, err
}

// ParseIndexEpoch accepts the human-friendly vN form used by deploy/.env.
func ParseIndexEpoch(value string) int64 {
	if len(value) > 0 && (value[0] == 'v' || value[0] == 'V') {
		value = value[1:]
	}
	epoch, _ := strconv.ParseInt(value, 10, 64)
	return epoch
}
