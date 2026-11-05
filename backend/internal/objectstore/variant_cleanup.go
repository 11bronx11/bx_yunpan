package objectstore

import (
	"context"

	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/minio/minio-go/v7"
)

// A writer creates this compensation before PutObject and owns its renewable
// lease until the variant commits. Every write has its own key. Once cleanup
// owns the operation, the former writer cannot commit that key as a variant.
func (s *Service) performVariantDelete(ctx context.Context, storage GCStorage, operation storageops.Operation) error {
	if operation.ObjectID == nil || operation.SourceBucket == "" || operation.SourceKey == "" {
		return storageops.ErrInvalidOperation
	}
	var adopted int64
	if err := s.db.WithContext(ctx).Table("object_variants").
		Where("object_id = ? AND object_key = ? AND status = 'ready'", *operation.ObjectID, operation.SourceKey).
		Count(&adopted).Error; err != nil {
		return err
	}
	if adopted == 0 {
		if err := storage.RemoveObject(ctx, operation.SourceBucket, operation.SourceKey, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return storageops.MarkSucceededOwned(s.db.WithContext(ctx), operation.ID, operationLeaseToken(operation))
}
