package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/11bronx11/bx_yunpan/backend/internal/bootstrap"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/logging"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", "worker")
	slog.SetDefault(logger)

	// 走 run 而不是直接 os.Exit：os.Exit 不执行 defer，会吞掉 trace flush。
	if err := run(cfg, logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Setup(ctx, cfg.Tracing, "bx-yunpan-worker")
	if err != nil {
		return err
	}
	// 用独立 context flush：此时 ctx 已被信号取消，复用它会让导出立刻失败。
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Warn("flush traces", "error", err)
		}
	}()

	return bootstrap.RunWorker(ctx, cfg, logger)
}
