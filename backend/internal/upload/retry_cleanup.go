package upload

import (
	"context"
	"errors"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RetireStoppedSessions releases quota when durable upload work has stopped.
// Cleanup operations keep their own budgets and remain auditable if they fail.
func (s *Service) RetireStoppedSessions(ctx context.Context, limit int) (int, error) {
	retired, failures := s.retireStoppedVerifications(ctx, limit)
	if limit <= retired {
		return retired, failures
	}
	var sessions []Session
	if err := s.maintenancePage(ctx, "stopped_uploads", s.db.Model(&Session{}).Where("status IN ?", []string{StatusInitializing, StatusCompleting}).Where(`EXISTS (
		SELECT 1 FROM storage_operations op WHERE op.upload_session_id = upload_sessions.id
		AND ((op.operation_type = 'multipart.create' AND upload_sessions.status = 'initializing')
		  OR (op.operation_type = 'multipart.complete' AND upload_sessions.status = 'completing'))
		AND (op.state = 'dead' OR EXISTS (
			SELECT 1 FROM outbox_recoveries r JOIN outbox_events e ON e.id = r.event_id
			WHERE e.aggregate_id = op.id AND r.stopped_at IS NOT NULL
		))
	)`), limit-retired, &sessions); err != nil {
		return retired, errors.Join(failures, err)
	}
	for _, candidate := range sessions {
		changed := false
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var session Session
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", candidate.ID).Error; err != nil {
				return err
			}
			if session.Status != StatusInitializing && session.Status != StatusCompleting {
				return nil
			}
			kind := storageops.MultipartCreate
			if session.Status == StatusCompleting {
				kind = storageops.MultipartComplete
			}
			var operation storageops.Operation
			q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("upload_session_id = ? AND operation_type = ?", session.ID, kind).First(&operation)
			if q.Error != nil {
				return q.Error
			}
			if operation.State != storageops.StateDead {
				var stopped int64
				if err := tx.Table("outbox_recoveries r").Joins("JOIN outbox_events e ON e.id = r.event_id").Where("e.aggregate_id = ? AND r.stopped_at IS NOT NULL", operation.ID).Count(&stopped).Error; err != nil {
					return err
				}
				if stopped == 0 {
					return nil
				}
				// A live replacement must get a chance to commit before retiring
				// its session, even if an earlier recovery family was stopped.
				if operation.State == storageops.StateRunning {
					return nil
				}
				if operation.State != storageops.StateSucceeded {
					if err := tx.Model(&operation).Updates(map[string]any{"state": storageops.StateDead, "completed_at": time.Now().UTC(), "lease_until": nil, "lease_token": nil, "last_error": "automatic recovery budget exhausted"}).Error; err != nil {
						return err
					}
				}
			}
			if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
				return err
			}
			if err := tx.Model(&session).Updates(map[string]any{"status": StatusFailed, "reserved_bytes": 0, "error_code": "upload.storage_operation_exhausted", "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
			uploadID := session.StorageUploadID
			if uploadID == "" {
				uploadID = operation.StorageUploadID
			}
			if uploadID != "" {
				if _, err := enqueueUploadOperation(tx, storageops.Input{OperationType: storageops.MultipartAbort, IdempotencyKey: "multipart.abort:" + session.ID.String(), UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey, StorageUploadID: uploadID}); err != nil {
					return err
				}
			}
			if _, err := enqueueStagingDelete(tx, session); err != nil {
				return err
			}
			changed = true
			return nil
		})
		failures = errors.Join(failures, err)
		if err == nil && changed {
			retired++
		}
	}
	return retired, failures
}

// The last verify attempt can fail while persisting its domain failure (for
// example during a short DB outage). Its task receipt may already be dead;
// finish releasing quota instead of leaving a verifying session forever.
func (s *Service) retireStoppedVerifications(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	var sessions []Session
	if err := s.maintenancePage(ctx, "stopped_verifications", s.db.Model(&Session{}).Where("status = ?", StatusVerifying).Where(`EXISTS (
		SELECT 1 FROM outbox_events e
		LEFT JOIN task_results t ON t.task_id = e.id::text || ':object.verify.v1@object'
		WHERE e.aggregate_id = upload_sessions.id AND e.event_type = 'object.verify_requested.v1'
		AND (t.status = 'dead' OR EXISTS (SELECT 1 FROM outbox_recoveries r WHERE r.event_id = e.id AND r.stopped_at IS NOT NULL))
	)`), limit, &sessions); err != nil {
		return 0, err
	}
	retired := 0
	var failures error
	for _, session := range sessions {
		if err := s.fail(ctx, session.ID, "upload.verification_failed"); err != nil {
			failures = errors.Join(failures, err)
		} else {
			retired++
		}
	}
	return retired, failures
}
