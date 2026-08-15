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
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", "api")
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.RunAPI(ctx, cfg, logger); err != nil {
		logger.Error("api stopped", "error", err)
		os.Exit(1)
	}
}
