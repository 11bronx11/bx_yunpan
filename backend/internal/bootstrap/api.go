package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
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
	aiService := ai.NewService(deps.GORM, driveService, objects, deps.Storage, cfg.AI)
	var aiLimiter ai.RequestLimiter
	if cfg.AI.RateLimitEnabled {
		aiLimiter = ai.NewRequestLimiter(deps.Redis, ai.RateLimits{
			SearchPerMinute:    cfg.AI.RateLimitSearchPerMinute,
			AskPerMinute:       cfg.AI.RateLimitAskPerMinute,
			ReprocessPerMinute: cfg.AI.RateLimitReprocessPerMinute,
		})
	}
	aiHTTP := ai.NewHTTP(aiService, identityHTTP.Authenticate(), aiLimiter)

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
