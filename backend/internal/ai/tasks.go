package ai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func ObjectReadyHandler(service *Service, logger *slog.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.ObjectID == uuid.Nil {
			return asynq.SkipRetry
		}
		startedAt := time.Now()
		logger.InfoContext(ctx, "AI indexing started", "object_id", payload.ObjectID)
		err := service.ProcessObject(ctx, payload.ObjectID)
		if err != nil {
			logger.ErrorContext(ctx, "AI indexing failed", "object_id", payload.ObjectID, "duration", time.Since(startedAt), "error", err)
		} else {
			logger.InfoContext(ctx, "AI indexing completed", "object_id", payload.ObjectID, "duration", time.Since(startedAt))
		}
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return asynq.SkipRetry
		}
		return err
	}
}

func ReprocessHandler(service *Service, logger *slog.Logger) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			TaskID   uuid.UUID `json:"task_id"`
			ObjectID uuid.UUID `json:"object_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.TaskID == uuid.Nil || payload.ObjectID == uuid.Nil {
			return asynq.SkipRetry
		}
		startedAt := time.Now()
		logger.InfoContext(ctx, "AI reprocessing started", "task_id", payload.TaskID, "object_id", payload.ObjectID)
		err := service.ProcessTask(ctx, payload.TaskID, payload.ObjectID)
		if err != nil {
			logger.ErrorContext(ctx, "AI reprocessing failed", "task_id", payload.TaskID, "object_id", payload.ObjectID, "duration", time.Since(startedAt), "error", err)
		} else {
			logger.InfoContext(ctx, "AI reprocessing completed", "task_id", payload.TaskID, "object_id", payload.ObjectID, "duration", time.Since(startedAt))
		}
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return asynq.SkipRetry
		}
		return err
	}
}
