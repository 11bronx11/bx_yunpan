package upload

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

// CommitAsync performs business changes only. The caller owns the transaction
// and records its effect receipt atomically. No object-store call is allowed here.
func (s *Service) CommitAsync(tx *gorm.DB, kind string, in asyncprotocol.Input, out asyncprotocol.Output) (asyncprotocol.Decision, error) {
	ctx := tx.Statement.Context
	decision := asyncprotocol.Decision{Outcome: "succeeded"}
	var session Session
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id=?", in.SessionID).Error; err != nil {
		return decision, err
	}
	if session.UserID != in.OwnerID {
		return decision, ErrInvalidInput
	}
	terminal := session.Status == StatusCompleted || session.Status == StatusFailed || session.Status == StatusAborted || session.Status == StatusExpired
	if out.Convergence || out.Error != "" {
		if out.Key != "" && strings.HasPrefix(out.Key, "objects/compute/") {
			if err := cleanupPrivate(tx, session, out.Bucket, out.Key); err != nil {
				return decision, err
			}
		}
		if session.Status == StatusCompleted {
			return decision, nil
		}
		if !terminal {
			if err := s.failAsync(tx, session, StatusFailed, out.Error); err != nil {
				return decision, err
			}
		}
		decision.Outcome = "failed"
		return decision, nil
	}
	switch kind {
	case "upload.expire":
		if session.Status != StatusInitializing && session.Status != StatusCreated && session.Status != StatusUploading {
			decision.Outcome = "superseded"
			return decision, nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return decision, err
		}
		if session.ExpiresAt.After(now) {
			decision.RescheduleAt = &session.ExpiresAt
			return decision, nil
		}
		return decision, s.failAsync(tx, session, StatusExpired, "upload.expired")
	case storageops.MultipartCreate:
		if out.UploadID == "" {
			return decision, ErrInvalidInput
		}
		if terminal || session.Status != StatusInitializing {
			if session.StorageUploadID != out.UploadID {
				if err := s.abortKnown(tx, session, out.UploadID); err != nil {
					return decision, err
				}
			}
			decision.Outcome = "superseded"
		} else {
			if err := tx.Model(&Session{}).Where("id=? AND status=?", session.ID, StatusInitializing).Updates(map[string]any{"status": StatusCreated, "storage_upload_id": out.UploadID, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
				return decision, err
			}
		}
	case storageops.MultipartComplete:
		if session.Status == StatusCompleting {
			if err := tx.Model(&Session{}).Where("id=?", session.ID).Updates(map[string]any{"status": StatusVerifying, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
				return decision, err
			}
			if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectVerifyRequestedV1, AggregateID: session.ID, AggregateVersion: session.Version + 1, Producer: "committer", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: session.ID}}); err != nil {
				return decision, err
			}
		} else if session.Status != StatusVerifying && session.Status != StatusCompleted {
			decision.Outcome = "superseded"
			if _, err := enqueueStagingDelete(tx, session); err != nil {
				return decision, err
			}
		}
	case "upload.verify":
		if session.Status != StatusVerifying {
			decision.Outcome = "superseded"
			return decision, nil
		}
		if out.SHA256 != session.DeclaredSHA256 || out.Size != session.SizeBytes {
			decision.Outcome = "rejected"
			return decision, s.failAsync(tx, session, StatusFailed, "upload.hash_mismatch")
		}
		if err := contentLock(tx, session); err != nil {
			return decision, err
		}
		object, err := s.objects.FindByHashWith(tx, out.SHA256, out.Size)
		if err == nil {
			return decision, s.finishAsync(tx, session, object)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return decision, err
		}
		// Each upload owns its copy operation. Concurrent identical content is
		// reconciled at final commit; the losing private output is cleaned explicitly.
		id := uuid.NewSHA1(session.ID, []byte("canonical-copy-v1"))
		_, err = storageops.Enqueue(tx, storageops.Input{ID: id, OperationType: storageops.CanonicalEnsure, IdempotencyKey: "canonical.ensure:" + session.ID.String(), UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey, TargetBucket: session.Bucket, TargetKey: "objects/compute/" + id.String() + "/"})
		return decision, err
	case storageops.CanonicalEnsure:
		if !strings.HasPrefix(out.Key, in.TargetKey) || out.Bucket != in.TargetBucket || out.Key == in.TargetKey {
			return decision, ErrInvalidInput
		}
		if session.Status != StatusVerifying {
			decision.Outcome = "superseded"
			if err := cleanupPrivate(tx, session, out.Bucket, out.Key); err != nil {
				return decision, err
			}
			return decision, markIntent(tx, in.OperationID, out.UploadID)
		}
		if err := contentLock(tx, session); err != nil {
			return decision, err
		}
		if err := lockAsyncDestination(tx, session); err != nil {
			return decision, err
		}
		var object objectstore.Object
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("sha256=? AND size_bytes=?", session.DeclaredSHA256, session.SizeBytes).First(&object).Error
		now := time.Now().UTC()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			object = objectstore.Object{ID: uuid.NewSHA1(session.ID, []byte("object-v1")), SHA256: session.DeclaredSHA256, SizeBytes: session.SizeBytes, MimeType: session.MimeType, Bucket: out.Bucket, ObjectKey: out.Key, Status: "ready", Version: 1, ContentGeneration: 1, VerifiedAt: &now}
			if err := tx.Create(&object).Error; err != nil {
				return decision, err
			}
		} else if err != nil {
			return decision, err
		} else if object.Status == "deleted" {
			q := tx.Model(&object).Where("status='deleted' AND reference_count=0").Updates(map[string]any{"status": "ready", "bucket": out.Bucket, "object_key": out.Key, "content_generation": gorm.Expr("content_generation+1"), "version": gorm.Expr("version+1"), "verified_at": now, "deleted_at": nil, "updated_at": now})
			if q.Error != nil {
				return decision, q.Error
			}
			if q.RowsAffected != 1 {
				return decision, ErrConflict
			}
			if err := tx.First(&object, "id=?", object.ID).Error; err != nil {
				return decision, err
			}
		} else if object.Status != "ready" {
			return decision, ErrConflict
		}
		if object.ObjectKey != out.Key {
			if err := cleanupPrivate(tx, session, out.Bucket, out.Key); err != nil {
				return decision, err
			}
		}
		if err := s.finishAsync(tx, session, object); err != nil {
			return decision, err
		}
	case storageops.MultipartAbort:
		if session.StorageUploadID == in.UploadID {
			if err := tx.Model(&session).Update("storage_upload_id", "").Error; err != nil {
				return decision, err
			}
		}
	case storageops.StagingDelete, storageops.VariantDelete, "multipart.sweep":
	default:
		return decision, ErrInvalidInput
	}
	if in.OperationID != uuid.Nil {
		if err := markIntent(tx, in.OperationID, out.UploadID); err != nil {
			return decision, err
		}
	}
	return decision, nil
}

