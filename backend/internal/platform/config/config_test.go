package config

import (
	"testing"
	"time"
)

func TestLoadRejectsInvalidWorkerConcurrency(t *testing.T) {
	t.Setenv("WORKER_CONCURRENCY", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid worker concurrency to fail")
	}
}

func TestLoadUsesDevelopmentDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.HTTP.Addr != ":8081" {
		t.Fatalf("unexpected HTTP address %q", cfg.HTTP.Addr)
	}
	if cfg.Storage.Bucket != "yunpan" {
		t.Fatalf("unexpected bucket %q", cfg.Storage.Bucket)
	}
	if cfg.HTTP.MaxBodyBytes != 1<<20 || cfg.Postgres.MaxOpenConns != 32 || cfg.Identity.DefaultQuotaBytes != 10*1024*1024*1024 {
		t.Fatalf("unexpected operational defaults: %+v", cfg)
	}
	if cfg.Upload.SessionTTL != 24*time.Hour || cfg.Upload.CleanupBatch != 100 {
		t.Fatalf("unexpected upload defaults: %+v", cfg.Upload)
	}
	if cfg.Outbox.BatchSize != 20 || cfg.Outbox.TaskTimeout != 30*time.Minute || cfg.Outbox.GCDelay != 24*time.Hour {
		t.Fatalf("unexpected outbox defaults: %+v", cfg.Outbox)
	}
	if !cfg.AI.RateLimitEnabled {
		t.Fatal("AI rate limiting should be enabled by default")
	}
	if cfg.AI.RateLimitSearchPerMinute != 30 || cfg.AI.RateLimitAskPerMinute != 10 || cfg.AI.RateLimitReprocessPerMinute != 3 {
		t.Fatalf("unexpected default AI rate limits: %+v", cfg.AI)
	}
}

func TestLoadUsesOperationalOverrides(t *testing.T) {
	t.Setenv("HTTP_MAX_BODY_BYTES", "2048")
	t.Setenv("POSTGRES_MAX_OPEN_CONNS", "48")
	t.Setenv("POSTGRES_MAX_IDLE_CONNS", "12")
	t.Setenv("USER_DEFAULT_QUOTA_BYTES", "21474836480")
	t.Setenv("UPLOAD_SESSION_TTL", "12h")
	t.Setenv("UPLOAD_CLEANUP_BATCH", "25")
	t.Setenv("OUTBOX_BATCH_SIZE", "50")
	t.Setenv("OBJECT_GC_DELAY", "6h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.HTTP.MaxBodyBytes != 2048 || cfg.Postgres.MaxOpenConns != 48 || cfg.Postgres.MaxIdleConns != 12 {
		t.Fatalf("unexpected HTTP/Postgres config: %+v %+v", cfg.HTTP, cfg.Postgres)
	}
	if cfg.Identity.DefaultQuotaBytes != 21474836480 || cfg.Upload.SessionTTL != 12*time.Hour || cfg.Upload.CleanupBatch != 25 {
		t.Fatalf("unexpected identity/upload config: %+v %+v", cfg.Identity, cfg.Upload)
	}
	if cfg.Outbox.BatchSize != 50 || cfg.Outbox.GCDelay != 6*time.Hour {
		t.Fatalf("unexpected outbox config: %+v", cfg.Outbox)
	}
}

func TestLoadRejectsInvalidPostgresPool(t *testing.T) {
	t.Setenv("POSTGRES_MAX_OPEN_CONNS", "4")
	t.Setenv("POSTGRES_MAX_IDLE_CONNS", "8")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid Postgres pool to fail")
	}
}

func TestLoadUsesConfiguredAIRateLimits(t *testing.T) {
	t.Setenv("AI_RATE_LIMIT_SEARCH_PER_MINUTE", "60")
	t.Setenv("AI_RATE_LIMIT_ASK_PER_MINUTE", "20")
	t.Setenv("AI_RATE_LIMIT_REPROCESS_PER_MINUTE", "6")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AI.RateLimitSearchPerMinute != 60 || cfg.AI.RateLimitAskPerMinute != 20 || cfg.AI.RateLimitReprocessPerMinute != 6 {
		t.Fatalf("unexpected configured AI rate limits: %+v", cfg.AI)
	}
}

func TestLoadRejectsNonPositiveEnabledAIRateLimit(t *testing.T) {
	t.Setenv("AI_RATE_LIMIT_ENABLED", "true")
	t.Setenv("AI_RATE_LIMIT_ASK_PER_MINUTE", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected non-positive enabled AI rate limit to fail")
	}
}

func TestLoadCanDisableAIRateLimit(t *testing.T) {
	t.Setenv("AI_RATE_LIMIT_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AI.RateLimitEnabled {
		t.Fatal("AI rate limiting should be disabled")
	}
}
