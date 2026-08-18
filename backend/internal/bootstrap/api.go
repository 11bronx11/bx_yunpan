package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/grpcclient"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/sharing"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func RunAPI(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
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
	// Search / Ask / Reprocess 经 gRPC 调 aisvc；限流已下移到 aisvc 侧。
	// 文档与任务的只读查询仍直接查库，不必绕一次 RPC。
	aiClient, err := newAIClient(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = aiClient.Close() }()
	aiHTTP := ai.NewHTTP(aiClient, ai.NewReader(deps.GORM, driveService), identityHTTP.Authenticate())

	router := httpapi.NewRouter(httpapi.RouterConfig{
		Environment:  cfg.App.Env,
		ProbeTimeout: cfg.HTTP.ProbeTimeout,
		MaxBodyBytes: cfg.HTTP.MaxBodyBytes,
		Logger:       logger,
		Probes:       deps.Probes(),
		Metrics:      httpapi.NewMetrics(),
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

// newAIClient 按配置构造 aisvc 客户端。重试策略按接口性质分别定：
// Ask 固定 1 次（不重试），Search 与 Reprocess 可重试。
func newAIClient(cfg config.Config) (*grpcclient.Client, error) {
	retry := func(attempts int) grpcclient.RetryPolicy {
		return grpcclient.RetryPolicy{
			MaxAttempts: attempts,
			BaseBackoff: cfg.AIService.RetryBaseBackoff,
			MaxBackoff:  cfg.AIService.RetryMaxBackoff,
		}
	}
	return grpcclient.New(grpcclient.Config{
		Target:      cfg.AIService.Target,
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
}
