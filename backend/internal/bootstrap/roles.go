package bootstrap

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dbmetrics"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
	"github.com/11bronx11/bx_yunpan/backend/internal/recovery"
	"github.com/11bronx11/bx_yunpan/backend/internal/search"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

// RunRole is the entry point for the P2 independently deployable roles.
func RunRole(ctx context.Context, cfg config.Config, logger *slog.Logger, role platformworker.Role) error {
	switch role {
	case platformworker.RoleRelay:
		return RunOutboxRelay(ctx, cfg, logger)
	case platformworker.RoleObject:
		return RunObjectWorker(ctx, cfg, logger)
	case platformworker.RoleMedia:
		return RunMediaWorker(ctx, cfg, logger)
	case platformworker.RoleMaintenance:
		return RunMaintenanceWorker(ctx, cfg, logger)
	case platformworker.RoleSearch:
		return RunSearchIndexer(ctx, cfg, logger)
	default:
		return RunWorker(ctx, cfg, logger)
	}
}

func RunSearchIndexer(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithSearchEnabled(ctx, true)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	ctx, background := newBackgroundGroup(ctx)
	defer background.Close()
	projector := search.NewProjector(deps.GORM, cfg.Search.IndexEpoch)
	if searchBackendConfigured(cfg.Search) {
		client := &http.Client{Timeout: cfg.Search.WriteTimeout}
		indexes := make([]search.Index, 0, 2)
		if cfg.Search.ESEndpoint != "" {
			indexes = append(indexes, search.NewElasticsearchIndex(cfg.Search.ESEndpoint, cfg.Search.ESUsername, cfg.Search.ESPassword, client))
		}
		if cfg.Search.QdrantEndpoint != "" {
			indexes = append(indexes, search.NewQdrantIndex(cfg.Search.QdrantEndpoint, cfg.Search.QdrantAPIKey, cfg.AI.Dimension, client))
		}
		projector.WithExternalIndex(search.NewExternalIndex(indexes...))
	}
	queues := platformworker.QueuesForRole(platformworker.RoleSearch, cfg.Worker.Queues)
	metricsRegistry := prometheus.NewRegistry()
	metrics := platformworker.NewMetrics(metricsRegistry, platformworker.RoleSearch, queues)
	dbmetrics.RegisterPool(metricsRegistry, deps.SQL, "search-indexer")
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger) })
	return platformworker.RunQueuesWithMetrics(ctx, cfg, queues, logger, map[string]taskqueue.HandlerFunc{
		outbox.TaskSearchProjectV1: projector.Handler,
	}, metrics, deps.GORM)
}

func searchBackendConfigured(cfg config.Search) bool {
	for _, backend := range cfg.WriteBackends {
		if backend == "es_qdrant" {
			return cfg.ESEndpoint != "" || cfg.QdrantEndpoint != ""
		}
	}
	return false
}

func RunOutboxRelay(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	ctx, background := newBackgroundGroup(ctx)
	defer background.Close()
	queueClient := taskqueue.New(platformworker.BrokerConfig(cfg))
	defer func() { background.Close(); _ = queueClient.Close() }()
	registry := prometheus.NewRegistry()
	metrics := outbox.NewMetrics(registry)
	dbmetrics.RegisterPool(registry, deps.SQL, "outbox-relay")
	registry.MustRegister(dbmetrics.NewOutboxCollector(deps.GORM))
	dispatcher := outbox.NewDispatcher(deps.GORM, queueClient, logger, cfg.Outbox).
		WithPublishTimeout(cfg.TaskQueue.PublishTimeout).
		WithLeaderLock(deps.RedisCoordination, cfg.Locking).
		WithMetrics(metrics).
		WithAIEnabled(cfg.AI.Enabled).WithSearchEnabled(cfg.Search.Enabled)
	background.Go(func() { dispatcher.Run(ctx) })
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, registry, logger) })
	<-ctx.Done()
	return nil
}

func RunObjectWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	ctx, background := newBackgroundGroup(ctx)
	defer background.Close()
	driveService := drive.NewService(deps.GORM)
	objects := objectstore.NewService(deps.GORM)
	quota := identity.NewService(deps.GORM, identity.NewTokenManager(cfg.Auth), driveService, cfg.Identity.DefaultQuotaBytes)
	uploads := upload.NewService(deps.GORM, deps.Storage, deps.Presigner, deps.Bucket, driveService, objects, quota, cfg.Upload)
	handlers := map[string]taskqueue.HandlerFunc{
		outbox.EventObjectVerifyRequested: upload.VerifyHandler(uploads),
		outbox.TaskObjectVerifyV1:         upload.VerifyHandler(uploads),
	}
	queues := platformworker.QueuesForRole(platformworker.RoleObject, cfg.Worker.Queues)
	metricsRegistry := prometheus.NewRegistry()
	metrics := platformworker.NewMetrics(metricsRegistry, platformworker.RoleObject, queues)
	dbmetrics.RegisterPool(metricsRegistry, deps.SQL, "object-worker")
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger) })
	return platformworker.RunQueuesWithMetrics(ctx, cfg, queues, logger, handlers, metrics, deps.GORM)
}

func RunMediaWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	ctx, background := newBackgroundGroup(ctx)
	defer background.Close()
	objects := objectstore.NewService(deps.GORM)
	mediaService := media.NewService(deps.GORM, objects, deps.Storage, deps.Presigner, deps.Bucket, cfg.Storage.ReadURLTTL)
	handler := media.Handler(mediaService)
	handlers := map[string]taskqueue.HandlerFunc{
		outbox.EventObjectReady:             handler,
		outbox.TaskMediaThumbnailV1:         handler,
		outbox.EventMediaThumbnailRequested: handler,
	}
	queues := platformworker.QueuesForRole(platformworker.RoleMedia, cfg.Worker.Queues)
	metricsRegistry := prometheus.NewRegistry()
	metrics := platformworker.NewMetrics(metricsRegistry, platformworker.RoleMedia, queues)
	dbmetrics.RegisterPool(metricsRegistry, deps.SQL, "media-worker")
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger) })
	return platformworker.RunQueuesWithMetrics(ctx, cfg, queues, logger, handlers, metrics, deps.GORM)
}

func RunMaintenanceWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
	ctx = outbox.WithSearchEnabled(ctx, cfg.Search.Enabled)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	ctx, background := newBackgroundGroup(ctx)
	defer background.Close()
	driveService := drive.NewService(deps.GORM)
	objects := objectstore.NewService(deps.GORM)
	quota := identity.NewService(deps.GORM, identity.NewTokenManager(cfg.Auth), driveService, cfg.Identity.DefaultQuotaBytes)
	uploads := upload.NewService(deps.GORM, deps.Storage, deps.Presigner, deps.Bucket, driveService, objects, quota, cfg.Upload)
	background.Go(func() { runUploadCleanup(ctx, uploads, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger) })
	background.Go(func() {
		runStorageReconciler(ctx, uploads, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	})
	background.Go(func() {
		runObjectDeleteReconciler(ctx, objects, deps.Storage, cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	})
	background.Go(func() {
		runRecoveryReconciler(ctx, recovery.New(deps.GORM, cfg.AI.Enabled), cfg.Upload.CleanupInterval, cfg.Upload.CleanupBatch, logger)
	})
	acknowledge := func(ctx context.Context, task *taskqueue.Task) error {
		logger.DebugContext(ctx, "maintenance task acknowledged", "task_type", task.Type())
		return nil
	}
	handler := objectstore.GCHandler(objects, deps.Storage)
	recoverOperation := func(ctx context.Context, task *taskqueue.Task) error {
		var payload eventcontract.StorageOperationRecoverV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.OperationID == uuid.Nil {
			return taskqueue.SkipRetry
		}
		if payload.OperationType == storageops.ObjectDelete {
			return objects.ReconcileObjectDeleteOperation(ctx, deps.Storage, payload.OperationID)
		}
		return uploads.ReconcileStorageOperation(ctx, payload.OperationID)
	}
	handlers := map[string]taskqueue.HandlerFunc{
		outbox.EventFileCreated:             acknowledge,
		outbox.EventShareImported:           acknowledge,
		outbox.EventObjectGCRequested:       handler,
		outbox.TaskObjectGCV1:               handler,
		outbox.EventStorageOperationRecover: recoverOperation,
	}
	queues := platformworker.QueuesForRole(platformworker.RoleMaintenance, cfg.Worker.Queues)
	metricsRegistry := prometheus.NewRegistry()
	metrics := platformworker.NewMetrics(metricsRegistry, platformworker.RoleMaintenance, queues)
	dbmetrics.RegisterPool(metricsRegistry, deps.SQL, "maintenance-worker")
	metricsRegistry.MustRegister(dbmetrics.NewStorageCollector(deps.GORM))
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger) })
	return platformworker.RunQueuesWithMetrics(ctx, cfg, queues, logger, handlers, metrics, deps.GORM)
}
