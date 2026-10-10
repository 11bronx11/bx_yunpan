package ai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
)

func ObjectReadyHandler(service *Service, logger *slog.Logger) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload eventcontract.AIIndexRequestedV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.ObjectID == uuid.Nil {
			return taskqueue.SkipRetry
		}
		startedAt := time.Now()
		logger.InfoContext(ctx, "AI indexing started", "object_id", payload.ObjectID)
		err := service.processObjectWithMetadata(ctx, payload.ObjectID, false, IndexMetadata{RunID: payload.RunID, ContentHash: payload.ContentHash, DocumentVersionID: payload.DocumentVersionID, PipelineVersion: payload.PipelineVersion, ModelVersion: payload.ModelVersion, ParserProfileVersion: payload.ParserProfileVersion, TraceID: payload.TraceID})
		if err != nil {
			logger.ErrorContext(ctx, "AI indexing failed", "object_id", payload.ObjectID, "duration", time.Since(startedAt), "error", err)
		} else {
			logger.InfoContext(ctx, "AI indexing completed", "object_id", payload.ObjectID, "duration", time.Since(startedAt))
		}
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return taskqueue.SkipRetry
		}
		return err
	}
}

func ReprocessHandler(service *Service, logger *slog.Logger) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload eventcontract.AIReprocessRequestedV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.TaskID == uuid.Nil || payload.ObjectID == uuid.Nil {
			return taskqueue.SkipRetry
		}
		startedAt := time.Now()
		logger.InfoContext(ctx, "AI reprocessing started", "task_id", payload.TaskID, "object_id", payload.ObjectID)
		err := service.processTaskWithMetadata(ctx, payload.TaskID, payload.ObjectID, IndexMetadata{RunID: payload.RunID, ContentHash: payload.ContentHash, DocumentVersionID: payload.DocumentVersionID, PipelineVersion: payload.PipelineVersion, ModelVersion: payload.ModelVersion, ParserProfileVersion: payload.ParserProfileVersion, TraceID: payload.TraceID})
		if err != nil {
			logger.ErrorContext(ctx, "AI reprocessing failed", "task_id", payload.TaskID, "object_id", payload.ObjectID, "duration", time.Since(startedAt), "error", err)
		} else {
			logger.InfoContext(ctx, "AI reprocessing completed", "task_id", payload.TaskID, "object_id", payload.ObjectID, "duration", time.Since(startedAt))
		}
		if errors.Is(err, errProviderQuota) || errors.Is(err, errProviderPermanent) {
			return taskqueue.SkipRetry
		}
		return err
	}
}
