package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/parserengine"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/parserrpc"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/registry"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
)

const parserPipelineBaseVersion = "document-index-v5"

func indexPipelineVersion(cfg config.Config) string {
	spec := struct {
		BaseVersion          string
		Provider             string
		EmbeddingModel       string
		ChatModel            string
		VisionModel          string
		Dimension            int
		MaxObjectBytes       int64
		RemoteParser         bool
		ParserProfileVersion string
		ChunkerVersion       string
		TokenizerID          string
		ChunkTokenBudget     int
		ChunkOverlapTokens   int
		MaxChunks            int
	}{
		BaseVersion: parserPipelineBaseVersion, Provider: cfg.AI.Provider,
		EmbeddingModel: cfg.AI.EmbeddingModel, ChatModel: cfg.AI.ChatModel,
		VisionModel: cfg.AI.VisionModel, Dimension: cfg.AI.Dimension,
		MaxObjectBytes: cfg.AI.MaxObjectBytes, RemoteParser: cfg.Parser.RemoteEnabled,
		ParserProfileVersion: cfg.Parser.ProfileVersion,
		ChunkerVersion:       cfg.Parser.ChunkerVersion, TokenizerID: cfg.Parser.TokenizerID,
		ChunkTokenBudget: cfg.Parser.ChunkTokenBudget, ChunkOverlapTokens: cfg.Parser.ChunkOverlapTokens,
		MaxChunks: cfg.Parser.MaxChunks,
	}
	serialized, _ := json.Marshal(spec)
	digest := sha256.Sum256(serialized)
	return fmt.Sprintf("%s-%x", parserPipelineBaseVersion, digest[:12])
}

func newParserGateway(cfg config.Config, logger *slog.Logger, registerer prometheus.Registerer) (ai.ParserGateway, func(), error) {
	noop := func() {}
	if !cfg.Parser.RemoteEnabled {
		return nil, noop, nil
	}

	target := cfg.Parser.Target
	closeRegistry := noop
	if cfg.Registry.Enabled {
		client, err := registry.Open(cfg.Registry)
		if err != nil {
			return nil, noop, err
		}
		resolver.Register(registry.NewBuilder(client, logger, registry.NewMetrics(registerer)))
		target = registry.Scheme + "://" + cfg.Parser.Prefix
		closeRegistry = func() { _ = client.Close() }
	}

	remote, err := parserrpc.NewClient(target, cfg.Parser.CallTimeout)
	if err != nil {
		closeRegistry()
		return nil, noop, err
	}
	cleanup := func() {
		_ = remote.Close()
		closeRegistry()
	}
	return remote, cleanup, nil
}

// RunParserSvc serves parser RPCs and registers each replica independently so
// the AI indexer can balance extraction work across parser instances.
func RunParserSvc(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	metricsRegistry := prometheus.NewRegistry()
	go serveWorkerMetrics(ctx, cfg.Worker.MetricsAddr, metricsRegistry, logger)
	engine, err := parserengine.Start(ctx, cfg.Parser.EngineAddr, cfg.Parser.WorkerDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := engine.Close(); err != nil {
			logger.WarnContext(ctx, "stop document parser worker", "error", err)
		}
	}()
	parser := ai.NewChunkingParser(engine, rag.ChunkOptions{
		TokenBudget: cfg.Parser.ChunkTokenBudget, Overlap: cfg.Parser.ChunkOverlapTokens,
		MaxChunks: cfg.Parser.MaxChunks, Version: cfg.Parser.ChunkerVersion, TokenizerID: cfg.Parser.TokenizerID,
	})
	serverOptions := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(parserrpc.MaxRequestBytes),
		grpc.MaxSendMsgSize(parserrpc.MaxResponseBytes),
	}
	serverOptions = append(serverOptions, grpcx.ServerInterceptors(grpcx.ServerOptions{
		Logger: logger, Metrics: grpcx.NewMetrics(metricsRegistry, "server"), Mapper: ai.GRPCStatus,
	})...)
	server := grpc.NewServer(serverOptions...)
	defer server.Stop()
	pb.RegisterDocumentParserServiceServer(server, parserrpc.NewServer(parser, cfg.Parser.MaxConcurrent))
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus(pb.DocumentParserService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	listener, err := net.Listen("tcp", cfg.Parser.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen parser gRPC: %w", err)
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("document parser gRPC listening", "address", cfg.Parser.GRPCAddr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serverErr <- err
		}
	}()

	deregister, err := registerEtcdInstance(ctx, cfg, logger, listener,
		cfg.Parser.Prefix, cfg.Parser.AdvertiseAddr, "document-parser")
	if err != nil {
		server.Stop()
		_ = listener.Close()
		return err
	}
	defer deregister()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return fmt.Errorf("serve document parser gRPC: %w", err)
	}
	healthServer.SetServingStatus(pb.DocumentParserService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	deregister()
	server.GracefulStop()
	return nil
}

func registerEtcdInstance(ctx context.Context, cfg config.Config, logger *slog.Logger, listener net.Listener, prefix, advertiseAddr, service string) (func(), error) {
	noop := func() {}
	client, err := registry.Open(cfg.Registry)
	if err != nil {
		return noop, err
	}
	if client == nil {
		return noop, nil
	}

	address := advertiseAddr
	if address == "" {
		address, err = advertiseAddress(listener)
		if err != nil {
			_ = client.Close()
			return noop, err
		}
	}
	instanceID := uuid.Must(uuid.NewV7()).String()
	registrar := registry.NewRegistrar(client, logger, prefix+"/"+instanceID, address, cfg.Registry.LeaseTTL)
	if err := registrar.Register(ctx); err != nil {
		_ = client.Close()
		return noop, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			if err := registrar.Deregister(revokeCtx); err != nil {
				logger.WarnContext(ctx, "deregister service", "service", service, "error", err)
			}
			_ = client.Close()
		})
	}, nil
}
