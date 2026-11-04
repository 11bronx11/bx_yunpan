package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
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
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	platformworker "github.com/11bronx11/bx_yunpan/backend/internal/platform/worker"
	searchindex "github.com/11bronx11/bx_yunpan/backend/internal/search"
)

// RunAISvc 在同一进程内跑 gRPC server 与 AI 专属 RabbitMQ worker。
//
// AI 模块是唯一有真实独立扩容理由的模块：抽取、OCR、Embedding 都是 CPU/IO
// 重活，与 API 的请求特征不同，混在一个进程里会互相挤资源。
func RunAISvc(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithSearchEnabled(ctx, cfg.Search.Enabled)
	switch cfg.AIService.Role {
	case "query":
		return RunAIQuery(ctx, cfg, logger)
	case "indexer":
		return RunAIIndexer(ctx, cfg, logger)
	default:
		return runCombinedAISvc(ctx, cfg, logger)
	}
}

func runCombinedAISvc(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()

	driveService := drive.NewService(deps.GORM)
	objects := objectstore.NewService(deps.GORM)
	aiCache := newQueryEmbeddingCache(cfg, deps)
	metricsRegistry := prometheus.NewRegistry()
	parser, closeParser, err := newParserGateway(cfg, logger, metricsRegistry)
	if err != nil {
		return err
	}
	defer closeParser()
	aiService := ai.NewServiceWithCache(deps.GORM, driveService, objects, deps.Storage, cfg.AI, aiCache).
		WithParser(parser).WithParserProfileVersion(cfg.Parser.ProfileVersion).WithPipelineVersion(indexPipelineVersion(cfg))
	configureAISearch(aiService, cfg)

	// 限流下移到 aisvc：放 API 侧的话多副本各自计数，总量会是副本数的倍数，
	// 对下游 LLM 配额没有保护作用。
	var limiter ai.RequestLimiter
	if cfg.AI.RateLimitEnabled {
		limiter = ai.NewRequestLimiter(deps.RedisCoordination, ai.RateLimits{
			SearchPerMinute:    cfg.AI.RateLimitSearchPerMinute,
			AskPerMinute:       cfg.AI.RateLimitAskPerMinute,
			ReprocessPerMinute: cfg.AI.RateLimitReprocessPerMinute,
		})
	}

	server := grpc.NewServer(grpcx.ServerInterceptors(grpcx.ServerOptions{
		Logger:  logger,
		Metrics: grpcx.NewMetrics(metricsRegistry, "server"),
		Mapper:  ai.GRPCStatus,
	})...)
	defer server.Stop()
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
	workerDone := make(chan struct{})
	defer func() { stopWorker(); <-workerDone }()
	aiQueues := map[string]int{platformworker.QueueAI: cfg.Worker.Queues[platformworker.QueueAI]}
	workerMetrics := platformworker.NewMetrics(metricsRegistry, platformworker.Role("aisvc"), aiQueues)
	go func() {
		defer close(workerDone)
		workerErr <- platformworker.RunQueuesWithMetrics(workerCtx, cfg, aiQueues, logger, aiWorkerHandlers(aiService, logger), workerMetrics, deps.GORM)
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

// RunAIQuery is the online half of the P3 split. It owns the gRPC query/ask
// contract and does not consume the asynchronous ai queue.
func RunAIQuery(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	service := ai.NewServiceWithCache(deps.GORM, drive.NewService(deps.GORM), objectstore.NewService(deps.GORM), deps.Storage, cfg.AI,
		newQueryEmbeddingCache(cfg, deps)).WithParserProfileVersion(cfg.Parser.ProfileVersion).WithPipelineVersion(indexPipelineVersion(cfg))
	configureAISearch(service, cfg)
	var limiter ai.RequestLimiter
	if cfg.AI.RateLimitEnabled {
		limiter = ai.NewRequestLimiter(deps.RedisCoordination, ai.RateLimits{
			SearchPerMinute: cfg.AI.RateLimitSearchPerMinute, AskPerMinute: cfg.AI.RateLimitAskPerMinute,
			ReprocessPerMinute: cfg.AI.RateLimitReprocessPerMinute,
		})
	}
	server, healthServer, listener, deregister, serverErr, registry, err := startAIServer(ctx, cfg, logger, service, limiter)
	if err != nil {
		return err
	}
	go serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, registry, logger)
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return fmt.Errorf("serve ai-query grpc: %w", err)
	}
	healthServer.SetServingStatus(pb.AIService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	deregister()
	server.GracefulStop()
	_ = listener.Close()
	return nil
}

// RunAIIndexer is the offline half of the P3 split. It owns the ai queue and
// never advertises an online gRPC endpoint, so query replicas can be scaled
// independently of extraction/embedding workers.
func RunAIIndexer(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	metricsRegistry := prometheus.NewRegistry()
	parser, closeParser, err := newParserGateway(cfg, logger, metricsRegistry)
	if err != nil {
		return err
	}
	defer closeParser()
	service := ai.NewServiceWithCache(deps.GORM, drive.NewService(deps.GORM), objectstore.NewService(deps.GORM), deps.Storage, cfg.AI, nil).
		WithParser(parser).WithParserProfileVersion(cfg.Parser.ProfileVersion).WithPipelineVersion(indexPipelineVersion(cfg))
	aiQueues := map[string]int{platformworker.QueueAI: cfg.Worker.Queues[platformworker.QueueAI]}
	workerMetrics := platformworker.NewMetrics(metricsRegistry, platformworker.Role("ai-indexer"), aiQueues)
	go serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger)
	return platformworker.RunQueuesWithMetrics(ctx, cfg, aiQueues, logger, aiWorkerHandlers(service, logger), workerMetrics, deps.GORM)
}