func contentLock(tx *gorm.DB, s Session) error {
	return dblock.Transaction(tx, "upload:object:"+s.DeclaredSHA256+":"+strconv.FormatInt(s.SizeBytes, 10))
}

func markIntent(tx *gorm.DB, id uuid.UUID, uploadID string) error {
	fields := map[string]any{"state": storageops.StateSucceeded, "completed_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()"), "lease_until": nil, "lease_token": nil}
	if uploadID != "" {
		fields["storage_upload_id"] = uploadID
	}
	return tx.Model(&storageops.Operation{}).Where("id=?", id).Updates(fields).Error
}

func (s *Service) finishAsync(tx *gorm.DB, session Session, object objectstore.Object) error {
	if err := lockAsyncDestination(tx, session); err != nil {
		return err
	}
	var count int64
	if err := tx.Model(&drive.FileEntry{}).Where("owner_id=? AND deleted_at IS NULL AND ((folder_id=? AND name_normalized=?) OR object_id=?)", session.UserID, session.FolderID, session.FilenameNormalized, object.ID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrNameConflict
	}
	entry, err := s.drive.CreateFile(tx, session.UserID, session.FolderID, object.ID, session.Filename)
	if err != nil {
		return err
	}
	if err := s.objects.AddReference(tx, object.ID); err != nil {
		return err
	}
	if err := s.quota.CommitQuota(tx, session.UserID, session.ReservedBytes, session.SizeBytes); err != nil {
		return err
	}
	if err := tx.Model(&session).Updates(map[string]any{"status": StatusCompleted, "completed_object_id": object.ID, "completed_entry_id": entry.ID, "reserved_bytes": 0, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
		return err
	}
	epoch, err := ownerepoch.BumpContent(tx, session.UserID)
	if err != nil {
		return err
	}
	if err := outbox.AddTyped(tx.Statement.Context, tx, eventcontract.EnvelopeInput{EventType: eventcontract.EventFileCreatedV1, AggregateID: entry.ID, AggregateVersion: entry.Version, Producer: "committer", Payload: eventcontract.FileCreatedV1{OwnerID: session.UserID, FileEntryID: entry.ID, ObjectID: object.ID, ContentEpoch: epoch.ContentEpoch}}); err != nil {
		return err
	}
	if err := outbox.AddTyped(tx.Statement.Context, tx, eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectReadyV1, AggregateID: object.ID, AggregateVersion: object.Version + 1, Producer: "committer", Payload: eventcontract.ObjectReadyV1{ObjectID: object.ID}}); err != nil {
		return err
	}
	_, err = enqueueStagingDelete(tx, session)
	return err
}

