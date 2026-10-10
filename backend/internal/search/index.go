// Package search owns rebuildable search projections.  It intentionally does
// not expose authorization decisions: callers must resolve candidates back
// through the PostgreSQL business facts before returning them to a user.
package search

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	BackendPostgres   = "postgres"
	BackendES         = "elasticsearch"
	BackendQdrant     = "qdrant"
	ProjectionContent = "content"
	ProjectionFile    = "file"
)

var ErrStaleProjection = errors.New("search projection is older than the stored version")

type ContentProjection struct {
	Backend           string
	TargetVersion     string
	ObjectID          uuid.UUID
	DocumentVersionID uuid.UUID
	ChunkID           uuid.UUID
	Ordinal           int
	PipelineVersion   string
	ModelVersion      string
	IndexEpoch        int64
	Content           string
	VectorRef         *string
	Tombstone         bool
}

type FileProjection struct {
	Backend       string
	TargetVersion string
	FileEntryID   uuid.UUID
	OwnerID       uuid.UUID
	FolderID      uuid.UUID
	ObjectID      uuid.UUID
	FileVersion   int64
	ACLVersion    int64
	Name          string
	MIMEType      string
	IndexEpoch    int64
	Tombstone     bool
}

// IndexVersion is durable control-plane metadata for one backend and
// projection.  Projection rows are intentionally independent from this
// record: a failed/retired version can be rebuilt without touching business
// facts or deleting the previous active version.
type IndexVersion struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	Backend           string
	ProjectionType    string
	TargetVersion     string
	IndexEpoch        int64
	State             string
	SnapshotWatermark *int64
	SnapshotCursor    *uuid.UUID `gorm:"type:uuid"`
	Checksum          *string
	LastError         *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ActivatedAt       *time.Time
}

func (IndexVersion) TableName() string { return "search_index_versions" }

var (
	ErrInvalidIndexVersion  = errors.New("search: invalid index version")
	ErrIndexVersionConflict = errors.New("search: index version state conflict")
)

// Index is the external-index boundary. ES/Qdrant adapters can implement it
// later without leaking client types into AI query code.
type Index interface {
	UpsertContent(context.Context, ContentProjection) error
	UpsertFile(context.Context, FileProjection) error
	TombstoneObjectContent(context.Context, string, uuid.UUID, int64) error
	TombstoneFile(context.Context, string, uuid.UUID, int64) error
	Health(context.Context) error
}

type contentRow struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	Backend           string
	TargetVersion     string
	ObjectID          uuid.UUID
	DocumentVersionID uuid.UUID
	ChunkID           uuid.UUID
	Ordinal           int
	PipelineVersion   string
	ModelVersion      string
	IndexEpoch        int64
	Content           string
	VectorRef         *string
	State             string
	Attempt           int
	LastError         *string
	IndexedAt         *time.Time
	UpdatedAt         time.Time
}

func (contentRow) TableName() string { return "search_content_projections" }

type fileRow struct {
	ID            uuid.UUID `gorm:"type:uuid;primaryKey"`
	Backend       string
	TargetVersion string
	FileEntryID   uuid.UUID
	OwnerID       uuid.UUID
	FolderID      uuid.UUID
	ObjectID      uuid.UUID
	FileVersion   int64
	ACLVersion    int64
	Name          string
	MIMEType      string `gorm:"column:mime_type"`
	Tombstone     bool
	IndexEpoch    int64
	State         string
	Attempt       int
	LastError     *string
	IndexedAt     *time.Time
	UpdatedAt     time.Time
}

func (fileRow) TableName() string { return "search_file_projections" }

// PostgresIndex is the local projection/checkpoint store. It is useful as a
// fallback backend and as the durable state beside ES/Qdrant adapters.
type PostgresIndex struct{ db *gorm.DB }

func NewPostgresIndex(db *gorm.DB) *PostgresIndex { return &PostgresIndex{db: db} }

