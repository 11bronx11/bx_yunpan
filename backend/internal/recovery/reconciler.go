// Package recovery rebuilds short-lived execution work from PostgreSQL
// authority after an Asynq/Redis loss. It never writes Redis directly: every
// repair is a deterministic Outbox event and therefore remains auditable.
package recovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

type Report struct {
	Scanned  int
	Repaired int
	ByKind   map[string]int
}

type Reconciler struct {
	db                  *gorm.DB
	aiEnabled           bool
	staleAfter          time.Duration
	operationStaleAfter time.Duration
	now                 func() time.Time
}

func New(db *gorm.DB, aiEnabled bool) *Reconciler {
	return &Reconciler{
		db: db, aiEnabled: aiEnabled,
		staleAfter: 15 * time.Minute, operationStaleAfter: 5 * time.Minute,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithStaleAfter makes time-based recovery deterministic in integration tests.
func (r *Reconciler) WithStaleAfter(value time.Duration) *Reconciler {
	if value > 0 {
		r.staleAfter = value
	}
	return r
}

func (r *Reconciler) WithOperationStaleAfter(value time.Duration) *Reconciler {
	if value > 0 {
		r.operationStaleAfter = value
	}
	return r
}

func (r *Reconciler) WithClock(now func() time.Time) *Reconciler {
	if now != nil {
		r.now = now
	}
	return r
}

// RunOnce scans all persistent intermediate states. limit is a global bound;
// each class is checked in a fixed order so repeated scans are fair and
// deterministic.
func (r *Reconciler) RunOnce(ctx context.Context, limit int) (Report, error) {
	if r == nil || r.db == nil || limit <= 0 {
		return Report{}, fmt.Errorf("recovery: invalid reconciler or limit")
	}
	report := Report{ByKind: map[string]int{}}
	remaining := limit
	for _, scan := range []func(context.Context, int, *Report) error{
		r.recoverVerifyingUploads,
		r.recoverMedia,
		r.recoverAI,
		r.recoverReprocessTasks,
		r.recoverGarbageCollection,
		r.recoverStorageOperations,
	} {
		if remaining <= 0 {
			break
		}
		before := report.Repaired
		if err := scan(ctx, remaining, &report); err != nil {
			return report, err
		}
		remaining -= report.Repaired - before
	}
	return report, nil
}

type uploadRow struct {
	ID      uuid.UUID
	Version int64
}

func (r *Reconciler) recoverVerifyingUploads(ctx context.Context, limit int, report *Report) error {
	var rows []uploadRow
	if err := r.db.WithContext(ctx).Table("upload_sessions").Select("id, version").
		Where("status = ?", "verifying").Order("updated_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		key := "upload.verify:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "broker_task_missing", "upload_session", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectVerifyRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: row.ID}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["object.verify"]++
		}
	}
	return nil
}

func (r *Reconciler) recoverMedia(ctx context.Context, limit int, report *Report) error {
	var rows []uploadRow
	if err := r.db.WithContext(ctx).Raw(`
		SELECT o.id, o.version
		FROM file_objects o
		WHERE o.status = 'ready' AND o.mime_type LIKE 'image/%'
		  AND NOT EXISTS (
			SELECT 1 FROM object_variants v
			WHERE v.object_id = o.id AND v.variant_type = 'thumbnail'
			  AND v.pipeline_version = 'thumbnail-v1' AND v.status = 'ready'
		  )
		ORDER BY o.updated_at ASC LIMIT ?`, limit).Scan(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		key := "media.thumbnail:" + row.ID.String() + ":thumbnail-v1"
		created, err := r.ensure(ctx, key, "missing_media_variant", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventMediaThumbnailRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.MediaThumbnailRequestedV1{ObjectID: row.ID}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["media.thumbnail"]++
		}
	}
	return nil
}

func (r *Reconciler) recoverAI(ctx context.Context, limit int, report *Report) error {
	if !r.aiEnabled {
		return nil
	}
	cutoff := r.now().Add(-r.staleAfter)
	var rows []uploadRow
	if err := r.db.WithContext(ctx).Raw(`
		SELECT o.id, o.version
		FROM file_objects o
		LEFT JOIN ai_documents d ON d.object_id = o.id
		WHERE o.status = 'ready' AND (
			d.id IS NULL OR (d.status IN ('pending', 'processing', 'building', 'failed') AND d.updated_at <= ?)
		)
		ORDER BY o.updated_at ASC LIMIT ?`, cutoff, limit).Scan(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		key := "ai.index:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "ai_index_incomplete", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventAIIndexRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.AIIndexRequestedV1{ObjectID: row.ID}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["ai.index"]++
		}
	}
	return nil
}

