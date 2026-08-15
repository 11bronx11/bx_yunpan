package objectstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
		Update("status", "deleting")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return nil
	}

	var variants []objectVariant
	if err := s.db.Where("object_id = ?", objectID).Find(&variants).Error; err != nil {
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
		if err := tx.Where("object_id = ?", objectID).Delete(&objectVariant{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM ai_documents WHERE object_id = ?", objectID).Error; err != nil {
			return err
		}
		result := tx.Model(&Object{}).
			Where("id = ? AND reference_count = 0 AND status = 'deleting'", objectID).
			Updates(map[string]any{"status": "deleted", "deleted_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("object reference changed during garbage collection")
		}
		return nil
	})
}

func (Object) TableName() string { return "file_objects" }

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
		Update("reference_count", gorm.Expr("reference_count + 1"))
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
		Update("reference_count", gorm.Expr("reference_count - 1")).Error; err != nil {
		return 0, err
	}
	var object Object
	if err := tx.First(&object, "id = ?", objectID).Error; err != nil {
		return 0, err
	}
	return object.ReferenceCount, nil
}
