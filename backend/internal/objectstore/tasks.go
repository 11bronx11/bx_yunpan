package objectstore

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/minio/minio-go/v7"
)

func GCHandler(service *Service, storage *minio.Client) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return asynq.SkipRetry
		}
		return service.GarbageCollect(ctx, storage, payload.ObjectID)
	}
}
