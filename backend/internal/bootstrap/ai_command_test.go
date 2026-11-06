package bootstrap

import (
	"context"
	"errors"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/google/uuid"
)

type commandRecorder struct {
	calls int
	task  ai.Task
}

func (c *commandRecorder) RequestReprocess(context.Context, uuid.UUID, uuid.UUID, string) (ai.Task, error) {
	c.calls++
	return c.task, nil
}

type commandLimiterStub struct {
	allowed bool
	err     error
	owner   uuid.UUID
	scope   string
}

func (l *commandLimiterStub) Allow(_ context.Context, owner uuid.UUID, scope string) (bool, error) {
	l.owner, l.scope = owner, scope
	return l.allowed, l.err
}

func TestLocalAICommandPreservesRateLimitWithoutQueryDependency(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limiter *commandLimiterStub
		denied  bool
	}{
		{name: "disabled"},
		{name: "allowed", limiter: &commandLimiterStub{allowed: true}},
		{name: "shared budget exhausted", limiter: &commandLimiterStub{}, denied: true},
		{name: "Redis unavailable preserves fail open", limiter: &commandLimiterStub{err: errors.New("Redis unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := uuid.New()
			command := &commandRecorder{task: ai.Task{ID: uuid.New()}}
			backend := &splitAIBackend{command: command}
			if tc.limiter != nil {
				backend.limiter = tc.limiter
			}
			got, err := backend.RequestReprocess(context.Background(), owner, uuid.New(), "replay-key")
			if tc.denied {
				if !errors.Is(err, ai.ErrRateLimited) || command.calls != 0 {
					t.Fatalf("exhausted budget dispatched command: calls=%d err=%v", command.calls, err)
				}
			} else if err != nil || got.ID != command.task.ID || command.calls != 1 {
				t.Fatalf("local command depends on unavailable query service: calls=%d task=%s err=%v", command.calls, got.ID, err)
			}
			if tc.limiter != nil && (tc.limiter.owner != owner || tc.limiter.scope != "reprocess") {
				t.Fatalf("wrong shared budget identity: owner=%s scope=%s", tc.limiter.owner, tc.limiter.scope)
			}
		})
	}
}
