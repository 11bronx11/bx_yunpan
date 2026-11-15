package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/recovery"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func runRecoveryReconciler(ctx context.Context, reconciler *recovery.Reconciler, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		scan, stop := context.WithTimeout(ctx, 30*time.Second)
		report, err := reconciler.RunOnce(scan, batch)
		stop()
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
		scan, stop := context.WithTimeout(ctx, 30*time.Second)
		retired, retireErr := uploads.RetireStoppedSessions(scan, batch)
		stop()
		if retireErr != nil && !errors.Is(retireErr, context.Canceled) {
			logger.ErrorContext(ctx, "retire stopped uploads", "error", retireErr, "retired", retired)
		} else if retired > 0 {
			logger.ErrorContext(ctx, "uploads stopped after retry exhaustion", "retired", retired)
		}
		scan, stop = context.WithTimeout(ctx, 30*time.Second)
		expired, err := uploads.Expire(scan, batch)
		stop()
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

func runCanonicalOrphanReconciler(ctx context.Context, uploads *upload.Service, interval time.Duration, batch int, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		scan, stop := context.WithTimeout(ctx, 30*time.Second)
		count, err := uploads.ReconcileCanonicalOrphans(scan, batch)
		stop()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "reconcile orphan canonical objects", "error", err, "repaired", count)
		} else if count > 0 {
			logger.InfoContext(ctx, "reconciled orphan canonical objects", "repaired", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func workerHandlers(verify, ready, gc taskqueue.HandlerFunc, logger *slog.Logger) map[string]taskqueue.HandlerFunc {
	return workerHandlersWithRecovery(verify, ready, gc, nil, logger)
}

func workerHandlersWithRecovery(verify, ready, gc, recoverOperation taskqueue.HandlerFunc, logger *slog.Logger) map[string]taskqueue.HandlerFunc {
	acknowledge := func(ctx context.Context, task *taskqueue.Task) error {
		logger.DebugContext(ctx, "domain event acknowledged", "event_type", task.Type())
		return nil
	}
	handlers := map[string]taskqueue.HandlerFunc{
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
	handlers[outbox.TaskUploadOperationRecoverV1] = recoverOperation
	return handlers
}

// Legacy command alias owns only the control role; no combined compute/scans.
func RunWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return RunAsync(ctx, cfg, logger, "async-control")
}
