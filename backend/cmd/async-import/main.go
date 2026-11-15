package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
)

func main() {
	cfg, err := config.LoadForRole("async-import")
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	deps, err := dependencies.OpenAsync(ctx, cfg, false)
	if err != nil {
		fail(err)
	}
	defer deps.Close()
	var legacy int64
	if err := deps.Read.WithContext(ctx).Table("outbox_events").Count(&legacy).Error; err != nil {
		fail(err)
	}
	if legacy > 0 && os.Getenv("ASYNC_LEGACY_WRITERS_STOPPED") != "true" {
		fail(fmt.Errorf("stop legacy API, relays and workers before setting ASYNC_LEGACY_WRITERS_STOPPED=true"))
	}
	s := asynccontrol.New(deps.Control)
	s.Shards = cfg.Async.Shards
	s.MaxAttempts = cfg.Async.MaxAttempts
	s.MaxDispatches = cfg.Async.MaxDispatches
	s.MaxRecoveries = cfg.Async.MaxRecoveries
	s.MaxResultSends = cfg.Async.MaxResultSends
	for k, d := range s.Registry {
		d.Lifetime = cfg.Async.TaskLifetime
		s.Registry[k] = d
	}
	if os.Getenv("ASYNC_LEGACY_WRITERS_STOPPED") == "true" {
		if err := s.DB.WithContext(ctx).Exec("INSERT INTO control_settings(key,value) VALUES('legacy_cutover_pending','true') ON CONFLICT(key) DO UPDATE SET value='true'").Error; err != nil {
			fail(err)
		}
	}
	if err := asyncjobs.ImportLegacy(ctx, deps.Read, s, &asyncjobs.Planner{Read: deps.Read, AI: cfg.AI.Enabled, Search: cfg.Search.Enabled, GCDelay: cfg.Outbox.GCDelay, OutputSafety: cfg.Async.OutputSafety}, cfg.App.InstanceID); err != nil {
		fail(err)
	}
	storage, err := minio.New(cfg.Storage.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""), Secure: cfg.Storage.Secure, Region: cfg.Storage.Region})
	if err != nil {
		fail(err)
	}
	lifecycleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := asyncjobs.EnsureLifecycle(lifecycleCtx, storage, cfg.Storage.Bucket, cfg.Async.StagingDays, cfg.Upload.SessionTTL+cfg.Async.TaskLifetime+cfg.Async.CommitWindow+cfg.Async.OutputSafety); err != nil {
		fail(err)
	}
	if os.Getenv("ASYNC_LEGACY_WRITERS_STOPPED") == "true" {
		if err := asyncjobs.TransferLegacyMessages(ctx, cfg.TaskQueue.URL, s); err != nil {
			fail(err)
		}
		if err := s.DB.WithContext(ctx).Exec("UPDATE control_settings SET value='false' WHERE key='legacy_cutover_pending'").Error; err != nil {
			fail(err)
		}
	}
	slog.Info("legacy async import complete; old queues retained; staging lifecycle verified")
}
func fail(err error) { slog.Error("async import", "error", err); os.Exit(1) }
