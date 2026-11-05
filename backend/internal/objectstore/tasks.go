package objectstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func GCHandler(service *Service, storage *minio.Client) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return taskqueue.SkipRetry
		}
		err := service.GarbageCollect(ctx, storage, payload.ObjectID)
		if errors.Is(err, storageops.ErrExhausted) {
			return errors.Join(taskqueue.SkipRetry, err)
		}
		return err
	}
}