func (s *Service) failAsync(tx *gorm.DB, session Session, state, code string) error {
	// Provider diagnostics remain in the durable control result. This column
	// is a bounded public error code, not an arbitrary remote error message.
	switch code {
	case "upload.expired", "upload.hash_mismatch", "upload.commit_precondition_failed":
	default:
		code = "upload.async_exhausted"
	}
	if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
		return err
	}
	if err := tx.Model(&session).Updates(map[string]any{"status": state, "error_code": code, "reserved_bytes": 0, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
		return err
	}
	if session.StorageUploadID != "" {
		if err := s.abortKnown(tx, session, session.StorageUploadID); err != nil {
			return err
		}
	}
	if err := enqueueMultipartSweep(tx, session); err != nil {
		return err
	}
	_, err := enqueueStagingDelete(tx, session)
	return err
}

func (s *Service) abortKnown(tx *gorm.DB, session Session, id string) error {
	_, err := storageops.Enqueue(tx, storageops.Input{OperationType: storageops.MultipartAbort, IdempotencyKey: fmt.Sprintf("multipart.abort:%s:%s", session.ID, id), UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey, StorageUploadID: id})
	return err
}

func cleanupPrivate(tx *gorm.DB, session Session, bucket, key string) error {
	if !strings.HasPrefix(key, "objects/compute/") && !strings.HasPrefix(key, "variants/compute/") {
		return ErrInvalidInput
	}
	_, err := storageops.Enqueue(tx, storageops.Input{OperationType: storageops.StagingDelete, IdempotencyKey: "private.delete:" + bucket + ":" + key, UploadSessionID: &session.ID, SourceBucket: bucket, SourceKey: key})
	return err
}

// Keep domain APIs independent of transport and bootstrap.
var _ = context.Background

func enqueueMultipartSweep(tx *gorm.DB, session Session) error {
	_, err := storageops.Enqueue(tx, storageops.Input{OperationType: "multipart.sweep", IdempotencyKey: "multipart.sweep:" + session.ID.String(), UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey})
	return err
}

func lockAsyncDestination(tx *gorm.DB, session Session) error {
	// Lock namespace before folder/file/object updates, matching API creation.
	if err := dblock.Transaction(tx, "upload:filename:"+session.UserID.String()+":"+session.FolderID.String()+":"+session.FilenameNormalized); err != nil {
		return err
	}
	var parent drive.Folder
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id=? AND owner_id=? AND deleted_at IS NULL", session.FolderID, session.UserID).First(&parent).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