func (p *PostgresIndex) UpsertContent(ctx context.Context, value ContentProjection) error {
	if p == nil || p.db == nil {
		return errors.New("search: nil postgres index")
	}
	return p.upsertContent(p.db.WithContext(ctx), value)
}

func (p *PostgresIndex) upsertContent(db *gorm.DB, value ContentProjection) error {
	if p == nil || db == nil {
		return errors.New("search: nil postgres index")
	}
	if value.IndexEpoch <= 0 || value.ObjectID == uuid.Nil || value.ChunkID == uuid.Nil {
		return errors.New("search: invalid content projection")
	}
	now := time.Now().UTC()
	row := contentRow{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(value.Backend+":"+value.TargetVersion+":"+value.ChunkID.String())), Backend: value.Backend, TargetVersion: value.TargetVersion, ObjectID: value.ObjectID, DocumentVersionID: value.DocumentVersionID, ChunkID: value.ChunkID, Ordinal: value.Ordinal, PipelineVersion: value.PipelineVersion, ModelVersion: value.ModelVersion, IndexEpoch: value.IndexEpoch, Content: value.Content, VectorRef: value.VectorRef, State: "indexed", IndexedAt: &now, UpdatedAt: now}
	if value.Tombstone {
		row.State = "tombstoned"
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "backend"}, {Name: "target_version"}, {Name: "chunk_id"}}, DoUpdates: clause.AssignmentColumns([]string{"object_id", "document_version_id", "ordinal", "pipeline_version", "model_version", "index_epoch", "content", "vector_ref", "state", "indexed_at", "updated_at"}), Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "search_content_projections.index_epoch <= EXCLUDED.index_epoch AND (search_content_projections.state <> 'tombstoned' OR EXCLUDED.state = 'tombstoned' OR search_content_projections.index_epoch < EXCLUDED.index_epoch)"}}}}).Create(&row).Error
}

func (p *PostgresIndex) UpsertFile(ctx context.Context, value FileProjection) error {
	if p == nil || p.db == nil {
		return errors.New("search: nil postgres index")
	}
	return p.upsertFile(p.db.WithContext(ctx), value)
}

func (p *PostgresIndex) upsertFile(db *gorm.DB, value FileProjection) error {
	if p == nil || db == nil {
		return errors.New("search: nil postgres index")
	}
	if value.IndexEpoch <= 0 || value.FileEntryID == uuid.Nil || value.OwnerID == uuid.Nil {
		return errors.New("search: invalid file projection")
	}
	if value.ACLVersion <= 0 {
		value.ACLVersion = 1
	}
	state := "indexed"
	if value.Tombstone {
		state = "tombstoned"
	}
	now := time.Now().UTC()
	row := fileRow{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(value.Backend+":"+value.TargetVersion+":"+value.FileEntryID.String())), Backend: value.Backend, TargetVersion: value.TargetVersion, FileEntryID: value.FileEntryID, OwnerID: value.OwnerID, FolderID: value.FolderID, ObjectID: value.ObjectID, FileVersion: value.FileVersion, ACLVersion: value.ACLVersion, Name: value.Name, MIMEType: value.MIMEType, Tombstone: value.Tombstone, IndexEpoch: value.IndexEpoch, State: state, IndexedAt: &now, UpdatedAt: now}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "backend"}, {Name: "target_version"}, {Name: "file_entry_id"}}, DoUpdates: clause.AssignmentColumns([]string{"owner_id", "folder_id", "object_id", "file_version", "acl_version", "name", "mime_type", "tombstone", "index_epoch", "state", "indexed_at", "updated_at"}), Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "search_file_projections.file_version <= EXCLUDED.file_version AND search_file_projections.index_epoch <= EXCLUDED.index_epoch AND search_file_projections.acl_version <= EXCLUDED.acl_version AND (NOT search_file_projections.tombstone OR EXCLUDED.tombstone OR search_file_projections.file_version < EXCLUDED.file_version)"}}}}).Create(&row).Error
}

