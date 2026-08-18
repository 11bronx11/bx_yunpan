package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

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

	// healthcheck 子命令供 compose healthcheck 使用：镜像里没有
	// grpc_health_probe，用自己的二进制走 grpc.health.v1 探活。
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probeHealth(cfg.AIService.GRPCAddr); err != nil {
			slog.Error("aisvc health check failed", "error", err)
			os.Exit(1)
		}
		return
	}

	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", "aisvc")
	slog.SetDefault(logger)

	// 走 run 而不是直接 os.Exit：os.Exit 不执行 defer，会吞掉 trace flush。
	if err := run(cfg, logger); err != nil {
		logger.Error("aisvc stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Setup(ctx, cfg.Tracing, "bx-yunpan-aisvc")
	if err != nil {
		return err
	}
	// 用独立 context flush：此时 ctx 已被信号取消，复用它会让导出立刻失败。
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Warn("flush traces", "error", err)
		}
	}()

	return bootstrap.RunAISvc(ctx, cfg, logger)
}

func probeHealth(address string) error {
	target := address
	if len(target) > 0 && target[0] == ':' {
		target = "127.0.0.1" + target
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}
