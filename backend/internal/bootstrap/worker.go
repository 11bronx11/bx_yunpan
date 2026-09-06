package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
	"github.com/11bronx11/bx_yunpan/backend/internal/recovery"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func RunWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
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
	redisOptions := asynq.RedisClientOpt{Addr: cfg.Redis.BrokerAddr, Password: cfg.Redis.BrokerPassword, DB: cfg.Redis.BrokerDB}
	queueClient := asynq.NewClient(redisOptions)
	defer func() { _ = queueClient.Close() }()
	metricsRegistry := prometheus.NewRegistry()
	metrics := outbox.NewMetrics(metricsRegistry)
	workerMetrics := platformworker.NewMetrics(metricsRegistry, platformworker.RoleCombined, platformworker.QueuesForRole(platformworker.RoleCombined, cfg.Worker.Queues), deps.RedisBroker)
	dispatcher := outbox.NewDispatcher(deps.GORM, queueClient, logger, cfg.Outbox).
		WithLeaderLock(deps.RedisCoordination, cfg.Locking).
		WithMetrics(metrics).
		WithAIEnabled(cfg.AI.Enabled).WithSearchEnabled(cfg.Search.Enabled)
	go dispatcher.Run(ctx)
	go serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger)
	go runUploadCleanup(ctx, uploads, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	go runStorageReconciler(ctx, uploads, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	go runObjectDeleteReconciler(ctx, objects, deps.Storage, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	go runRecoveryReconciler(ctx, recovery.New(deps.GORM, cfg.AI.Enabled), cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)

	recoverOperation := func(ctx context.Context, task *asynq.Task) error {
		var payload eventcontract.StorageOperationRecoverV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.OperationID == uuid.Nil {
			return asynq.SkipRetry
		}
		if payload.OperationType == storageops.ObjectDelete {
			return objects.ReconcileObjectDeleteOperation(ctx, deps.Storage, payload.OperationID)
		}
		return uploads.ReconcileStorageOperation(ctx, payload.OperationID)
	}
	// AI 索引与重建索引已迁到 cmd/aisvc，这里只保留缩略图、对象校验、GC 与清理。
	handlers := workerHandlersWithRecovery(
		upload.VerifyHandler(uploads),
		media.Handler(mediaService),
		objectstore.GCHandler(objects, deps.Storage),
		recoverOperation,
		logger,
	)
	return platformworker.RunWithMetrics(ctx, cfg, logger, handlers, workerMetrics)
}

func runRecoveryReconciler(ctx context.Context, reconciler *recovery.Reconciler, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := reconciler.RunOnce(ctx, batch)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "reconcile broker-lost work", "error", err, "repaired", report.Repaired)
		} else if report.Repaired > 0 {
			logger.InfoContext(ctx, "repaired broker-lost work", "repaired", report.Repaired, "by_kind", report.ByKind)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runStorageReconciler(ctx context.Context, uploads *upload.Service, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		count, err := uploads.ReconcileStorageOperations(ctx, batch)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "reconcile storage operations", "error", err, "completed", count)
		} else if count > 0 {
			logger.InfoContext(ctx, "reconciled storage operations", "completed", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runObjectDeleteReconciler(ctx context.Context, objects *objectstore.Service, storage objectstore.GCStorage, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		count, err := objects.ReconcileObjectDeletes(ctx, storage, batch)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "reconcile object deletes", "error", err, "completed", count)
		} else if count > 0 {
			logger.InfoContext(ctx, "reconciled object deletes", "completed", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
	return workerHandlersWithRecovery(verify, ready, gc, nil, logger)
}

func workerHandlersWithRecovery(verify, ready, gc, recoverOperation asynq.HandlerFunc, logger *slog.Logger) map[string]asynq.HandlerFunc {
	acknowledge := func(ctx context.Context, task *asynq.Task) error {
		logger.DebugContext(ctx, "domain event acknowledged", "event_type", task.Type())
		return nil
	}
	handlers := map[string]asynq.HandlerFunc{
		outbox.EventFileCreated:             acknowledge,
		outbox.EventShareImported:           acknowledge,
		outbox.EventObjectVerifyRequested:   verify,
		outbox.TaskObjectVerifyV1:           verify,
		outbox.EventObjectReady:             ready,
		outbox.TaskMediaThumbnailV1:         ready,
		outbox.EventMediaThumbnailRequested: ready,
		outbox.EventObjectGCRequested:       gc,
		outbox.TaskObjectGCV1:               gc,
	}
	if recoverOperation == nil {
		recoverOperation = acknowledge
	}
	handlers[outbox.EventStorageOperationRecover] = recoverOperation
	return handlers
}
