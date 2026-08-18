package worker

import (
	"context"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

const TaskSystemPing = "system:ping"

// QueueAI 由 aisvc 独占消费，其余队列留给 cmd/worker。
const QueueAI = "ai"

// Run 消费 cfg.Worker.Queues 里除 ai 之外的队列。
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, handlers map[string]asynq.HandlerFunc) error {
	queues := make(map[string]int, len(cfg.Worker.Queues))
	for name, weight := range cfg.Worker.Queues {
		if name == QueueAI {
			continue
		}
		queues[name] = weight
	}
	return run(ctx, cfg, queues, logger, handlers)
}

// RunQueues 只消费指定队列，供 aisvc 独占 ai 队列使用。
// 进程只能消费自己注册了 handler 的队列，否则 Asynq 取到任务却找不到
// handler，任务会一路重试到进死信队列。
func RunQueues(ctx context.Context, cfg config.Config, queues map[string]int, logger *slog.Logger, handlers map[string]asynq.HandlerFunc) error {
	return run(ctx, cfg, queues, logger, handlers)
}

func run(ctx context.Context, cfg config.Config, queues map[string]int, logger *slog.Logger, handlers map[string]asynq.HandlerFunc) error {
	server := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		},
		asynq.Config{
			Concurrency: cfg.Worker.Concurrency,
			Queues:      queues,
			Logger:      asynqLogger{logger: logger},
		},
	)
	mux := asynq.NewServeMux()
	mux.Use(TracingMiddleware())
	mux.HandleFunc(TaskSystemPing, func(ctx context.Context, _ *asynq.Task) error {
		logger.DebugContext(ctx, "worker ping handled")
		return nil
	})
	for taskType, handler := range handlers {
		mux.HandleFunc(taskType, handler)
	}

	if err := server.Start(mux); err != nil {
		return err
	}
	logger.Info("worker started", "concurrency", cfg.Worker.Concurrency, "queues", queues)
	<-ctx.Done()
	logger.Info("worker shutdown requested")
	server.Shutdown()
	return nil
}

type asynqLogger struct {
	logger *slog.Logger
}

func (l asynqLogger) Debug(args ...any) { l.logger.Debug("asynq", "message", args) }
func (l asynqLogger) Info(args ...any)  { l.logger.Info("asynq", "message", args) }
func (l asynqLogger) Warn(args ...any)  { l.logger.Warn("asynq", "message", args) }
func (l asynqLogger) Error(args ...any) { l.logger.Error("asynq", "message", args) }
func (l asynqLogger) Fatal(args ...any) { l.logger.Error("asynq fatal", "message", args) }
