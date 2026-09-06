package search

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
)

const ConsumerName = "search-indexer"

type Projector struct {
	db         *gorm.DB
	index      *PostgresIndex
	target     string
	indexEpoch int64
}

func NewProjector(db *gorm.DB, targetVersion string) *Projector {
	return NewProjectorWithEpoch(db, targetVersion, 1)
}

func NewProjectorWithEpoch(db *gorm.DB, targetVersion string, indexEpoch int64) *Projector {
	if targetVersion == "" {
		targetVersion = "v1"
	}
	if indexEpoch <= 0 {
		if parsed, err := strconv.ParseInt(targetVersion, 10, 64); err == nil && parsed > 0 {
			indexEpoch = parsed
		} else {
			indexEpoch = 1
		}
	}
	return &Projector{db: db, index: NewPostgresIndex(db), target: targetVersion, indexEpoch: indexEpoch}
}

// Handler consumes the versioned search task. The task metadata is injected by
// the Outbox relay, so an older producer can still be projected during rollout.
func (p *Projector) Handler(ctx context.Context, task *asynq.Task) error {
	if p == nil || p.db == nil || task == nil {
		return asynq.SkipRetry
	}
	var envelope struct {
		EventID          uuid.UUID       `json:"event_id"`
		EventType        string          `json:"event_type"`
		AggregateVersion *int64          `json:"aggregate_version"`
		Payload          json.RawMessage `json:"-"`
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(task.Payload(), &fields); err != nil {
		return asynq.SkipRetry
	}
	if err := json.Unmarshal(fields["event_id"], &envelope.EventID); err != nil || envelope.EventID == uuid.Nil {
		return asynq.SkipRetry
	}
	if err := json.Unmarshal(fields["event_type"], &envelope.EventType); err != nil || envelope.EventType == "" {
		return asynq.SkipRetry
	}
	if raw, ok := fields["aggregate_version"]; ok {
		_ = json.Unmarshal(raw, &envelope.AggregateVersion)
	}
	delete(fields, "event_id")
	delete(fields, "task_id")
	delete(fields, "event_type")
	delete(fields, "event_version")
	delete(fields, outbox.TracePayloadKey)
	delete(fields, "aggregate_version")
	envelope.Payload, _ = json.Marshal(fields)

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := outbox.ConsumeOnce(tx, envelope.EventID, ConsumerName, func(tx *gorm.DB) error {
			return p.project(tx, envelope.EventID, envelope.EventType, envelope.AggregateVersion, envelope.Payload)
		})
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

func (p *Projector) project(tx *gorm.DB, _ uuid.UUID, eventType string, aggregateVersion *int64, payload []byte) error {
	switch eventType {
	case eventcontract.EventFileCreatedV1, eventcontract.EventFileUpdatedV1, eventcontract.EventShareImportedV1:
		var value struct {
			OwnerID     uuid.UUID `json:"owner_id"`
			FileEntryID uuid.UUID `json:"file_entry_id"`
		}
		if err := json.Unmarshal(payload, &value); err != nil || value.FileEntryID == uuid.Nil || value.OwnerID == uuid.Nil {
			return errors.New("search: invalid file event")
		}
		return p.projectFile(tx, value.FileEntryID, false)
	case eventcontract.EventFileDeletedV1:
		var value eventcontract.FileDeletedV1
		if err := json.Unmarshal(payload, &value); err != nil || value.FileEntryID == uuid.Nil {
			return errors.New("search: invalid file delete event")
		}
		version := int64(1 << 62)
		if aggregateVersion != nil && *aggregateVersion > 0 {
			version = *aggregateVersion
		}
		return p.index.tombstoneFile(tx, BackendPostgres, value.FileEntryID, version)
	case eventcontract.EventShareCreatedV1:
		var value eventcontract.ShareCreatedV1
		if err := json.Unmarshal(payload, &value); err != nil || value.FileEntryID == uuid.Nil || value.OwnerID == uuid.Nil {
			return errors.New("search: invalid share create event")
		}
		return p.projectFileWithACL(tx, value.FileEntryID, false, value.ACLEpoch)
	case eventcontract.EventShareRevokedV1:
		var value eventcontract.ShareRevokedV1
		if err := json.Unmarshal(payload, &value); err != nil || value.ShareID == uuid.Nil || value.OwnerID == uuid.Nil {
			return errors.New("search: invalid share revoke event")
		}
		var share struct{ FileEntryID uuid.UUID }
		if err := tx.Table("shares").Select("file_entry_id").Where("id = ?", value.ShareID).Scan(&share).Error; err != nil {
			return err
		}
		if share.FileEntryID == uuid.Nil {
			return nil
		}
		return p.projectFileWithACL(tx, share.FileEntryID, false, value.ACLEpoch)
	case eventcontract.EventFolderCreatedV1, eventcontract.EventFolderRenamedV1, eventcontract.EventFolderMovedV1, eventcontract.EventFolderDeletedV1:
		var owner struct {
			OwnerID uuid.UUID `json:"owner_id"`
		}
		if err := json.Unmarshal(payload, &owner); err != nil || owner.OwnerID == uuid.Nil {
			return errors.New("search: invalid folder event")
		}
		return p.projectOwner(tx, owner.OwnerID)
	case eventcontract.EventObjectReadyV1, eventcontract.EventAIIndexRequestedV1:
		var value struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(payload, &value); err != nil || value.ObjectID == uuid.Nil {
			return errors.New("search: invalid object event")
		}
		return p.projectObject(tx, value.ObjectID)
	case eventcontract.EventObjectGCRequestedV1:
		var value eventcontract.ObjectGCRequestedV1
		if err := json.Unmarshal(payload, &value); err != nil || value.ObjectID == uuid.Nil {
			return errors.New("search: invalid object gc event")
		}
		return p.index.tombstoneObjectContent(tx, BackendPostgres, value.ObjectID, 1<<62)
	default:
		return nil
	}
}

type fileSource struct {
	FileEntryID uuid.UUID
	OwnerID     uuid.UUID
	FolderID    uuid.UUID
	ObjectID    uuid.UUID
	FileVersion int64
	Name        string
	MIMEType    string
	DeletedAt   *time.Time
}

func (p *Projector) projectFile(tx *gorm.DB, fileID uuid.UUID, tombstone bool) error {
	return p.projectFileWithACL(tx, fileID, tombstone, 0)
}

func (p *Projector) projectFileWithACL(tx *gorm.DB, fileID uuid.UUID, tombstone bool, aclVersion int64) error {
	var source fileSource
	err := tx.Table("file_entries e").Select("e.id AS file_entry_id, e.owner_id, e.folder_id, e.object_id, e.version AS file_version, e.name, o.mime_type, e.deleted_at").Joins("JOIN file_objects o ON o.id = e.object_id").Where("e.id = ?", fileID).Scan(&source).Error
	if err != nil {
		return err
	}
	if source.FileEntryID == uuid.Nil {
		return p.index.tombstoneFile(tx, BackendPostgres, fileID, 1<<62)
	}
	if aclVersion <= 0 {
		epoch, err := ownerepoch.Current(tx, source.OwnerID)
		if err != nil {
			return err
		}
		aclVersion = epoch.ACLEpoch
	}
	return p.index.upsertFile(tx, FileProjection{Backend: BackendPostgres, TargetVersion: p.target, FileEntryID: source.FileEntryID, OwnerID: source.OwnerID, FolderID: source.FolderID, ObjectID: source.ObjectID, FileVersion: source.FileVersion, ACLVersion: aclVersion, Name: source.Name, MIMEType: source.MIMEType, IndexEpoch: p.indexEpoch, Tombstone: tombstone || source.DeletedAt != nil})
}

func (p *Projector) projectOwner(tx *gorm.DB, ownerID uuid.UUID) error {
	var files []fileSource
	epoch, err := ownerepoch.Current(tx, ownerID)
	if err != nil {
		return err
	}
	if err := tx.Table("file_entries e").Select("e.id AS file_entry_id, e.owner_id, e.folder_id, e.object_id, e.version AS file_version, e.name, o.mime_type, e.deleted_at").Joins("JOIN file_objects o ON o.id = e.object_id").Where("e.owner_id = ?", ownerID).Find(&files).Error; err != nil {
		return err
	}
	for _, file := range files {
		if err := p.index.upsertFile(tx, FileProjection{Backend: BackendPostgres, TargetVersion: p.target, FileEntryID: file.FileEntryID, OwnerID: file.OwnerID, FolderID: file.FolderID, ObjectID: file.ObjectID, FileVersion: file.FileVersion, ACLVersion: epoch.ACLEpoch, Name: file.Name, MIMEType: file.MIMEType, IndexEpoch: p.indexEpoch, Tombstone: file.DeletedAt != nil}); err != nil {
			return err
		}
	}
	return nil
}

type chunkSource struct {
	ID                uuid.UUID
	DocumentVersionID uuid.UUID
	ObjectID          uuid.UUID
	Ordinal           int
	PipelineVersion   string
	ModelVersion      string
	Content           string
}

func (p *Projector) projectObject(tx *gorm.DB, objectID uuid.UUID) error {
	var chunks []chunkSource
	err := tx.Table("ai_chunks c").Select("c.id, c.document_version_id, d.object_id, c.ordinal, v.pipeline_version, v.model_version, c.content").Joins("JOIN ai_documents d ON d.id = c.document_id").Joins("JOIN ai_document_versions v ON v.id = c.document_version_id AND v.status = 'indexed'").Where("d.object_id = ? AND d.active_version_id = v.id", objectID).Order("c.ordinal ASC").Find(&chunks).Error
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := p.index.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: p.target, ObjectID: chunk.ObjectID, DocumentVersionID: chunk.DocumentVersionID, ChunkID: chunk.ID, Ordinal: chunk.Ordinal, PipelineVersion: chunk.PipelineVersion, ModelVersion: chunk.ModelVersion, IndexEpoch: p.indexEpoch, Content: chunk.Content}); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot is intentionally resumable at the file/object level. The caller
// records the last UUID checkpoint; rows are always rebuilt from PostgreSQL.
func (p *Projector) Snapshot(ctx context.Context, after uuid.UUID, batchSize int) (uuid.UUID, int, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	var files []fileSource
	query := p.db.WithContext(ctx).Table("file_entries e").Select("e.id AS file_entry_id, e.owner_id, e.folder_id, e.object_id, e.version AS file_version, e.name, o.mime_type, e.deleted_at").Joins("JOIN file_objects o ON o.id = e.object_id")
	if after != uuid.Nil {
		query = query.Where("e.id > ?", after)
	}
	if err := query.Order("e.id ASC").Limit(batchSize).Find(&files).Error; err != nil {
		return after, 0, err
	}
	for _, file := range files {
		epoch, err := ownerepoch.Current(p.db, file.OwnerID)
		if err != nil {
			return after, 0, err
		}
		if err := p.index.UpsertFile(ctx, FileProjection{Backend: BackendPostgres, TargetVersion: p.target, FileEntryID: file.FileEntryID, OwnerID: file.OwnerID, FolderID: file.FolderID, ObjectID: file.ObjectID, FileVersion: file.FileVersion, ACLVersion: epoch.ACLEpoch, Name: file.Name, MIMEType: file.MIMEType, IndexEpoch: p.indexEpoch, Tombstone: file.DeletedAt != nil}); err != nil {
			return after, 0, err
		}
	}
	if len(files) == 0 {
		return after, 0, nil
	}
	return files[len(files)-1].FileEntryID, len(files), nil
}

// SnapshotContent rebuilds only active AI document chunks. It is separate from
// Snapshot because file metadata and object content have different checkpoints
// and failure/retry characteristics.
func (p *Projector) SnapshotContent(ctx context.Context, after uuid.UUID, batchSize int) (uuid.UUID, int, error) {
	if p == nil || p.db == nil {
		return after, 0, errors.New("search: nil projector")
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	var chunks []chunkSource
	query := p.db.WithContext(ctx).Table("ai_chunks c").Select("c.id, c.document_version_id, d.object_id, c.ordinal, v.pipeline_version, v.model_version, c.content").Joins("JOIN ai_documents d ON d.id = c.document_id").Joins("JOIN ai_document_versions v ON v.id = c.document_version_id AND v.status = 'indexed'").Where("d.active_version_id = v.id")
	if after != uuid.Nil {
		query = query.Where("c.id > ?", after)
	}
	if err := query.Order("c.id ASC").Limit(batchSize).Find(&chunks).Error; err != nil {
		return after, 0, err
	}
	for _, chunk := range chunks {
		if err := p.index.UpsertContent(ctx, ContentProjection{Backend: BackendPostgres, TargetVersion: p.target, ObjectID: chunk.ObjectID, DocumentVersionID: chunk.DocumentVersionID, ChunkID: chunk.ID, Ordinal: chunk.Ordinal, PipelineVersion: chunk.PipelineVersion, ModelVersion: chunk.ModelVersion, IndexEpoch: p.indexEpoch, Content: chunk.Content}); err != nil {
			return after, 0, err
		}
	}
	if len(chunks) == 0 {
		return after, 0, nil
	}
	return chunks[len(chunks)-1].ID, len(chunks), nil
}
