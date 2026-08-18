package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func RunWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()

	driveService := drive.NewService(deps.GORM)
	objects := objectstore.NewService(deps.GORM)
	quota := identity.NewService(deps.GORM, identity.NewTokenManager(cfg.Auth), driveService, cfg.Identity.DefaultQuotaBytes)
	uploads := upload.NewService(deps.GORM, deps.Storage, deps.Presigner, deps.Bucket, driveService, objects, quota, cfg.Upload)
	mediaService := media.NewService(deps.GORM, objects, deps.Storage, deps.Presigner, deps.Bucket, cfg.Storage.ReadURLTTL)
	redisOptions := asynq.RedisClientOpt{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB}
	queueClient := asynq.NewClient(redisOptions)
	defer func() { _ = queueClient.Close() }()
	dispatcher := outbox.NewDispatcher(deps.GORM, queueClient, logger, cfg.Outbox).
		WithLeaderLock(deps.Redis, cfg.Locking).
		WithAIEnabled(cfg.AI.Enabled)
	go dispatcher.Run(ctx)
	go runUploadCleanup(ctx, uploads, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)

	// AI 索引与重建索引已迁到 cmd/aisvc，这里只保留缩略图、对象校验、GC 与清理。
	handlers := workerHandlers(
		upload.VerifyHandler(uploads),
		media.Handler(mediaService),
		objectstore.GCHandler(objects, deps.Storage),
		logger,
	)
	return platformworker.Run(ctx, cfg, logger, handlers)
}

func runUploadCleanup(ctx context.Context, uploads *upload.Service, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		expired, err := uploads.Expire(ctx, batch)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "expire uploads", "error", err)
		} else if expired > 0 {
			logger.InfoContext(ctx, "expired uploads", "count", expired)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func workerHandlers(verify, ready, gc asynq.HandlerFunc, logger *slog.Logger) map[string]asynq.HandlerFunc {
	acknowledge := func(ctx context.Context, task *asynq.Task) error {
		logger.DebugContext(ctx, "domain event acknowledged", "event_type", task.Type())
		return nil
	}
	return map[string]asynq.HandlerFunc{
		outbox.EventFileCreated:           acknowledge,
		outbox.EventShareImported:         acknowledge,
		outbox.EventObjectVerifyRequested: verify,
		outbox.EventObjectReady:           ready,
		outbox.EventObjectGCRequested:     gc,
	}
}
