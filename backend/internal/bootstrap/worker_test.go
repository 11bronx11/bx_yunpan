package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

// AI 任务已迁到 cmd/aisvc，事件覆盖要按两个进程的并集断言：任何一个 outbox
// 事件类型都必须恰好被 worker 或 aisvc 中的一个消费，不能重复也不能漏。
func TestWorkerAndAISvcHandlersCoverOutboxEvents(t *testing.T) {
	noop := func(context.Context, *taskqueue.Task) error { return nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	covered := map[string]bool{}
	for eventType := range workerHandlers(noop, noop, noop, logger) {
		covered[eventType] = true
	}
	// aiWorkerHandlers 只构造 map，不调用 service，故可传 nil。
	for eventType := range aiWorkerHandlers(nil, logger) {
		if covered[eventType] {
			t.Errorf("%s handled by both worker and aisvc", eventType)
		}
		covered[eventType] = true
	}

	for _, eventType := range outbox.EventTypes() {
		if !covered[eventType] {
			t.Errorf("no handler for %s", eventType)
		}
	}
}

func TestDomainEventsAreAcknowledged(t *testing.T) {
	noop := func(context.Context, *taskqueue.Task) error { return nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handlers := workerHandlers(noop, noop, noop, logger)

	for _, eventType := range []string{outbox.EventFileCreated, outbox.EventShareImported} {
		if err := handlers[eventType](context.Background(), taskqueue.NewTask(eventType, nil)); err != nil {
			t.Errorf("acknowledge %s: %v", eventType, err)
		}
	}
}

func TestMaintenanceRecoveryDefersLegacyUploadOperations(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := maintenanceRecoveryHandler(nil, nil, logger)
	payload, err := json.Marshal(eventcontract.StorageOperationRecoverV1{
		OperationID: uuid.New(), OperationType: storageops.MultipartAbort, RecoveryKey: "legacy-upload-recovery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), taskqueue.NewTask(outbox.EventStorageOperationRecover, payload)); err != nil {
		t.Fatalf("legacy upload task should be acknowledged for maintenance scan recovery: %v", err)
	}
}

func TestMaintenanceRecoveryRejectsUnknownOperations(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := maintenanceRecoveryHandler(nil, nil, logger)
	payload, err := json.Marshal(eventcontract.StorageOperationRecoverV1{
		OperationID: uuid.New(), OperationType: "unknown.operation", RecoveryKey: "unknown-recovery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), taskqueue.NewTask(outbox.EventStorageOperationRecover, payload)); !errors.Is(err, taskqueue.SkipRetry) {
		t.Fatalf("unknown operation error = %v, want SkipRetry", err)
	}
}
