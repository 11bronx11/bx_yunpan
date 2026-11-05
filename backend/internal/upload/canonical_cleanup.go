package upload

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

// A verified copy can outlive a failed business transaction. Adopt it as a
// zero-reference object and use the normal, generation-fenced GC path. Never
// delete a shared canonical key directly from an upload failure handler.
func (s *Service) registerCanonicalCleanup(ctx context.Context, tx *gorm.DB, session Session) error {
	var operations []storageops.Operation
	if err := tx.Where("upload_session_id = ? AND operation_type = ?", session.ID, storageops.CanonicalEnsure).Find(&operations).Error; err != nil {
		return err
	}
	if len(operations) == 0 {
		return nil
	}
	if err := dblock.Transaction(tx, "upload:object:"+session.DeclaredSHA256+":"+strconv.FormatInt(session.SizeBytes, 10)); err != nil {
		return err
	}
	for _, operation := range operations {
		var object objectstore.Object
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("sha256 = ? AND size_bytes = ?", session.DeclaredSHA256, session.SizeBytes).First(&object).Error
		if err == nil {
			if object.Status != "deleted" || operation.TargetKey != objectstore.RevivalKey(object) {
				continue
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now().UTC()
		object, err = s.objects.CreateOrGet(tx, objectstore.Object{
			ID: uuid.Must(uuid.NewV7()), SHA256: session.DeclaredSHA256, SizeBytes: session.SizeBytes,
			MimeType: session.MimeType, Bucket: operation.TargetBucket, ObjectKey: operation.TargetKey,
			Status: "ready", Version: 1, VerifiedAt: &now,
		})
		if err != nil {
			return err
		}
		// A timed-out copy may not exist yet. Keep the adopted row deleting,
		// so another upload cannot use it as a verified, ready object.
		object.Version++
		if err := tx.Model(&object).Updates(map[string]any{"status": "deleting", "version": object.Version, "updated_at": now}).Error; err != nil {
			return err
		}
		if _, err := storageops.Enqueue(tx, storageops.Input{
			OperationType: storageops.ObjectDelete, IdempotencyKey: "object.delete:" + object.ID.String() + ":" + strconv.FormatInt(object.Version, 10),
			ObjectID: &object.ID, SourceBucket: object.Bucket, SourceKey: object.ObjectKey,
		}); err != nil {
			return err
		}
		if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: object.ID, AggregateVersion: object.Version,
			Producer: "upload-cleanup", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: object.ID},
		}); err != nil {
			return err
		}
	}
	return nil
}

// Recover copies left by earlier releases or interrupted terminal-state
// handling. Inventory-only leftovers without an operation need a bucket audit.
func (s *Service) ReconcileCanonicalOrphans(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	var sessions []Session
	err := s.maintenancePage(ctx, "canonical_orphans", s.db.Model(&Session{}).Where("status IN ?", []string{StatusFailed, StatusAborted, StatusExpired}).Where(`EXISTS (
		SELECT 1 FROM storage_operations op WHERE op.upload_session_id = upload_sessions.id
		AND op.operation_type = 'canonical.ensure' AND op.state = 'succeeded'
		AND NOT EXISTS (SELECT 1 FROM file_objects o WHERE o.bucket = op.target_bucket AND o.object_key = op.target_key)
		AND NOT EXISTS (SELECT 1 FROM file_objects o WHERE o.sha256 = upload_sessions.declared_sha256 AND o.size_bytes = upload_sessions.size_bytes AND o.status <> 'deleted')
	)`), limit, &sessions)
	if err != nil {
		return 0, err
	}
	repaired := 0
	var failures error
	for _, session := range sessions {
		if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", session.ID).Error; err != nil {
				return err
			}
			return s.registerCanonicalCleanup(ctx, tx, session)
		}); err != nil {
			failures = errors.Join(failures, err)
		} else {
			repaired++
		}
	}
	return repaired, failures
}