func (p *PostgresIndex) TombstoneObjectContent(ctx context.Context, backend string, objectID uuid.UUID, epoch int64) error {
	if p == nil || p.db == nil {
		return errors.New("search: nil postgres index")
	}
	return p.tombstoneObjectContent(p.db.WithContext(ctx), backend, objectID, epoch)
}

func (p *PostgresIndex) tombstoneObjectContent(db *gorm.DB, backend string, objectID uuid.UUID, epoch int64) error {
	return db.Model(&contentRow{}).Where("backend = ? AND object_id = ? AND index_epoch <= ?", backend, objectID, epoch).Updates(map[string]any{"state": "tombstoned", "index_epoch": epoch, "updated_at": time.Now().UTC()}).Error
}

func (p *PostgresIndex) TombstoneFile(ctx context.Context, backend string, fileID uuid.UUID, version int64) error {
	if p == nil || p.db == nil {
		return errors.New("search: nil postgres index")
	}
	return p.tombstoneFile(p.db.WithContext(ctx), backend, fileID, version)
}

func (p *PostgresIndex) tombstoneFile(db *gorm.DB, backend string, fileID uuid.UUID, version int64) error {
	return db.Model(&fileRow{}).Where("backend = ? AND file_entry_id = ? AND file_version <= ?", backend, fileID, version).Updates(map[string]any{"tombstone": true, "state": "tombstoned", "file_version": version, "updated_at": time.Now().UTC()}).Error
}

func (p *PostgresIndex) Health(ctx context.Context) error {
	if p == nil || p.db == nil {
		return errors.New("search: nil postgres index")
	}
	return p.db.WithContext(ctx).Exec("SELECT 1").Error
}

// StartVersion creates a building version idempotently. The unique active
// index is enforced by PostgreSQL, so two indexers cannot activate competing
// versions for the same backend/projection.
func (p *PostgresIndex) StartVersion(ctx context.Context, backend, projectionType, targetVersion string, epoch int64) (IndexVersion, error) {
	if p == nil || p.db == nil || backend == "" || projectionType == "" || targetVersion == "" || epoch <= 0 {
		return IndexVersion{}, ErrInvalidIndexVersion
	}
	row := IndexVersion{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(backend+":"+projectionType+":"+targetVersion)), Backend: backend, ProjectionType: projectionType, TargetVersion: targetVersion, IndexEpoch: epoch, State: "building", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	err := p.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "backend"}, {Name: "projection_type"}, {Name: "target_version"}}, DoNothing: true}).Create(&row).Error
	if err != nil {
		return IndexVersion{}, err
	}
	if err := p.db.WithContext(ctx).Where("backend = ? AND projection_type = ? AND target_version = ?", backend, projectionType, targetVersion).First(&row).Error; err != nil {
		return IndexVersion{}, err
	}
	return row, nil
}

// SetVersionState performs an explicit state transition. A caller cannot
// accidentally move a retired/failed version back to active without first
// starting a new version.
func (p *PostgresIndex) SetVersionState(ctx context.Context, id uuid.UUID, from, to string, lastError *string) error {
	if p == nil || p.db == nil || id == uuid.Nil || from == "" || to == "" {
		return ErrInvalidIndexVersion
	}
	if !allowedIndexVersionTransition(from, to) {
		return ErrIndexVersionConflict
	}
	now := time.Now().UTC()
	updates := map[string]any{"state": to, "last_error": lastError, "updated_at": now}
	if to == "active" {
		updates["activated_at"] = now
	}
	result := p.db.WithContext(ctx).Model(&IndexVersion{}).Where("id = ? AND state = ?", id, from).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexVersionConflict
	}
	return nil
}

