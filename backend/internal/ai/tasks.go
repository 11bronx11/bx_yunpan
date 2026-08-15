package ai

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func ObjectReadyHandler(service *Service) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.ObjectID == uuid.Nil {
			return asynq.SkipRetry
		}
		err := service.ProcessObject(ctx, payload.ObjectID)
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return asynq.SkipRetry
		}
		return err
	}
}

func ReprocessHandler(service *Service) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			TaskID   uuid.UUID `json:"task_id"`
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.TaskID == uuid.Nil || payload.ObjectID == uuid.Nil {
			return asynq.SkipRetry
		}
		err := service.ProcessTask(ctx, payload.TaskID, payload.ObjectID)
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return asynq.SkipRetry
		}
		return err
	}
}
