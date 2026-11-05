package search

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
)

const ConsumerName = "search-indexer"

type Projector struct {
	db         *gorm.DB
	index      *PostgresIndex
	external   Index
	target     string
	indexEpoch int64
}

type ProjectionCheckpoint struct {
	EventID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	ConsumerName string    `gorm:"primaryKey"`
	State        string
	Attempt      int
	LastError    *string
	Watermark    *int64
	Checksum     *string
	UpdatedAt    time.Time
}

func (ProjectionCheckpoint) TableName() string { return "search_projection_checkpoints" }

// WithExternalIndex enables the two-phase projection path. PostgreSQL rows
// are committed first, external upserts happen without a SQL transaction, and
// the event-consumption marker is written only after all external effects
// succeed. A nil index preserves the legacy atomic local-only path.
func (p *Projector) WithExternalIndex(index Index) *Projector {
	p.external = index
	return p
}

func NewProjector(db *gorm.DB, targetVersion string) *Projector {
	return NewProjectorWithEpoch(db, targetVersion, 0)
}

func NewProjectorWithEpoch(db *gorm.DB, targetVersion string, indexEpoch int64) *Projector {
	if targetVersion == "" {
		targetVersion = "v1"
	}
	if indexEpoch <= 0 {
		if parsed, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(targetVersion), "v"), 10, 64); err == nil && parsed > 0 {
			indexEpoch = parsed
		} else {
			indexEpoch = 1
		}
	}
	return &Projector{db: db, index: NewPostgresIndex(db), target: targetVersion, indexEpoch: indexEpoch}
}

