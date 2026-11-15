package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dbmetrics"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
	"github.com/11bronx11/bx_yunpan/backend/internal/search"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

// RunRole is the entry point for the P2 independently deployable roles.
func RunRole(ctx context.Context, cfg config.Config, logger *slog.Logger, role platformworker.Role) error {
	switch role {
	case platformworker.RoleRelay:
		return RunAsync(ctx, cfg, logger, "async-control")
	case platformworker.RoleObject:
		return RunAsync(ctx, cfg, logger, "object-worker")
	case platformworker.RoleMedia:
		return RunAsync(ctx, cfg, logger, "media-worker")
	case platformworker.RoleMaintenance:
		return RunAsync(ctx, cfg, logger, "gc-worker")
	case platformworker.RoleUpload:
		return RunAsync(ctx, cfg, logger, "upload-worker")
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
		external := search.NewExternalIndex(indexes...)
		if err := external.(search.EpochPreparer).PrepareEpoch(ctx, cfg.Search.IndexEpoch, int64(cfg.AI.Dimension)); err != nil {
			return fmt.Errorf("prepare search target: %w", err)
		}
		projector.WithExternalIndex(external)
	}
	queues := platformworker.QueuesForRole(platformworker.RoleSearch, cfg.Worker.Queues)
	metricsRegistry := prometheus.NewRegistry()
	_ = platformworker.NewMetrics(metricsRegistry, platformworker.RoleSearch, queues)
	dbmetrics.RegisterPool(metricsRegistry, deps.SQL, "search-indexer")
	background.Go(func() { serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger) })
	return runAsyncAux(ctx, cfg, "search", logger, func(ctx context.Context, t asynccontrol.Task, in asyncprotocol.Input) error {
		return projector.Handler(ctx, &taskqueue.Task{ID: t.ID.String(), Kind: outbox.TaskSearchProjectV1, Queue: "search", Body: in.OriginalPayload})
	})
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
	return RunAsync(ctx, cfg, logger, "async-control")
}

func RunObjectWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return RunAsync(ctx, cfg, logger, "object-worker")
}

func RunMediaWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return RunAsync(ctx, cfg, logger, "media-worker")
}

func RunMaintenanceWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return RunAsync(ctx, cfg, logger, "gc-worker")
}

func maintenanceRecoveryHandler(objects *objectstore.Service, storage objectstore.GCStorage, logger *slog.Logger) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload eventcontract.StorageOperationRecoverV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.OperationID == uuid.Nil {
			return taskqueue.SkipRetry
		}
		switch payload.OperationType {
		case storageops.ObjectDelete, storageops.VariantDelete:
			return storageRecoveryResult(objects.ReconcileObjectDeleteOperation(ctx, storage, payload.OperationID))
		case storageops.MultipartCreate, storageops.MultipartComplete, storageops.MultipartAbort, storageops.CanonicalEnsure, storageops.StagingDelete:
			// Legacy upload nudges are acknowledged here; the durable operation
			// scanner re-emits them to the upload queue without running upload I/O
			// in the maintenance role.
			logger.InfoContext(ctx, "legacy upload recovery acknowledged for maintenance scan requeue", "operation_id", payload.OperationID, "operation_type", payload.OperationType)
			return nil
		default:
			return taskqueue.SkipRetry
		}
	}
}

func RunUploadWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return RunAsync(ctx, cfg, logger, "upload-worker")
}

func storageRecoveryResult(err error) error {
	if errors.Is(err, storageops.ErrExhausted) {
		return errors.Join(taskqueue.SkipRetry, err)
	}
	return err
}

func uploadRecoveryHandler(uploads *upload.Service) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload eventcontract.UploadOperationRecoverV1
		if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.OperationID == uuid.Nil {
			return taskqueue.SkipRetry
		}
		switch payload.OperationType {
		case storageops.MultipartCreate, storageops.MultipartComplete, storageops.MultipartAbort, storageops.CanonicalEnsure, storageops.StagingDelete:
			return storageRecoveryResult(uploads.ReconcileStorageOperation(ctx, payload.OperationID))
		default:
			return taskqueue.SkipRetry
		}
	}
}
