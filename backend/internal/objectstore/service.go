package objectstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

type Object struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	SHA256         string
	SizeBytes      int64
	MimeType       string
	Bucket         string
	ObjectKey      string
	Status         string
	ReferenceCount int64
	Version        int64
	VerifiedAt     *time.Time
	DeletedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type GCStorage interface {
	RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error
}

type objectVariant struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	ObjectKey string
}

func (objectVariant) TableName() string { return "object_variants" }

func (s *Service) GarbageCollect(ctx context.Context, storage GCStorage, objectID uuid.UUID) error {
	var operation storageops.Operation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var object Object
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&object, "id = ?", objectID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var references int64
		if err := tx.Table("file_entries").Where("object_id = ? AND deleted_at IS NULL", objectID).Count(&references).Error; err != nil {
			return err
		}
		if references > 0 || object.ReferenceCount > 0 || object.Status == "deleted" {
			return nil
		}
		if object.Status == "ready" {
			object.Status = "deleting"
			object.Version++
			if err := tx.Model(&object).Updates(map[string]any{"status": object.Status, "version": object.Version, "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
		var err error
		operation, err = storageops.Enqueue(tx, storageops.Input{
			OperationType: storageops.ObjectDelete, IdempotencyKey: fmt.Sprintf("object.delete:%s:%d", objectID, object.Version),
			ObjectID: &objectID, SourceBucket: object.Bucket, SourceKey: object.ObjectKey,
		})
		return err
	})
	if err != nil || operation.ID == uuid.Nil {
		return err
	}
	return s.ReconcileObjectDeleteOperation(ctx, storage, operation.ID)
}

// ReconcileObjectDeletes completes object.delete operations claimed after a
// worker crash. It rechecks references immediately before the final database
// transition, so a stale operation cannot delete a newly referenced object.
func (s *Service) ReconcileObjectDeletes(ctx context.Context, storage GCStorage, limit int) (int, error) {
	operations, err := storageops.ClaimTypes(ctx, s.db, limit, 5*time.Minute, storageops.ObjectDelete)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, operation := range operations {
		if err := s.reconcileObjectDelete(ctx, storage, operation); err != nil {
			if operation.LeaseToken != nil {
				_ = storageops.MarkFailedOwned(s.db, operation.ID, *operation.LeaseToken, err, time.Second)
			}
			return completed, err
		}
		completed++
	}
	return completed, nil
}

// ReconcileObjectDeleteOperation is the single-operation form used by the
// broker-loss recovery event. Claiming is lease-fenced and therefore safe to
// invoke repeatedly from multiple maintenance workers.
func (s *Service) ReconcileObjectDeleteOperation(ctx context.Context, storage GCStorage, operationID uuid.UUID) error {
	operation, err := storageops.Get(s.db, operationID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if operation.State == storageops.StateSucceeded {
		return nil
	}
	claimed, token, err := storageops.TryStartWithToken(s.db, operation.ID, 5*time.Minute)
	if err != nil || !claimed {
		return err
	}
	operation.LeaseToken = &token
	if err := s.reconcileObjectDelete(ctx, storage, operation); err != nil {
		_ = storageops.MarkFailedOwned(s.db, operation.ID, token, err, time.Second)
		return err
	}
	return nil
}

func (s *Service) reconcileObjectDelete(ctx context.Context, storage GCStorage, operation storageops.Operation) error {
	if operation.ObjectID == nil {
		return errors.New("object delete operation has no object")
	}
	var object Object
	if err := s.db.First(&object, "id = ?", *operation.ObjectID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
		}
		return err
	}
	var references int64
	if err := s.db.Table("file_entries").Where("object_id = ? AND deleted_at IS NULL", object.ID).Count(&references).Error; err != nil {
		return err
	}
	if references > 0 || object.ReferenceCount > 0 || object.Status != "deleting" || object.ObjectKey != operation.SourceKey || object.Bucket != operation.SourceBucket {
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	}
	var variants []objectVariant
	if err := s.db.Where("object_id = ?", object.ID).
		Where("EXISTS (SELECT 1 FROM file_objects o WHERE o.id = object_variants.object_id AND o.status = 'deleting' AND o.object_key = ?)", operation.SourceKey).Find(&variants).Error; err != nil {
		return err
	}
	for _, variant := range variants {
		if err := storage.RemoveObject(ctx, object.Bucket, variant.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	if err := storage.RemoveObject(ctx, operation.SourceBucket, operation.SourceKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owned storageops.Operation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND state = ? AND lease_token = ?", operation.ID, storageops.StateRunning, operationLeaseToken(operation)).First(&owned).Error; err != nil {
			return err
		}
		var latest Object
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&latest, "id = ?", object.ID).Error; err != nil {
			return err
		}
		var remaining int64
		if err := tx.Table("file_entries").Where("object_id = ? AND deleted_at IS NULL", object.ID).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining > 0 || latest.ReferenceCount > 0 {
			return errors.New("object reference changed during delete reconciliation")
		}
		if latest.Status != "deleting" || latest.ObjectKey != operation.SourceKey {
			return storageops.MarkSucceededOwned(tx, operation.ID, operationLeaseToken(operation))
		}
		if err := tx.Where("object_id = ?", object.ID).Delete(&objectVariant{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM ai_documents WHERE object_id = ?", object.ID).Error; err != nil {
			return err
		}
		if err := tx.Model(&Object{}).Where("id = ? AND status = 'deleting' AND reference_count = 0", object.ID).
			Updates(map[string]any{"status": "deleted", "deleted_at": now, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		// Publish the committed deletion too: the original GC request may have
		// reached search before the object was actually retired.
		if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: object.ID, AggregateVersion: latest.Version + 1,
			Producer: "object-gc-completed", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: object.ID},
		}); err != nil {
			return err
		}
		return storageops.MarkSucceededOwned(tx, operation.ID, operationLeaseToken(operation))
	})
}

