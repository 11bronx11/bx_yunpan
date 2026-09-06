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
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", string(platformworker.RoleSearch), "instance_id", cfg.App.InstanceID)
	if err := run(cfg, logger); err != nil {
		logger.Error("search indexer stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown, err := tracing.Setup(ctx, cfg.Tracing, "bx-yunpan-search-indexer")
	if err != nil {
		return err
	}
	defer func() { _ = shutdown(context.Background()) }()
	return bootstrap.RunSearchIndexer(ctx, cfg, logger)
}
