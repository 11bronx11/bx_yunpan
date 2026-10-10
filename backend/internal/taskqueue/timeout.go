package taskqueue

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// A Go handler cannot be forcibly stopped in isolation. Never detach a stuck
// handler and let another task race its side effects. Exit the worker process
// with its delivery unacknowledged; the supervisor can safely restart it.
func invokeBounded(ctx context.Context, t *Task, h HandlerFunc, grace time.Duration, beforeExit ...func()) error {
	if grace <= 0 {
		grace = 10 * time.Second
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	done := make(chan error, 1)
	go func() { done <- invoke(ctx, t, h) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		return context.Cause(ctx)
	case <-timer.C:
		for _, record := range beforeExit {
			record()
		}
		slog.Error("task handler ignored cancellation; terminating worker", "task_id", t.ID, "task_type", t.Kind, "grace", grace)
		os.Exit(70)
		return context.Cause(ctx)
	}
}
