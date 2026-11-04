package bootstrap

import (
	"context"
	"log/slog"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func configureOutboxWakeups(ctx context.Context, cfg config.Config, dispatcher *outbox.Dispatcher, logger *slog.Logger) func() {
	if !cfg.Outbox.NotifyEnabled {
		return func() {}
	}
	listener := outbox.NewPostgresWakeupListener(ctx, cfg.Postgres.PrimaryDSN, logger)
	if listener == nil {
		return func() {}
	}
	dispatcher.WithWakeups(listener.Notifications())
	return func() {
		if err := listener.Close(); err != nil && logger != nil {
			logger.Warn("close outbox PostgreSQL notification listener", "error", err)
		}
	}
}
