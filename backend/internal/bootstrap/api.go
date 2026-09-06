package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/resolver"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/grpcclient"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/registry"
	"github.com/11bronx11/bx_yunpan/backend/internal/sharing"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func RunAPI(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx = outbox.WithSearchEnabled(ctx, cfg.Search.Enabled)
	ctx = outbox.WithKafkaEnabled(ctx, cfg.EventStream.KafkaEnabled)
	deps, err := dependencies.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()
	driveService := drive.NewService(deps.GORM)
	tokens := identity.NewTokenManager(cfg.Auth)
	identityService := identity.NewService(deps.GORM, tokens, driveService, cfg.Identity.DefaultQuotaBytes)
	identityHTTP := identity.NewHTTP(identityService, tokens, cfg.Auth)
	driveHTTP := drive.NewHTTP(driveService, identityHTTP.Authenticate())
	objects := objectstore.NewService(deps.GORM)
	uploadService := upload.NewService(deps.GORM, deps.Storage, deps.Presigner, deps.Bucket, driveService, objects, identityService, cfg.Upload)
	uploadHTTP := upload.NewHTTP(uploadService, identityHTTP.Authenticate())
	mediaService := media.NewService(deps.GORM, objects, deps.Storage, deps.Presigner, deps.Bucket, cfg.Storage.ReadURLTTL)
	fileManager := drive.NewFileManager(driveService, objects, identityService, deps.Presigner, mediaService, cfg.Storage.ReadURLTTL)
	fileHTTP := drive.NewFileHTTP(fileManager, identityHTTP.Authenticate())
	sharingService := sharing.NewService(deps.GORM, driveService, objects, identityService, cfg.Sharing)
	sharingHTTP := sharing.NewHTTP(sharingService, fileManager, identityHTTP.Authenticate())
	metrics := httpapi.NewMetrics(cfg.App.InstanceID)
	readiness := httpapi.NewReadiness(true)
	var aiBackend ai.Backend = ai.DisabledBackend{}
	closeAI := func() {}
	if cfg.AI.Enabled {
		// Search / Ask 经 gRPC 调 query 服务；Reprocess 是 core-api 的命令，
		// 必须在本地事务里完成 owner 校验、幂等 Task 和 Outbox 写入。
		aiClient, closeRegistry, err := newAIClient(cfg, logger, metrics.Registry)
		if err != nil {
			return err
		}
		aiBackend = &splitAIBackend{query: aiClient, command: ai.NewService(deps.GORM, driveService, objects, deps.Storage, cfg.AI)}
		closeAI = func() {
			_ = aiClient.Close()
			closeRegistry()
		}
	}
	defer closeAI()
	aiHTTP := ai.NewHTTP(aiBackend, ai.NewReader(deps.GORM, driveService), identityHTTP.Authenticate())

	router := httpapi.NewRouter(httpapi.RouterConfig{
		Environment:     cfg.App.Env,
		ServiceName:     "bx-yunpan-api",
		ProbeTimeout:    cfg.HTTP.ProbeTimeout,
		MaxBodyBytes:    cfg.HTTP.MaxBodyBytes,
		V2EventsEnabled: cfg.Outbox.V2EventsEnabled,
		SearchEnabled:   cfg.Search.Enabled,
		KafkaEnabled:    cfg.EventStream.KafkaEnabled,
		Readiness:       readiness,
		Logger:          logger,
		Probes:          deps.Probes(),
		AdvisoryProbes:  deps.AdvisoryProbes(),
		Metrics:         metrics,
		Registrars: []func(*gin.RouterGroup){
			identityHTTP.RegisterRoutes,
			driveHTTP.RegisterRoutes,
			fileHTTP.RegisterRoutes,
			uploadHTTP.RegisterRoutes,
			sharingHTTP.RegisterRoutes,
			aiHTTP.RegisterRoutes,
		},
	})
	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api listening", "address", cfg.HTTP.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("api shutdown requested")
		readiness.SetReady(false)
	case err := <-serverErr:
		return fmt.Errorf("serve http: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http: %w", err)
	}
	return nil
}

// splitAIBackend keeps the online query plane behind gRPC while retaining
// command ownership in core-api. The legacy gRPC Reprocess endpoint remains
// available for an older client during the compatibility window, but current
// HTTP requests never depend on it.
type splitAIBackend struct {
	query   *grpcclient.Client
	command *ai.Service
}

func (b *splitAIBackend) Search(ctx context.Context, ownerID uuid.UUID, input ai.SearchInput) ([]ai.SearchHit, error) {
	return b.query.Search(ctx, ownerID, input)
}

func (b *splitAIBackend) Ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []ai.Citation, error) {
	return b.query.Ask(ctx, ownerID, question, folderID, fileIDs)
}

func (b *splitAIBackend) RequestReprocess(ctx context.Context, ownerID, fileID uuid.UUID, idempotencyKey string) (ai.Task, error) {
	return b.command.RequestReprocess(ctx, ownerID, fileID, idempotencyKey)
}

// newAIClient 按配置构造 aisvc 客户端。重试策略按接口性质分别定：
// Ask 固定 1 次（不重试），Search 与 Reprocess 可重试。
func newAIClient(cfg config.Config, logger *slog.Logger, registerer prometheus.Registerer) (*grpcclient.Client, func(), error) {
	closeRegistry := func() {}
	target := cfg.AIService.Target
	if cfg.Registry.Enabled {
		etcdClient, err := registry.Open(cfg.Registry)
		if err != nil {
			return nil, closeRegistry, err
		}
		// resolver 注册进 gRPC 的全局表，dial 目标改成 scheme URL 后由它解析。
		resolver.Register(registry.NewBuilder(etcdClient, logger, registry.NewMetrics(registerer)))
		target = registry.Scheme + "://" + cfg.Registry.Prefix
		closeRegistry = func() { _ = etcdClient.Close() }
	}

	retry := func(attempts int) grpcclient.RetryPolicy {
		return grpcclient.RetryPolicy{
			MaxAttempts: attempts,
			BaseBackoff: cfg.AIService.RetryBaseBackoff,
			MaxBackoff:  cfg.AIService.RetryMaxBackoff,
		}
	}
	client, err := grpcclient.New(grpcclient.Config{
		Target:      target,
		CallTimeout: cfg.AIService.CallTimeout,
		SearchRetry: retry(cfg.AIService.SearchMaxAttempts),
		// LLM 调用非幂等且有成本，超时重试可能重复计费还返回两份不同答案。
		AskRetry:       retry(1),
		ReprocessRetry: retry(cfg.AIService.ReprocessMaxAttempts),
		Breaker: breaker.Config{
			Window:         cfg.AIService.BreakerWindow,
			MinRequests:    cfg.AIService.BreakerMinRequests,
			FailureRate:    cfg.AIService.BreakerFailureRate,
			OpenTimeout:    cfg.AIService.BreakerOpenTimeout,
			HalfOpenProbes: cfg.AIService.BreakerHalfOpenProbes,
		},
	})
	if err != nil {
		closeRegistry()
		return nil, func() {}, err
	}
	return client, closeRegistry, nil
}