func newQueryEmbeddingCache(cfg config.Config, deps *dependencies.Set) ai.QueryEmbeddingCache {
	if !cfg.Cache.Enabled {
		return nil
	}
	return platformcache.NewEmbeddingStore(deps.RedisCache, cfg.Cache.TTL, cfg.Cache.L1MaxItems)
}

func configureAISearch(service *ai.Service, cfg config.Config) {
	if service == nil || (cfg.Search.ReadBackend != "es_qdrant" && cfg.Search.ReadBackend != "shadow") {
		return
	}
	client := &http.Client{Timeout: cfg.Search.ExternalTimeout}
	var textBackend, vectorBackend searchindex.RetrievalBackend
	if cfg.Search.ESEndpoint != "" {
		textBackend = searchindex.NewElasticsearchIndex(cfg.Search.ESEndpoint, cfg.Search.ESUsername, cfg.Search.ESPassword, client)
	}
	if cfg.Search.QdrantEndpoint != "" {
		vectorBackend = searchindex.NewQdrantIndex(cfg.Search.QdrantEndpoint, cfg.Search.QdrantAPIKey, cfg.AI.Dimension, client)
	}
	service.WithRetrieval(searchindex.NewRetrievalRouter(textBackend, vectorBackend), cfg.Search)
}

func startAIServer(ctx context.Context, cfg config.Config, logger *slog.Logger, aiService *ai.Service, limiter ai.RequestLimiter) (*grpc.Server, *health.Server, net.Listener, func(), <-chan error, *prometheus.Registry, error) {
	registry := prometheus.NewRegistry()
	server := grpc.NewServer(grpcx.ServerInterceptors(grpcx.ServerOptions{
		Logger: logger, Metrics: grpcx.NewMetrics(registry, "server"), Mapper: ai.GRPCStatus,
	})...)
	pb.RegisterAIServiceServer(server, grpcserver.New(aiService, limiter))
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus(pb.AIService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	listener, err := net.Listen("tcp", cfg.AIService.GRPCAddr)
	if err != nil {
		return nil, nil, nil, func() {}, nil, registry, fmt.Errorf("listen grpc: %w", err)
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("ai-query grpc listening", "address", cfg.AIService.GRPCAddr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serverErr <- err
		}
	}()
	deregister, err := registerAISvc(ctx, cfg, logger, listener)
	if err != nil {
		server.Stop()
		_ = listener.Close()
		return nil, nil, nil, func() {}, nil, registry, err
	}
	return server, healthServer, listener, deregister, serverErr, registry, nil
}

// registerAISvc 把本实例注册到 etcd，返回幂等的撤销函数。
// 未启用 etcd 时返回空操作，让单副本部署无需额外依赖。
func registerAISvc(ctx context.Context, cfg config.Config, logger *slog.Logger, listener net.Listener) (func(), error) {
	return registerEtcdInstance(ctx, cfg, logger, listener, cfg.Registry.Prefix, cfg.Registry.AdvertiseAddr, "aisvc")
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
func aiWorkerHandlers(service *ai.Service, logger *slog.Logger) map[string]taskqueue.HandlerFunc {
	return map[string]taskqueue.HandlerFunc{
		outbox.EventAIIndexRequested:     ai.ObjectReadyHandler(service, logger),
		outbox.TaskAIIndexV1:             ai.ObjectReadyHandler(service, logger),
		outbox.EventAIReprocessRequested: ai.ReprocessHandler(service, logger),
		outbox.TaskAIReprocessV1:         ai.ReprocessHandler(service, logger),
	}
}
