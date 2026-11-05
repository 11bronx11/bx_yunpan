// Package recovery rebuilds short-lived execution work from PostgreSQL
// authority after missing task state. It never writes the broker directly: every
// repair is a deterministic Outbox event and therefore remains auditable.
package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
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
	scanMu              sync.Mutex
	nextScan            int
	cursors             map[string]uuid.UUID
}

// Each event still has its broker retry budget. Limit fresh identities too,
// including handlers that acknowledge work without advancing domain state.
const MaxRecoveryEvents = 3

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
// each class gets a bounded share and the first class rotates between calls.
func (r *Reconciler) RunOnce(ctx context.Context, limit int) (Report, error) {
	if r == nil || r.db == nil || limit <= 0 {
		return Report{}, fmt.Errorf("recovery: invalid reconciler or limit")
	}
	report := Report{ByKind: map[string]int{}}
	if err := storageops.StopExhausted(ctx, r.db, limit); err != nil {
		return report, err
	}
	scans := []func(context.Context, int, *Report) error{
		r.recoverVerifyingUploads,
		r.recoverMedia,
		r.recoverAI,
		r.recoverReprocessTasks,
		r.recoverGarbageCollection,
		r.recoverStorageOperations,
	}
	// Give every work class a bounded share and rotate the first class so a
	// large or repeatedly active upload backlog cannot starve later scans.
	r.scanMu.Lock()
	start := r.nextScan % len(scans)
	r.nextScan = (start + 1) % len(scans)
	r.scanMu.Unlock()
	base, extra := limit/len(scans), limit%len(scans)
	var scanErrors error
	for offset := range scans {
		slot := base
		if offset < extra {
			slot++
		}
		if slot == 0 {
			break
		}
		if err := scans[(start+offset)%len(scans)](ctx, slot, &report); err != nil {
			scanErrors = errors.Join(scanErrors, err)
		}
	}
	return report, scanErrors
}

type uploadRow struct {
	ID      uuid.UUID
	Version int64
}

func (row uploadRow) recoveryID() uuid.UUID { return row.ID }

// Scan past active, terminal and broken candidates as well as repaired ones.
// LIMIT on oldest rows alone can pin every scan to the same no-op backlog.
func scanPage[T interface{ recoveryID() uuid.UUID }](r *Reconciler, ctx context.Context, kind string, candidates *gorm.DB, limit int, rows *[]T) error {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	if r.cursors == nil {
		r.cursors = make(map[string]uuid.UUID)
	}
	query := func(after uuid.UUID) error {
		q := r.db.WithContext(ctx).Table("(?) AS candidates", candidates).Order("id ASC").Limit(limit)
		if after != uuid.Nil {
			q = q.Where("id > ?", after)
		}
		return q.Find(rows).Error
	}
	cursor := r.cursors[kind]
	if err := query(cursor); err != nil {
		return err
	}
	if len(*rows) == 0 && cursor != uuid.Nil {
		if err := query(uuid.Nil); err != nil {
			return err
		}
	}
	if len(*rows) > 0 {
		r.cursors[kind] = (*rows)[len(*rows)-1].recoveryID()
	} else {
		delete(r.cursors, kind)
	}
	return nil
}

func (r *Reconciler) recoverVerifyingUploads(ctx context.Context, limit int, report *Report) error {
	var rows []uploadRow
	if err := scanPage(r, ctx, "verify", r.db.Table("upload_sessions").Select("id, version").
		Where("status = ?", "verifying"), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
	for _, row := range rows {
		key := "upload.verify:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "broker_task_missing", "upload_session", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectVerifyRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: row.ID}})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["object.verify"]++
		}
	}
	return failures
}

type mediaRow struct {
	ID        uuid.UUID
	Version   int64
	ObjectKey string
	BornAt    time.Time
}

func (row mediaRow) recoveryID() uuid.UUID { return row.ID }

