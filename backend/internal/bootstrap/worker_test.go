package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

// AI 任务已迁到 cmd/aisvc，事件覆盖要按两个进程的并集断言：任何一个 outbox
// 事件类型都必须恰好被 worker 或 aisvc 中的一个消费，不能重复也不能漏。
func TestWorkerAndAISvcHandlersCoverOutboxEvents(t *testing.T) {
	noop := func(context.Context, *asynq.Task) error { return nil }
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
	noop := func(context.Context, *asynq.Task) error { return nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handlers := workerHandlers(noop, noop, noop, logger)

	for _, eventType := range []string{outbox.EventFileCreated, outbox.EventShareImported} {
		if err := handlers[eventType](context.Background(), asynq.NewTask(eventType, nil)); err != nil {
			t.Errorf("acknowledge %s: %v", eventType, err)
		}
	}
}
