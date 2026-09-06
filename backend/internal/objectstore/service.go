package objectstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	var object Object
	if err := s.db.First(&object, "id = ?", objectID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	var references int64
	if err := s.db.Table("file_entries").Where("object_id = ? AND deleted_at IS NULL", objectID).Count(&references).Error; err != nil {
		return err
	}
	if references > 0 || object.ReferenceCount > 0 || object.Status == "deleted" {
		return nil
	}
	result := s.db.Model(&Object{}).
		Where("id = ? AND reference_count = 0 AND status IN ?", objectID, []string{"ready", "deleting"}).
		Updates(map[string]any{"status": "deleting", "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return nil
	}

	operation, err := storageops.Enqueue(s.db, storageops.Input{
		OperationType: storageops.ObjectDelete, IdempotencyKey: "object.delete:" + objectID.String(),
		ObjectID: &objectID, SourceBucket: object.Bucket, SourceKey: object.ObjectKey,
	})
	if err != nil {
		return err
	}
	if operation.State == storageops.StateSucceeded {
		return nil
	}
	started, leaseToken, err := storageops.TryStartWithToken(s.db, operation.ID, 5*time.Minute)
	if err != nil {
		return err
	}
	if !started {
		return nil
	}
	var variants []objectVariant
	if err := s.db.Where("object_id = ?", objectID).Find(&variants).Error; err != nil {
		return err
	}
	for _, variant := range variants {
		if err := storage.RemoveObject(ctx, object.Bucket, variant.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
			_ = storageops.MarkFailedOwned(s.db, operation.ID, leaseToken, err, time.Second)
			return err
		}
	}
	if err := storage.RemoveObject(ctx, object.Bucket, object.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
		_ = storageops.MarkFailedOwned(s.db, operation.ID, leaseToken, err, time.Second)
		return err
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("object_id = ?", objectID).Delete(&objectVariant{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM ai_documents WHERE object_id = ?", objectID).Error; err != nil {
			return err
		}
		result := tx.Model(&Object{}).
			Where("id = ? AND reference_count = 0 AND status = 'deleting'", objectID).
			Updates(map[string]any{"status": "deleted", "deleted_at": now, "version": gorm.Expr("version + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("object reference changed during garbage collection")
		}
		return storageops.MarkSucceededOwned(tx, operation.ID, leaseToken)
	})
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
	if references > 0 || object.ReferenceCount > 0 || object.Status == "deleted" {
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	}
	var variants []objectVariant
	if err := s.db.Where("object_id = ?", object.ID).Find(&variants).Error; err != nil {
		return err
	}
	for _, variant := range variants {
		if err := storage.RemoveObject(ctx, object.Bucket, variant.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	if err := storage.RemoveObject(ctx, object.Bucket, object.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
	if err != nil || object.ID == uuid.Nil {
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
	if object.Version == 0 {
		object.Version = 1
	}
	result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "sha256"}, {Name: "size_bytes"}}, DoNothing: true}).Create(&object)
	if result.Error != nil {
		return Object{}, result.Error
	}
	if result.RowsAffected == 0 {
		var existing Object
		if err := tx.Where("sha256 = ? AND size_bytes = ? AND status = 'ready'", object.SHA256, object.SizeBytes).First(&existing).Error; err != nil {
			return Object{}, err
		}
		return existing, nil
	}
	return object, nil
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