func (r *Reconciler) recoverMedia(ctx context.Context, limit int, report *Report) error {
	var rows []mediaRow
	if err := scanPage(r, ctx, "media", r.db.Raw(`
		SELECT o.id, o.version, o.object_key, COALESCE(o.verified_at, o.created_at) AS born_at
		FROM file_objects o
		WHERE o.status = 'ready' AND o.mime_type LIKE 'image/%'
		  AND NOT EXISTS (
			SELECT 1 FROM object_variants v
			WHERE v.object_id = o.id AND v.variant_type = 'thumbnail'
			  AND v.pipeline_version = 'thumbnail-v1' AND v.status = 'ready'
		  )
		`), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
	for _, row := range rows {
		key := fmt.Sprintf("media.thumbnail:%s:thumbnail-v1:%x", row.ID, sha256.Sum256([]byte(row.ObjectKey)))
		created, err := r.ensureWithPolicy(ctx, key, "missing_media_variant", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventMediaThumbnailRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.MediaThumbnailRequestedV1{ObjectID: row.ID}}, false, row.BornAt)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["media.thumbnail"]++
		}
	}
	return failures
}

func (r *Reconciler) recoverAI(ctx context.Context, limit int, report *Report) error {
	if !r.aiEnabled {
		return nil
	}
	cutoff := r.now().Add(-r.staleAfter)
	var rows []uploadRow
	if err := scanPage(r, ctx, "ai", r.db.Raw(`
		SELECT o.id, o.version
		FROM file_objects o
		LEFT JOIN ai_documents d ON d.object_id = o.id
		WHERE o.status = 'ready' AND (
			d.id IS NULL OR (d.status IN ('pending', 'processing', 'building', 'failed') AND d.updated_at <= ?)
		)
		`, cutoff), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
	for _, row := range rows {
		key := "ai.index:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "ai_index_incomplete", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventAIIndexRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.AIIndexRequestedV1{ObjectID: row.ID}})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["ai.index"]++
		}
	}
	return failures
}

type taskRow struct {
	ID       uuid.UUID
	Resource uuid.UUID `gorm:"column:resource_id"`
	Version  int64
}

func (row taskRow) recoveryID() uuid.UUID { return row.ID }

func (r *Reconciler) recoverReprocessTasks(ctx context.Context, limit int, report *Report) error {
	if !r.aiEnabled {
		return nil
	}
	cutoff := r.now().Add(-r.staleAfter)
	var rows []taskRow
	if err := scanPage(r, ctx, "reprocess", r.db.Table("async_tasks").Select("id, resource_id, version").
		Where("task_type = ? AND resource_id IS NOT NULL AND status IN ? AND updated_at <= ?", "ai.reprocess", []string{"pending", "running", "failed"}, cutoff).
		Session(&gorm.Session{}), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
	for _, row := range rows {
		key := "ai.reprocess:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "ai_task_incomplete", "ai_task", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventAIReprocessRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.AIReprocessRequestedV1{TaskID: row.ID, ObjectID: row.Resource}})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["ai.reprocess"]++
		}
	}
	return failures
}

func (r *Reconciler) recoverGarbageCollection(ctx context.Context, limit int, report *Report) error {
	var rows []uploadRow
	if err := scanPage(r, ctx, "gc", r.db.Raw(`
		SELECT o.id, o.version
		FROM file_objects o
		WHERE o.status IN ('ready', 'deleting') AND o.reference_count = 0
		  AND NOT EXISTS (
			SELECT 1 FROM storage_operations so
			WHERE so.object_id = o.id AND so.operation_type = 'object.delete'
			  AND so.state IN ('pending', 'running', 'failed', 'dead')
		  )
		`), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
	for _, row := range rows {
		key := "object.gc:" + row.ID.String() + ":" + fmt.Sprint(row.Version)
		created, err := r.ensure(ctx, key, "zero_reference_gc", "file_object", row.ID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: row.ID, AggregateVersion: row.Version, Producer: "maintenance-recovery", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: row.ID}})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["object.gc"]++
		}
	}
	return failures
}

