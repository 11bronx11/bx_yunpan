package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
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
	return errors.New("search-indexer requires an explicit ai-text-projector or ai-vector-projector role")
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