func operationLeaseToken(operation storageops.Operation) uuid.UUID {
	if operation.LeaseToken == nil {
		return uuid.Nil
	}
	return *operation.LeaseToken
}

func (Object) TableName() string { return "file_objects" }

func (o *Object) BeforeCreate(_ *gorm.DB) error {
	if o.Version == 0 {
		o.Version = 1
	}
	return nil
}

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service { return &Service{db: db} }

func (s *Service) FindOwned(ownerID uuid.UUID, sha256 string, size int64) (Object, error) {
	return s.FindOwnedWith(s.db, ownerID, sha256, size)
}

func (s *Service) FindOwnedWith(db *gorm.DB, ownerID uuid.UUID, sha256 string, size int64) (Object, error) {
	var object Object
	err := db.Raw(`
        SELECT DISTINCT o.*
        FROM file_objects o
        JOIN file_entries e ON e.object_id = o.id
        WHERE e.owner_id = ? AND e.deleted_at IS NULL
          AND o.sha256 = ? AND o.size_bytes = ? AND o.status = 'ready'
        LIMIT 1`, ownerID, sha256, size).Scan(&object).Error
	if err != nil {
		return Object{}, err
	}
	if object.ID == uuid.Nil {
		return Object{}, gorm.ErrRecordNotFound
	}
	return object, nil
}

func (s *Service) Get(objectID uuid.UUID) (Object, error) {
	var object Object
	if err := s.db.Where("id = ? AND status = 'ready'", objectID).First(&object).Error; err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *Service) FindByHash(sha256 string, size int64) (Object, error) {
	return s.FindByHashWith(s.db, sha256, size)
}

func (s *Service) FindByHashWith(db *gorm.DB, sha256 string, size int64) (Object, error) {
	var object Object
	if err := db.Where("sha256 = ? AND size_bytes = ? AND status = 'ready'", sha256, size).First(&object).Error; err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *Service) CreateOrGet(tx *gorm.DB, object Object) (Object, error) {
	if tx == nil {
		return Object{}, errors.New("object transaction is nil")
	}
	if object.Version == 0 {
		object.Version = 1
	}
	result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "sha256"}, {Name: "size_bytes"}}, DoNothing: true}).Create(&object)
	if result.Error != nil {
		return Object{}, result.Error
	}
	if result.RowsAffected == 0 {
		var existing Object
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("sha256 = ? AND size_bytes = ?", object.SHA256, object.SizeBytes).First(&existing).Error; err != nil {
			return Object{}, err
		}
		switch existing.Status {
		case "ready":
			return existing, nil
		case "deleted":
			if object.ObjectKey != RevivalKey(existing) {
				return Object{}, errors.New("canonical object generation changed; retry verification")
			}
			// The canonical object has already been copied by ensureCanonical.
			// Reuse the durable row so the global hash/size key remains unique.
			now := time.Now().UTC()
			result := tx.Model(&Object{}).Where("id = ? AND status = 'deleted' AND reference_count = 0", existing.ID).
				Updates(map[string]any{
					"mime_type": object.MimeType, "bucket": object.Bucket, "object_key": object.ObjectKey,
					"status": "ready", "verified_at": object.VerifiedAt, "deleted_at": nil,
					"version": gorm.Expr("version + 1"), "updated_at": now,
				})
			if result.Error != nil {
				return Object{}, result.Error
			}
			if result.RowsAffected != 1 {
				return Object{}, errors.New("deleted object changed during reuse")
			}
			if err := tx.First(&existing, "id = ?", existing.ID).Error; err != nil {
				return Object{}, err
			}
			return existing, nil
		case "deleting":
			return Object{}, errors.New("object is being deleted")
		default:
			return Object{}, errors.New("object has unknown status")
		}
	}
	return object, nil
}

// RevivalKey isolates new bytes from delayed deletes issued for an older life
// of the same content-addressed database row.
func RevivalKey(object Object) string {
	return fmt.Sprintf("objects/sha256/%s/%s/v%d", object.SHA256[:2], object.SHA256, object.Version)
}

func (s *Service) AddReference(tx *gorm.DB, objectID uuid.UUID) error {
	result := tx.Model(&Object{}).Where("id = ? AND status = 'ready'", objectID).
		Updates(map[string]any{"reference_count": gorm.Expr("reference_count + 1"), "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("object not ready")
	}
	return nil
}

func (s *Service) ReleaseReference(tx *gorm.DB, objectID uuid.UUID) (int64, error) {
	if err := tx.Model(&Object{}).Where("id = ? AND reference_count > 0", objectID).
		Updates(map[string]any{"reference_count": gorm.Expr("reference_count - 1"), "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
		return 0, err
	}
	var object Object
	if err := tx.First(&object, "id = ?", objectID).Error; err != nil {
		return 0, err
	}
	return object.ReferenceCount, nil
}