// Handler consumes the versioned search task. The task metadata is injected by
// the Outbox relay, so an older producer can still be projected during rollout.
func (p *Projector) Handler(ctx context.Context, task *taskqueue.Task) error {
	if p == nil || p.db == nil || task == nil {
		return taskqueue.SkipRetry
	}
	// Every queue consumer writes every live generation. Competing consumers
	// configured for different targets would otherwise split events between
	// generations instead of keeping both up to date during a rebuild.
	var versions []IndexVersion
	if err := p.db.WithContext(ctx).Where("backend = ? AND state IN ?", BackendPostgres, []string{"building", "active"}).
		Order("target_version ASC").Find(&versions).Error; err != nil {
		return err
	}
	targets := make(map[string]int64)
	for _, version := range versions {
		if epoch, found := targets[version.TargetVersion]; found && epoch != version.IndexEpoch {
			return ErrIndexVersionConflict
		}
		targets[version.TargetVersion] = version.IndexEpoch
	}
	if _, found := targets[p.target]; !found {
		targets[p.target] = p.indexEpoch
	}
	names := make([]string, 0, len(targets))
	for target := range targets {
		names = append(names, target)
	}
	sort.Strings(names)
	var failures error
	for _, target := range names {
		worker := *p
		worker.target, worker.indexEpoch = target, targets[target]
		if err := worker.handleTarget(ctx, task); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// Receipts belong to one generation and writer set. A PostgreSQL-only ACK
// must never suppress a later ES/Qdrant publish of the same event.
func (p *Projector) consumerName() string {
	backends := []string{BackendPostgres}
	if p.external != nil {
		if provider, ok := p.external.(interface{ Backends() []string }); ok {
			backends = append(backends, provider.Backends()...)
		} else {
			backends = append(backends, "external")
		}
	}
	sort.Strings(backends)
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", p.target, p.indexEpoch, strings.Join(backends, ","))))
	return fmt.Sprintf("%s:%x", ConsumerName, digest[:])
}

func (p *Projector) handleTarget(ctx context.Context, task *taskqueue.Task) error {
	var envelope struct {
		EventID          uuid.UUID       `json:"event_id"`
		EventType        string          `json:"event_type"`
		AggregateVersion *int64          `json:"aggregate_version"`
		Payload          json.RawMessage `json:"-"`
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(task.Payload(), &fields); err != nil {
		return taskqueue.SkipRetry
	}
	if err := json.Unmarshal(fields["event_id"], &envelope.EventID); err != nil || envelope.EventID == uuid.Nil {
		return taskqueue.SkipRetry
	}
	if err := json.Unmarshal(fields["event_type"], &envelope.EventType); err != nil || envelope.EventType == "" {
		return taskqueue.SkipRetry
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

	if p.external == nil {
		return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			_, err := outbox.ConsumeOnce(tx, envelope.EventID, p.consumerName(), func(tx *gorm.DB) error {
				return p.project(tx, envelope.EventID, envelope.EventType, envelope.AggregateVersion, envelope.Payload)
			})
			return err
		})
	}
	// A completed marker is an ACK. Do not redo network work for a duplicate
	// delivery, but tolerate the crash window before the marker transaction.
	var marker outbox.Consumption
	if err := p.db.WithContext(ctx).Where("event_id = ? AND consumer_name = ?", envelope.EventID, p.consumerName()).First(&marker).Error; err == nil {
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	// If the external publish and checkpoint commit completed but the final
	// marker transaction was interrupted, only the marker needs replaying.
	if checkpoint, checkpointErr := p.Checkpoint(ctx, envelope.EventID); checkpointErr == nil && (checkpoint.State == "indexed" || checkpoint.State == "tombstoned") {
		return p.ack(ctx, envelope.EventID)
	} else if checkpointErr != nil && !errors.Is(checkpointErr, gorm.ErrRecordNotFound) {
		return checkpointErr
	}
	checksum := sha256.Sum256(envelope.Payload)
	checksumText := fmt.Sprintf("sha256:%x", checksum[:])
	var watermark *int64
	if envelope.AggregateVersion != nil && *envelope.AggregateVersion > 0 {
		value := *envelope.AggregateVersion
		watermark = &value
	}
	if err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := p.project(tx, envelope.EventID, envelope.EventType, envelope.AggregateVersion, envelope.Payload); err != nil {
			return err
		}
		return p.beginCheckpoint(tx, envelope.EventID, watermark, &checksumText)
	}); err != nil {
		return err
	}
	if err := p.setCheckpoint(ctx, envelope.EventID, "publishing", nil); err != nil {
		return err
	}
	if err := p.publishExternal(ctx, envelope.EventType, envelope.AggregateVersion, envelope.Payload); err != nil {
		_ = p.setCheckpoint(ctx, envelope.EventID, "failed", stringPtr(limitError(err)))
		if errors.Is(err, ErrPermanent) {
			return taskqueue.SkipRetry
		}
		return err
	}
	state := "indexed"
	if eventTypeIsTombstone(envelope.EventType) {
		state = "tombstoned"
	}
	if err := p.setCheckpoint(ctx, envelope.EventID, state, nil); err != nil {
		return err
	}
	return p.ack(ctx, envelope.EventID)
}

func (p *Projector) ack(ctx context.Context, eventID uuid.UUID) error {
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := outbox.ConsumeOnce(tx, eventID, p.consumerName(), nil)
		return err
	})
}

func (p *Projector) beginCheckpoint(tx *gorm.DB, eventID uuid.UUID, watermark *int64, checksum *string) error {
	if tx == nil || eventID == uuid.Nil {
		return errors.New("search: invalid projection checkpoint")
	}
	now := time.Now().UTC()
	row := ProjectionCheckpoint{EventID: eventID, ConsumerName: p.consumerName(), State: "pending", Attempt: 1, Watermark: watermark, Checksum: checksum, UpdatedAt: now}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}, {Name: "consumer_name"}}, DoUpdates: clause.Assignments(map[string]any{"state": "pending", "attempt": gorm.Expr("search_projection_checkpoints.attempt + 1"), "last_error": nil, "watermark": watermark, "checksum": checksum, "updated_at": now})}).Create(&row).Error
}

