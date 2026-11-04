package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/bootstrap"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/logging"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probeHealth(cfg.Parser.GRPCAddr, pb.DocumentParserService_ServiceDesc.ServiceName); err != nil {
			slog.Error("document parser health check failed", "error", err)
			os.Exit(1)
		}
		return
	}

	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", "parser-svc", "instance_id", cfg.App.InstanceID)
	slog.SetDefault(logger)
	if err := run(cfg, logger); err != nil {
		logger.Error("document parser stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Setup(ctx, cfg.Tracing, "bx-yunpan-parser-svc")
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Warn("flush traces", "error", err)
		}
	}()
	return bootstrap.RunParserSvc(ctx, cfg, logger)
}

func probeHealth(address, service string) error {
	if len(address) > 0 && address[0] == ':' {
		address = "127.0.0.1" + address
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
	return err
}