// UpdateVersionProgress records resumable snapshot state without changing
// the active read version. It is safe for a restarted backfill to continue
// from the last committed watermark.
func (p *PostgresIndex) UpdateVersionProgress(ctx context.Context, id uuid.UUID, watermark *int64, checksum *string) error {
	if p == nil || p.db == nil || id == uuid.Nil {
		return ErrInvalidIndexVersion
	}
	result := p.db.WithContext(ctx).Model(&IndexVersion{}).Where("id = ? AND state = 'building'", id).Updates(map[string]any{"snapshot_watermark": watermark, "checksum": checksum, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexVersionConflict
	}
	return nil
}

// UpdateVersionCursor commits a keyset cursor together with the progress
// counters.  The cursor is deliberately opaque to callers so the snapshot
// implementation can use UUIDs without pretending they are ordered numbers.
func (p *PostgresIndex) UpdateVersionCursor(ctx context.Context, id uuid.UUID, cursor *uuid.UUID, watermark *int64, checksum *string) error {
	if p == nil || p.db == nil || id == uuid.Nil {
		return ErrInvalidIndexVersion
	}
	result := p.db.WithContext(ctx).Model(&IndexVersion{}).
		Where("id = ? AND state = 'building'", id).
		Updates(map[string]any{"snapshot_cursor": cursor, "snapshot_watermark": watermark, "checksum": checksum, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexVersionConflict
	}
	return nil
}

// ActivateVersion retires the previous active generation and atomically
// promotes a fully snapshotted building generation. Business facts are never
// modified by this control-plane operation.
func (p *PostgresIndex) ActivateVersion(ctx context.Context, id uuid.UUID) error {
	if p == nil || p.db == nil || id == uuid.Nil {
		return ErrInvalidIndexVersion
	}
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var next IndexVersion
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&next, "id = ?", id).Error; err != nil {
			return err
		}
		if next.State != "building" {
			return ErrIndexVersionConflict
		}
		now := time.Now().UTC()
		if err := tx.Model(&IndexVersion{}).Where("backend = ? AND projection_type = ? AND state = 'active' AND id <> ?", next.Backend, next.ProjectionType, id).Updates(map[string]any{"state": "retired", "updated_at": now}).Error; err != nil {
			return err
		}
		result := tx.Model(&IndexVersion{}).Where("id = ? AND state = 'building'", id).Updates(map[string]any{"state": "active", "activated_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrIndexVersionConflict
		}
		return nil
	})
}

// RollbackVersion explicitly promotes a previously retired generation.  This
// is the operator rollback path; normal activation still only accepts a
// building generation and therefore cannot accidentally resurrect old data.
func (p *PostgresIndex) RollbackVersion(ctx context.Context, backend, projectionType, targetVersion string) error {
	if p == nil || p.db == nil || backend == "" || projectionType == "" || targetVersion == "" {
		return ErrInvalidIndexVersion
	}
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var next IndexVersion
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("backend = ? AND projection_type = ? AND target_version = ?", backend, projectionType, targetVersion).First(&next).Error; err != nil {
			return err
		}
		if next.State != "retired" && next.State != "active" {
			return ErrIndexVersionConflict
		}
		now := time.Now().UTC()
		if err := tx.Model(&IndexVersion{}).Where("backend = ? AND projection_type = ? AND state = 'active' AND id <> ?", backend, projectionType, next.ID).Updates(map[string]any{"state": "retired", "updated_at": now}).Error; err != nil {
			return err
		}
		if next.State == "retired" {
			result := tx.Model(&IndexVersion{}).Where("id = ? AND state = 'retired'", next.ID).Updates(map[string]any{"state": "active", "activated_at": now, "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrIndexVersionConflict
			}
		}
		return nil
	})
}

func allowedIndexVersionTransition(from, to string) bool {
	switch from {
	case "building":
		return to == "active" || to == "failed"
	case "active":
		return to == "retired"
	case "failed":
		return to == "retired"
	default:
		return false
	}
}

func (p *PostgresIndex) ActiveVersion(ctx context.Context, backend, projectionType string) (IndexVersion, error) {
	if p == nil || p.db == nil || backend == "" || projectionType == "" {
		return IndexVersion{}, ErrInvalidIndexVersion
	}
	var row IndexVersion
	err := p.db.WithContext(ctx).Where("backend = ? AND projection_type = ? AND state = 'active'", backend, projectionType).First(&row).Error
	return row, err
}