func (p *Projector) setCheckpoint(ctx context.Context, eventID uuid.UUID, state string, lastError *string) error {
	if p == nil || p.db == nil || eventID == uuid.Nil {
		return errors.New("search: invalid projection checkpoint")
	}
	result := p.db.WithContext(ctx).Model(&ProjectionCheckpoint{}).Where("event_id = ? AND consumer_name = ?", eventID, p.consumerName()).Updates(map[string]any{"state": state, "last_error": lastError, "updated_at": time.Now().UTC()})
	return result.Error
}

func (p *Projector) Checkpoint(ctx context.Context, eventID uuid.UUID) (ProjectionCheckpoint, error) {
	if p == nil || p.db == nil || eventID == uuid.Nil {
		return ProjectionCheckpoint{}, errors.New("search: invalid projection checkpoint")
	}
	var row ProjectionCheckpoint
	err := p.db.WithContext(ctx).Where("event_id = ? AND consumer_name = ?", eventID, p.consumerName()).First(&row).Error
	return row, err
}

func limitError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func stringPtr(value string) *string { return &value }

func eventTypeIsTombstone(eventType string) bool {
	return eventType == eventcontract.EventFileDeletedV1 || eventType == eventcontract.EventObjectGCRequestedV1
}

func (p *Projector) publishExternal(ctx context.Context, eventType string, aggregateVersion *int64, payload []byte) error {
	if p.external == nil {
		return nil
	}
	fileIDs := make([]uuid.UUID, 0, 1)
	objectIDs := make([]uuid.UUID, 0, 1)
	ownerIDs := make([]uuid.UUID, 0, 1)
	var value struct {
		OwnerID     uuid.UUID `json:"owner_id"`
		FileEntryID uuid.UUID `json:"file_entry_id"`
		ObjectID    uuid.UUID `json:"object_id"`
		ShareID     uuid.UUID `json:"share_id"`
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	switch eventType {
	case eventcontract.EventFileCreatedV1, eventcontract.EventFileUpdatedV1, eventcontract.EventShareImportedV1, eventcontract.EventShareCreatedV1:
		fileIDs = append(fileIDs, value.FileEntryID)
	case eventcontract.EventFileDeletedV1:
		fileIDs = append(fileIDs, value.FileEntryID)
	case eventcontract.EventShareRevokedV1:
		var share struct{ FileEntryID uuid.UUID }
		if err := p.db.WithContext(ctx).Table("shares").Select("file_entry_id").Where("id = ?", value.ShareID).Scan(&share).Error; err != nil {
			return err
		}
		fileIDs = append(fileIDs, share.FileEntryID)
	case eventcontract.EventFolderCreatedV1, eventcontract.EventFolderRenamedV1, eventcontract.EventFolderMovedV1, eventcontract.EventFolderDeletedV1:
		ownerIDs = append(ownerIDs, value.OwnerID)
	case eventcontract.EventObjectReadyV1, eventcontract.EventAIIndexRequestedV1, eventcontract.EventAIIndexedV1:
		objectIDs = append(objectIDs, value.ObjectID)
	case eventcontract.EventObjectGCRequestedV1:
		objectIDs = append(objectIDs, value.ObjectID)
	default:
		return nil
	}
	for _, fileID := range fileIDs {
		var row fileRow
		err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ? AND file_entry_id = ?", BackendPostgres, p.target, fileID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if eventType == eventcontract.EventFileDeletedV1 {
				version := int64(1 << 62)
				if aggregateVersion != nil && *aggregateVersion > 0 {
					version = *aggregateVersion
				}
				if err := p.external.TombstoneFile(ctx, p.target, fileID, version); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		value := FileProjection{Backend: row.Backend, TargetVersion: row.TargetVersion, FileEntryID: row.FileEntryID, OwnerID: row.OwnerID, FolderID: row.FolderID, ObjectID: row.ObjectID, FileVersion: row.FileVersion, ACLVersion: row.ACLVersion, Name: row.Name, MIMEType: row.MIMEType, IndexEpoch: row.IndexEpoch, Tombstone: row.Tombstone || row.State == "tombstoned"}
		// Tombstone/state and file_version come from the canonical PG row. This
		// prevents a late delete event from masking a newer file update.
		if err := p.external.UpsertFile(ctx, value); err != nil {
			return err
		}
	}
	for _, ownerID := range ownerIDs {
		var rows []fileRow
		if err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ? AND owner_id = ?", BackendPostgres, p.target, ownerID).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if err := p.external.UpsertFile(ctx, FileProjection{Backend: row.Backend, TargetVersion: row.TargetVersion, FileEntryID: row.FileEntryID, OwnerID: row.OwnerID, FolderID: row.FolderID, ObjectID: row.ObjectID, FileVersion: row.FileVersion, ACLVersion: row.ACLVersion, Name: row.Name, MIMEType: row.MIMEType, IndexEpoch: row.IndexEpoch, Tombstone: row.Tombstone || row.State == "tombstoned"}); err != nil {
				return err
			}
		}
	}
	for _, objectID := range objectIDs {
		var rows []contentRow
		if err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ? AND object_id = ?", BackendPostgres, p.target, objectID).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			// GC can be cancelled or followed by same-hash revival. Retire the
			// recorded chunks, never every future generation of the object ID.
			value := ContentProjection{Backend: row.Backend, TargetVersion: row.TargetVersion, ObjectID: row.ObjectID, DocumentVersionID: row.DocumentVersionID, ChunkID: row.ChunkID, Ordinal: row.Ordinal, PipelineVersion: row.PipelineVersion, ModelVersion: row.ModelVersion, IndexEpoch: row.IndexEpoch, Content: row.Content, VectorRef: row.VectorRef, Tombstone: row.State == "tombstoned"}
			if err := p.external.UpsertContent(ctx, value); err != nil {
				return err
			}
		}
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
	case eventcontract.EventObjectReadyV1, eventcontract.EventAIIndexRequestedV1, eventcontract.EventAIIndexedV1:
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
		return p.projectObject(tx, value.ObjectID)
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
	VectorRef         string
}

