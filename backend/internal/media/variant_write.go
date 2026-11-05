package media

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errVariantUnneeded = errors.New("thumbnail superseded or object retired")

func (s *Service) writeVariant(ctx context.Context, object objectstore.Object, variant Variant, data []byte) error {
	var operation storageops.Operation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize registration with GC's ready -> deleting transition. The
		// transaction ends before any provider call starts.
		var current objectstore.Object
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ? AND status = 'ready' AND object_key = ?", object.ID, object.ObjectKey).First(&current).Error; err != nil {
			return err
		}
		var err error
		operation, err = storageops.Enqueue(tx, storageops.Input{
			OperationType: storageops.VariantDelete, IdempotencyKey: "variant.delete:" + variant.ID.String(),
			ObjectID: &object.ID, SourceBucket: s.bucket, SourceKey: variant.ObjectKey,
		})
		if err != nil {
			return err
		}
		claimed, token, err := storageops.TryStartWithToken(tx, operation.ID, 5*time.Minute)
		if err != nil {
			return err
		}
		if !claimed {
			return storageops.ErrLeaseLost
		}
		operation.LeaseToken = &token
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil // GC retired this generation before a write was registered.
	}
	if err != nil {
		return err
	}
	err = storageops.RunOwned(ctx, s.db, operation, 5*time.Minute, func(ctx context.Context) error {
		if _, err := s.storage.PutObject(ctx, s.bucket, variant.ObjectKey, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "image/jpeg"}); err != nil {
			return err
		}
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var owned storageops.Operation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND state = ? AND lease_token = ?", operation.ID, storageops.StateRunning, *operation.LeaseToken).First(&owned).Error; err != nil {
				return err
			}
			var current objectstore.Object
			if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
				Where("id = ? AND status = 'ready' AND object_key = ?", object.ID, object.ObjectKey).First(&current).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errVariantUnneeded
				}
				return err
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&variant)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return errVariantUnneeded
			}
			// Cancel compensation atomically with publication of the variant.
			return storageops.MarkSucceededOwned(tx, operation.ID, *operation.LeaseToken)
		})
	})
	if errors.Is(err, errVariantUnneeded) && ctx.Err() == nil {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return storageops.MarkFailedOwned(s.db.WithContext(persist), operation.ID, *operation.LeaseToken, err, time.Second)
	}
	if err != nil {
		return storageops.RecordError(ctx, s.db, operation, err)
	}
	return nil
}
