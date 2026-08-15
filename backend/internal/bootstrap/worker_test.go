package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

func TestWorkerHandlersCoverOutboxEvents(t *testing.T) {
	noop := func(context.Context, *asynq.Task) error { return nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handlers := workerHandlers(noop, noop, noop, noop, logger)

	for _, eventType := range outbox.EventTypes() {
		if handlers[eventType] == nil {
			t.Errorf("no worker handler for %s", eventType)
		}
	}
}

func TestDomainEventsAreAcknowledged(t *testing.T) {
	noop := func(context.Context, *asynq.Task) error { return nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handlers := workerHandlers(noop, noop, noop, noop, logger)

	for _, eventType := range []string{outbox.EventFileCreated, outbox.EventShareImported} {
		if err := handlers[eventType](context.Background(), asynq.NewTask(eventType, nil)); err != nil {
			t.Errorf("acknowledge %s: %v", eventType, err)
		}
	}
}
