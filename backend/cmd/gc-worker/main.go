package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/11bronx11/bx_yunpan/backend/internal/bootstrap"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func main() {
	cfg, err := config.LoadForRole("gc-worker")
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.RunAsync(ctx, cfg, slog.Default(), "gc-worker"); err != nil && ctx.Err() == nil {
		slog.Error("async process stopped", "error", err)
		os.Exit(1)
	}
}
