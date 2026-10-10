package worker

import (
	"context"
	"gorm.io/gorm"
	"log/slog"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
)

const TaskSystemPing = "system:ping"
const QueueAI = "ai"

func BrokerConfig(cfg config.Config) taskqueue.Config {
	return taskqueue.Config{URL: cfg.TaskQueue.URL, Exchange: cfg.TaskQueue.Exchange, Prefetch: cfg.TaskQueue.Prefetch, Concurrency: cfg.Worker.Concurrency, PublishTimeout: cfg.TaskQueue.PublishTimeout, MaxRetry: cfg.Outbox.MaxRetry, TaskTimeout: cfg.Outbox.TaskTimeout, StopTimeout: cfg.TaskQueue.StopTimeout}
}
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, handlers map[string]taskqueue.HandlerFunc) error {
	return RunWithMetrics(ctx, cfg, logger, handlers, nil)
}
func RunWithMetrics(ctx context.Context, cfg config.Config, logger *slog.Logger, handlers map[string]taskqueue.HandlerFunc, metrics *Metrics, databases ...*gorm.DB) error {
	return RunQueuesWithMetrics(ctx, cfg, QueuesForRole(RoleCombined, cfg.Worker.Queues), logger, handlers, metrics, databases...)
}
func RunQueues(ctx context.Context, cfg config.Config, queues map[string]int, logger *slog.Logger, handlers map[string]taskqueue.HandlerFunc) error {
	return RunQueuesWithMetrics(ctx, cfg, queues, logger, handlers, nil)
}
func RunQueuesWithMetrics(ctx context.Context, cfg config.Config, queues map[string]int, logger *slog.Logger, handlers map[string]taskqueue.HandlerFunc, metrics *Metrics, databases ...*gorm.DB) error {
	ctx = outbox.WithV2EventsEnabled(ctx, cfg.Outbox.V2EventsEnabled)
	ctx = outbox.WithSearchEnabled(ctx, cfg.Search.Enabled)
	broker := taskqueue.New(BrokerConfig(cfg))
	defer broker.Close()
	var store *taskqueue.Store
	if len(databases) > 0 && databases[0] != nil {
		store = taskqueue.NewStore(databases[0]).WithStopTimeout(cfg.TaskQueue.StopTimeout)
		broker.FailureRecorder = store.RecordFailure
		broker.ExecutionStore = store
	}
	wrapped := make(map[string]taskqueue.HandlerFunc, len(handlers)+1)
	handlersCopy := make(map[string]taskqueue.HandlerFunc, len(handlers)+1)
	for kind, h := range handlers {
		handlersCopy[kind] = h
	}
	handlersCopy[TaskSystemPing] = func(context.Context, *taskqueue.Task) error { return nil }
	for kind, h := range handlersCopy {
		var next taskqueue.Handler = h
		if metrics != nil {
			next = metrics.Middleware()(next)
		}
		next = TracingMiddleware()(next)
		wrapped[kind] = next.ProcessTask
	}
	logger.Info("rabbitmq worker started", "concurrency", cfg.Worker.Concurrency, "queues", queues)
	return broker.Consume(ctx, queues, wrapped, logger)
}