type taskRow struct {
	ID       uuid.UUID
	Resource uuid.UUID `gorm:"column:resource_id"`
	Version  int64
}

func (r *Reconciler) recoverReprocessTasks(ctx context.Context, limit int, report *Report) error {
	if !r.aiEnabled {
		return nil
	}
	cutoff := r.now().Add(-r.staleAfter)
	var rows []taskRow
	if err := r.db.WithContext(ctx).Table("async_tasks").Select("id, resource_id, version").
		Where("task_type = ? AND resource_id IS NOT NULL AND status IN ? AND updated_at <= ?", "ai.reprocess", []string{"pending", "running", "failed"}, cutoff).
		Order("updated_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		key := "ai.reprocess:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "ai_task_incomplete", "ai_task", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventAIReprocessRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.AIReprocessRequestedV1{TaskID: row.ID, ObjectID: row.Resource}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["ai.reprocess"]++
		}
	}
	return nil
}

func (r *Reconciler) recoverGarbageCollection(ctx context.Context, limit int, report *Report) error {
	var rows []uploadRow
	if err := r.db.WithContext(ctx).Raw(`
		SELECT o.id, o.version
		FROM file_objects o
		WHERE o.status IN ('ready', 'deleting') AND o.reference_count = 0
		  AND NOT EXISTS (
			SELECT 1 FROM storage_operations so
			WHERE so.object_id = o.id AND so.operation_type = 'object.delete'
			  AND (so.state IN ('pending', 'running') OR (so.state = 'failed' AND so.available_at > ?))
		  )
		ORDER BY o.updated_at ASC LIMIT ?`, r.now(), limit).Scan(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		key := "object.gc:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "zero_reference_gc", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: row.ID}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["object.gc"]++
		}
	}
	return nil
}

type operationRow struct {
	ID            uuid.UUID
	OperationType string
	ObjectID      *uuid.UUID
	UploadID      *uuid.UUID `gorm:"column:upload_session_id"`
	Version       int64
}

func (r *Reconciler) recoverStorageOperations(ctx context.Context, limit int, report *Report) error {
	now := r.now()
	operationCutoff := now.Add(-r.operationStaleAfter)
	var rows []operationRow
	if err := r.db.WithContext(ctx).Table("storage_operations").Select("id, operation_type, object_id, upload_session_id, attempt as version").
		Where(`
			(state IN ? AND available_at <= ?)
			OR (state = ? AND (
				(lease_until IS NOT NULL AND lease_until <= ?)
				OR (lease_until IS NULL AND updated_at <= ?)
			))`,
			[]string{storageops.StatePending, storageops.StateFailed}, now,
			storageops.StateRunning, now, operationCutoff).
		Order("available_at ASC, created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	report.Scanned += len(rows)
	for _, row := range rows {
		resourceID := row.ID
		resourceType := "storage_operation"
		if row.ObjectID != nil {
			resourceID = *row.ObjectID
			resourceType = "file_object"
		} else if row.UploadID != nil {
			resourceID = *row.UploadID
			resourceType = "upload_session"
		}
		key := "storage.operation:" + row.ID.String() + ":" + strings.ReplaceAll(row.OperationType, "/", "_")
		created, err := r.ensure(ctx, key, "storage_operation_unfinished", resourceType, resourceID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventStorageOperationRecoverV1, AggregateID: row.ID, AggregateVersion: maxVersion(row.Version), Producer: "maintenance-recovery", Payload: eventcontract.StorageOperationRecoverV1{OperationID: row.ID, OperationType: row.OperationType, RecoveryKey: key}})
		if err != nil {
			return err
		}
		if created {
			report.Repaired++
			report.ByKind["storage.operation"]++
		}
	}
	return nil
}

func maxVersion(value int64) int64 {
	if value <= 0 {
		return 1
	}
	return value
}

func (r *Reconciler) ensure(ctx context.Context, key, reason, resourceType string, resourceID uuid.UUID, version int64, input eventcontract.EnvelopeInput) (bool, error) {
	var created bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		active, err := outbox.HasActiveDelivery(tx, input.EventType, input.AggregateID)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		created, err = outbox.EnsureRecovery(tx, key, reason, resourceType, resourceID, maxVersion(version), input)
		return err
	})
	return created, err
}
