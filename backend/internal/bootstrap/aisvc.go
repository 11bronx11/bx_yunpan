package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/grpcserver"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
)

// RunAISvc 在同一进程内跑 gRPC server 与 AI 专属 Asynq worker。
//
// AI 模块是唯一有真实独立扩容理由的模块：抽取、OCR、Embedding 都是 CPU/IO
// 重活，与 API 的请求特征不同，混在一个进程里会互相挤资源。
func RunAISvc(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()

	driveService := drive.NewService(deps.GORM)
	objects := objectstore.NewService(deps.GORM)
	aiService := ai.NewService(deps.GORM, driveService, objects, deps.Storage, cfg.AI)

	// 限流下移到 aisvc：放 API 侧的话多副本各自计数，总量会是副本数的倍数，
	// 对下游 LLM 配额没有保护作用。
	var limiter ai.RequestLimiter
	if cfg.AI.RateLimitEnabled {
		limiter = ai.NewRequestLimiter(deps.Redis, ai.RateLimits{
			SearchPerMinute:    cfg.AI.RateLimitSearchPerMinute,
			AskPerMinute:       cfg.AI.RateLimitAskPerMinute,
			ReprocessPerMinute: cfg.AI.RateLimitReprocessPerMinute,
		})
	}

	registry := prometheus.NewRegistry()
	server := grpc.NewServer(grpcx.ServerInterceptors(grpcx.ServerOptions{
		Logger:  logger,
		Metrics: grpcx.NewMetrics(registry, "server"),
		Mapper:  ai.GRPCStatus,
	})...)
	pb.RegisterAIServiceServer(server, grpcserver.New(aiService, limiter))

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus(pb.AIService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	listener, err := net.Listen("tcp", cfg.AIService.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("aisvc grpc listening", "address", cfg.AIService.GRPCAddr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serverErr <- err
		}
	}()

	workerErr := make(chan error, 1)
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	aiQueues := map[string]int{platformworker.QueueAI: cfg.Worker.Queues[platformworker.QueueAI]}
	go func() {
		workerErr <- platformworker.RunQueues(workerCtx, cfg, aiQueues, logger, aiWorkerHandlers(aiService, logger))
	}()

	select {
	case <-ctx.Done():
		logger.Info("aisvc shutdown requested")
	case err := <-serverErr:
		return fmt.Errorf("serve grpc: %w", err)
	case err := <-workerErr:
		if err != nil {
			return fmt.Errorf("run ai worker: %w", err)
		}
	}

	healthServer.SetServingStatus(pb.AIService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	server.GracefulStop()
	stopWorker()
	return nil
}

// aiWorkerHandlers 只消费 ai:* 任务：索引与重建索引。
// 缩略图、对象校验、GC 与清理留在 cmd/worker。
func aiWorkerHandlers(service *ai.Service, logger *slog.Logger) map[string]asynq.HandlerFunc {
	return map[string]asynq.HandlerFunc{
		outbox.EventAIIndexRequested:     ai.ObjectReadyHandler(service, logger),
		outbox.EventAIReprocessRequested: ai.ReprocessHandler(service, logger),
	}
}