type operationRow struct {
	ID            uuid.UUID
	OperationType string
	ObjectID      *uuid.UUID
	UploadID      *uuid.UUID `gorm:"column:upload_session_id"`
	Version       int64
}

func (row operationRow) recoveryID() uuid.UUID { return row.ID }

func (r *Reconciler) recoverStorageOperations(ctx context.Context, limit int, report *Report) error {
	now := r.now()
	operationCutoff := now.Add(-r.operationStaleAfter)
	var rows []operationRow
	if err := scanPage(r, ctx, "storage", r.db.Table("storage_operations").Select("id, operation_type, object_id, upload_session_id, attempt as version").
		Where(`
			(state IN ? AND available_at <= ?)
			OR (state = ? AND (
				(lease_until IS NOT NULL AND lease_until <= ?)
				OR (lease_until IS NULL AND updated_at <= ?)
			))
			OR (state = ? AND (
				(operation_type = ? AND EXISTS (
					SELECT 1 FROM upload_sessions s WHERE s.id = storage_operations.upload_session_id AND s.status = ?
				)) OR
				(operation_type = ? AND EXISTS (
					SELECT 1 FROM upload_sessions s WHERE s.id = storage_operations.upload_session_id AND s.status = ?
				))
			))`,
			[]string{storageops.StatePending, storageops.StateFailed}, now,
			storageops.StateRunning, now, operationCutoff,
			storageops.StateSucceeded,
			storageops.MultipartCreate, "initializing",
			storageops.MultipartComplete, "completing").
		Session(&gorm.Session{}), limit, &rows); err != nil {
		return err
	}
	report.Scanned += len(rows)
	var failures error
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
		suffix := row.ID.String() + ":" + strings.ReplaceAll(row.OperationType, "/", "_")
		key := "storage.operation:" + suffix
		eventType := eventcontract.EventUploadOperationRecoverV1
		payload := any(eventcontract.UploadOperationRecoverV1{OperationID: row.ID, OperationType: row.OperationType, RecoveryKey: key})
		if row.OperationType == storageops.ObjectDelete || row.OperationType == storageops.VariantDelete {
			eventType = eventcontract.EventStorageOperationRecoverV1
			payload = eventcontract.StorageOperationRecoverV1{OperationID: row.ID, OperationType: row.OperationType, RecoveryKey: key}
		} else {
			// Older schedulers used the storage-operation key for maintenance
			// recovery events. Keep upload events in a separate idempotency
			// namespace so an existing legacy recovery receipt cannot suppress
			// delivery to the new upload queue during a rolling upgrade.
			key = "upload.operation_recovery:" + suffix
			payload = eventcontract.UploadOperationRecoverV1{OperationID: row.ID, OperationType: row.OperationType, RecoveryKey: key}
		}
		created, err := r.ensureStorageOperation(ctx, key, resourceType, resourceID, row.Version,
			eventcontract.EnvelopeInput{EventType: eventType, AggregateID: row.ID, AggregateVersion: maxVersion(row.Version), Producer: "maintenance-recovery", Payload: payload})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover %s: %w", row.ID, err))
			continue
		}
		if created {
			report.Repaired++
			report.ByKind["storage.operation"]++
		}
	}
	return failures
}

func maxVersion(value int64) int64 {
	if value <= 0 {
		return 1
	}
	return value
}

func (r *Reconciler) ensure(ctx context.Context, key, reason, resourceType string, resourceID uuid.UUID, version int64, input eventcontract.EnvelopeInput) (bool, error) {
	return r.ensureWithPolicy(ctx, key, reason, resourceType, resourceID, version, input, false)
}

func (r *Reconciler) ensureStorageOperation(ctx context.Context, key, resourceType string, resourceID uuid.UUID, version int64, input eventcontract.EnvelopeInput) (bool, error) {
	return r.ensureWithPolicy(ctx, key, "storage_operation_unfinished", resourceType, resourceID, version, input, true)
}

