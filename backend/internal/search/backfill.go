package search

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// BackfillOptions controls one resumable epoch build.  A new target version is
// expected for each epoch; activation is deliberately opt-in so operators can
// run shadow checks before changing reads.
type BackfillOptions struct {
	TargetVersion string
	IndexEpoch    int64
	BatchSize     int
	Activate      bool
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
		if err := preparer.PrepareEpoch(ctx, worker.target, 1024); err != nil {
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
		for _, version := range versions.all {
			if err := worker.index.ActivateVersion(ctx, version.ID); err != nil {
				return BackfillReport{}, err
			}
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
	for _, projectionType := range []string{ProjectionFile, ProjectionContent} {
		var selected IndexVersion
		for _, backend := range backends {
			version, err := p.index.StartVersion(ctx, backend, projectionType, p.target, p.indexEpoch)
			if err != nil {
				return backfillVersions{}, err
			}
			result.all = append(result.all, version)
			if backend == BackendPostgres {
				selected = version
			}
		}
		result.canonical[projectionType] = selected
	}
	return result, nil
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
		_ = p.index.SetVersionState(ctx, version.ID, "building", "failed", &message)
	}
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
		var rows []fileRow
		if err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ?", BackendPostgres, p.target).Order("id ASC").Find(&rows).Error; err != nil {
			return "", err
		}
		for _, row := range rows {
			writeChecksum(h, row.ID, row.ObjectID, row.FileVersion, row.ACLVersion, row.IndexEpoch, row.Tombstone, row.Name)
		}
	} else {
		var rows []contentRow
		if err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ?", BackendPostgres, p.target).Order("id ASC").Find(&rows).Error; err != nil {
			return "", err
		}
		for _, row := range rows {
			writeChecksum(h, row.ID, row.ObjectID, row.DocumentVersionID, int64(row.Ordinal), row.IndexEpoch, row.State == "tombstoned", row.Content)
		}
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func writeChecksum(h hash.Hash, id, objectID uuid.UUID, values ...any) {
	_, _ = fmt.Fprintf(h, "%s|%s|", id, objectID)
	for _, value := range values {
		_, _ = fmt.Fprintf(h, "%v|", value)
	}
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
