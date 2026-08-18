package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
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
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/registry"
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

	// 注册放在 Serve 之后：先能接请求再对外宣告，避免被发现了却还没监听。
	deregister, err := registerAISvc(ctx, cfg, logger, listener)
	if err != nil {
		server.Stop()
		return err
	}
	defer deregister()

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
	// 先撤销注册再 GracefulStop：让调用方先把本实例从地址列表里摘掉，
	// 再等在途请求跑完，否则停机窗口里还会有新请求打进来。
	deregister()
	server.GracefulStop()
	stopWorker()
	return nil
}

// registerAISvc 把本实例注册到 etcd，返回幂等的撤销函数。
// 未启用 etcd 时返回空操作，让单副本部署无需额外依赖。
func registerAISvc(ctx context.Context, cfg config.Config, logger *slog.Logger, listener net.Listener) (func(), error) {
	noop := func() {}
	client, err := registry.Open(cfg.Registry)
	if err != nil {
		return noop, err
	}
	if client == nil {
		return noop, nil
	}

	address := cfg.Registry.AdvertiseAddr
	if address == "" {
		// 未显式配置时用监听地址推断：容器内 hostname 是容器 ID，
		// 同一 compose 网络里其他服务可以解析到它。
		address, err = advertiseAddress(listener)
		if err != nil {
			_ = client.Close()
			return noop, err
		}
	}

	instanceID := uuid.Must(uuid.NewV7()).String()
	registrar := registry.NewRegistrar(client, logger,
		cfg.Registry.Prefix+"/"+instanceID, address, cfg.Registry.LeaseTTL)
	if err := registrar.Register(ctx); err != nil {
		_ = client.Close()
		return noop, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			// 用独立 context：停机时 ctx 已取消，复用它 Revoke 发不出去，
			// 实例就得等一个 TTL 才被摘除。
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			if err := registrar.Deregister(revokeCtx); err != nil {
				logger.WarnContext(ctx, "deregister aisvc", "error", err)
			}
			_ = client.Close()
		})
	}, nil
}

// advertiseAddress 把监听地址补成对端可 dial 的形式：":8082" 这种
// 只有端口的监听串对调用方没有意义，要换成本机 hostname。
func advertiseAddress(listener net.Listener) (string, error) {
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return "", fmt.Errorf("parse listen address: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("read hostname: %w", err)
	}
	return net.JoinHostPort(host, port), nil
}

// aiWorkerHandlers 只消费 ai:* 任务：索引与重建索引。
// 缩略图、对象校验、GC 与清理留在 cmd/worker。
func aiWorkerHandlers(service *ai.Service, logger *slog.Logger) map[string]asynq.HandlerFunc {
	return map[string]asynq.HandlerFunc{
		outbox.EventAIIndexRequested:     ai.ObjectReadyHandler(service, logger),
		outbox.EventAIReprocessRequested: ai.ReprocessHandler(service, logger),
	}
}