func (r *Reconciler) ensureWithPolicy(ctx context.Context, key, reason, resourceType string, resourceID uuid.UUID, version int64, input eventcontract.EnvelopeInput, retryDead bool, since ...time.Time) (bool, error) {
	var created bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := dblock.Transaction(tx, "recovery:"+key); err != nil {
			return err
		}
		family := func() *gorm.DB {
			prefix := key + ":after:"
			return tx.Model(&outbox.Recovery{}).Where("recovery_key = ? OR left(recovery_key, ?) = ?", key, len(prefix), prefix)
		}
		var stopped int64
		if err := family().Where("stopped_at IS NOT NULL").Count(&stopped).Error; err != nil || stopped > 0 {
			return err
		}
		var active, replayed, terminalDead bool
		var err error
		if retryDead {
			active, replayed, terminalDead, err = outbox.RecoverDeliveryForRetryableWork(tx, input.EventType, input.AggregateID, r.now(), r.staleAfter)
		} else if len(since) > 0 {
			active, replayed, err = outbox.RecoverDeliverySince(tx, input.EventType, input.AggregateID, r.now(), r.staleAfter, since[0])
		} else {
			active, replayed, err = outbox.RecoverDelivery(tx, input.EventType, input.AggregateID, r.now(), r.staleAfter)
		}
		if err != nil {
			return err
		}
		if active {
			created = replayed
			return nil
		}
		var attempts int64
		if err := family().Count(&attempts).Error; err != nil {
			return err
		}
		if attempts >= MaxRecoveryEvents {
			if retryDead {
				var operation storageops.Operation
				err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", input.AggregateID).Take(&operation).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil && operation.State == storageops.StateRunning && operation.LeaseUntil != nil && operation.LeaseUntil.After(r.now()) {
					return nil
				}
				if err == nil && operation.State != storageops.StateSucceeded && operation.State != storageops.StateDead {
					if err := tx.Model(&operation).Updates(map[string]any{"state": storageops.StateDead, "lease_token": nil, "lease_until": nil, "completed_at": r.now(), "last_error": "automatic recovery budget exhausted"}).Error; err != nil {
						return err
					}
				}
			}
			if err := family().Where("stopped_at IS NULL").Update("stopped_at", r.now()).Error; err != nil {
				return err
			}
			slog.ErrorContext(ctx, "automatic recovery stopped; manual repair required", "recovery_key", key, "resource_id", resourceID, "event_type", input.EventType, "attempts", attempts)
			return nil
		}
		// A published command can be lost from the broker more than once.
		// Chain a fresh event ID after the last publication, with a grace period
		// so scans cannot continually duplicate normally running work.
		var previous outbox.Event
		previousQuery := tx.Where("event_type = ? AND aggregate_id = ? AND status = 'published'", input.EventType, input.AggregateID)
		if len(since) > 0 && !since[0].IsZero() {
			previousQuery = previousQuery.Where("created_at >= ?", since[0])
		}
		err = previousQuery.Order("published_at DESC NULLS LAST, created_at DESC, id DESC").Take(&previous).Error
		if err == nil {
			publishedAt := previous.CreatedAt
			if previous.PublishedAt != nil {
				publishedAt = *previous.PublishedAt
			}
			if !terminalDead && publishedAt.After(r.now().Add(-r.staleAfter)) {
				return nil
			}
			key += ":after:" + previous.ID.String()
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		switch payload := input.Payload.(type) {
		case eventcontract.UploadOperationRecoverV1:
			payload.RecoveryKey = key
			input.Payload = payload
		case eventcontract.StorageOperationRecoverV1:
			payload.RecoveryKey = key
			input.Payload = payload
		}
		created, err = outbox.EnsureRecovery(tx, key, reason, resourceType, resourceID, maxVersion(version), input)
		return err
	})
	return created, err
}