func (p *Projector) projectObject(tx *gorm.DB, objectID uuid.UUID) error {
	return p.projectObjectChunks(tx, objectID, nil)
}

// RepairObject re-reads one object's facts and republishes its complete
// projection, including retired chunks, to the explicitly selected target.
// It is safe to retry after a partial external publish.
func (p *Projector) RepairObject(ctx context.Context, objectID uuid.UUID) error {
	if p == nil || p.db == nil || objectID == uuid.Nil {
		return ErrInvalidIndexVersion
	}
	if err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return p.projectObject(tx, objectID)
	}); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"object_id": objectID})
	return p.publishExternal(ctx, eventcontract.EventAIIndexedV1, nil, payload)
}

func (p *Projector) projectObjectChunks(tx *gorm.DB, objectID uuid.UUID, chunkIDs []uuid.UUID) error {
	var object struct {
		ID     uuid.UUID
		Status string
	}
	err := tx.Table("file_objects").Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", objectID).Take(&object).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && object.Status != "ready") {
		return p.index.tombstoneObjectContent(tx, BackendPostgres, objectID, p.indexEpoch)
	}
	if err != nil {
		return err
	}
	var document struct {
		ID              uuid.UUID
		ActiveVersionID *uuid.UUID
	}
	err = tx.Table("ai_documents").Clauses(clause.Locking{Strength: "SHARE"}).Where("object_id = ?", objectID).Take(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if document.ActiveVersionID != nil {
		// Superseded chunks must leave the external candidate pool too. SQL
		// authorization alone would filter them only after they consumed recall.
		if err := tx.Model(&contentRow{}).Where("backend = ? AND target_version = ? AND object_id = ? AND document_version_id <> ? AND index_epoch <= ?", BackendPostgres, p.target, objectID, *document.ActiveVersionID, p.indexEpoch).
			Updates(map[string]any{"state": "tombstoned", "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
	}
	var chunks []chunkSource
	query := tx.Table("ai_chunks c").Select("c.id, c.document_version_id, d.object_id, c.ordinal, v.pipeline_version, v.model_version, c.content, c.embedding::text AS vector_ref").Joins("JOIN ai_documents d ON d.id = c.document_id").Joins("JOIN ai_document_versions v ON v.id = c.document_version_id AND v.status = 'indexed'").Where("d.object_id = ? AND d.active_version_id = v.id", objectID)
	if len(chunkIDs) > 0 {
		query = query.Where("c.id IN ?", chunkIDs)
	}
	err = query.Order("c.ordinal ASC").Find(&chunks).Error
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		vector := chunk.VectorRef
		if err := p.index.upsertContent(tx, ContentProjection{Backend: BackendPostgres, TargetVersion: p.target, ObjectID: chunk.ObjectID, DocumentVersionID: chunk.DocumentVersionID, ChunkID: chunk.ID, Ordinal: chunk.Ordinal, PipelineVersion: chunk.PipelineVersion, ModelVersion: chunk.ModelVersion, IndexEpoch: p.indexEpoch, Content: chunk.Content, VectorRef: &vector}); err != nil {
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
		if err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return p.projectFile(tx, file.FileEntryID, false)
		}); err != nil {
			return after, 0, err
		}
		payload, _ := json.Marshal(map[string]any{"file_entry_id": file.FileEntryID})
		if err := p.publishExternal(ctx, eventcontract.EventFileUpdatedV1, nil, payload); err != nil {
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
	query := p.db.WithContext(ctx).Table("ai_chunks c").Select("c.id, d.object_id").Joins("JOIN ai_documents d ON d.id = c.document_id").Joins("JOIN ai_document_versions v ON v.id = c.document_version_id AND v.status = 'indexed'").Where("d.active_version_id = v.id")
	if after != uuid.Nil {
		query = query.Where("c.id > ?", after)
	}
	if err := query.Order("c.id ASC").Limit(batchSize).Find(&chunks).Error; err != nil {
		return after, 0, err
	}
	groups := make(map[uuid.UUID][]uuid.UUID)
	for _, chunk := range chunks {
		groups[chunk.ObjectID] = append(groups[chunk.ObjectID], chunk.ID)
	}
	for objectID, ids := range groups {
		// Re-read under the same source locks as live projection. Publishing
		// the page's old raw values could resurrect a concurrent GC tombstone.
		if err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return p.projectObjectChunks(tx, objectID, ids)
		}); err != nil {
			return after, 0, err
		}
		if p.external != nil {
			var rows []contentRow
			if err := p.db.WithContext(ctx).Where("backend = ? AND target_version = ? AND chunk_id IN ?", BackendPostgres, p.target, ids).Find(&rows).Error; err != nil {
				return after, 0, err
			}
			for _, row := range rows {
				value := ContentProjection{Backend: row.Backend, TargetVersion: row.TargetVersion, ObjectID: row.ObjectID, DocumentVersionID: row.DocumentVersionID, ChunkID: row.ChunkID, Ordinal: row.Ordinal, PipelineVersion: row.PipelineVersion, ModelVersion: row.ModelVersion, IndexEpoch: row.IndexEpoch, Content: row.Content, VectorRef: row.VectorRef, Tombstone: row.State == "tombstoned"}
				if err := p.external.UpsertContent(ctx, value); err != nil {
					return after, 0, err
				}
			}
		}
	}
	if len(chunks) == 0 {
		return after, 0, nil
	}
	return chunks[len(chunks)-1].ID, len(chunks), nil
}
