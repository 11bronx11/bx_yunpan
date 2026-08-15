package worker

import (
	"context"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

const TaskSystemPing = "system:ping"

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, handlers map[string]asynq.HandlerFunc) error {
	server := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		},
		asynq.Config{
			Concurrency: cfg.Worker.Concurrency,
			Queues:      cfg.Worker.Queues,
			Logger:      asynqLogger{logger: logger},
		},
	)
	mux := asynq.NewServeMux()
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
	logger.Info("worker started", "concurrency", cfg.Worker.Concurrency, "queues", cfg.Worker.Queues)
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
