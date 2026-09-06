package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func serveWorkerMetrics(ctx context.Context, addr string, registry *prometheus.Registry, logger *slog.Logger) {
	if addr == "" || registry == nil {
		return
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "worker metrics listening", "address", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			result <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.WarnContext(ctx, "shutdown worker metrics", "error", err)
		}
	case err := <-result:
		if err != nil {
			logger.ErrorContext(ctx, "worker metrics stopped", "error", fmt.Errorf("serve metrics: %w", err))
		}
	}
}
