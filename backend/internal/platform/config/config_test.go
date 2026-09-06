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
	// The default-value contract must not depend on a developer's deployment
	// environment. In particular, deploy/.env carries an optional reranker URL
	// even while reranking is disabled.
	for _, name := range []string{
		"AI_RERANK_ENABLED",
	} {
		t.Setenv(name, "false")
	}
	t.Setenv("AI_RERANK_BASE_URL", "")
	t.Setenv("AI_RERANK_MODEL", "qwen3-rerank")
	t.Setenv("AI_RERANK_CANDIDATES", "30")
	t.Setenv("AI_RERANK_TOP_K", "8")
	t.Setenv("AI_RERANK_TIMEOUT", "30s")
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
		t.Fatalf("unexpected operational defaults: body=%d open=%d quota=%d", cfg.HTTP.MaxBodyBytes, cfg.Postgres.MaxOpenConns, cfg.Identity.DefaultQuotaBytes)
	}
	if cfg.Upload.SessionTTL != 24*time.Hour || cfg.Upload.CleanupBatch != 100 {
		t.Fatalf("unexpected upload defaults: session_ttl=%s cleanup_batch=%d", cfg.Upload.SessionTTL, cfg.Upload.CleanupBatch)
	}
	if cfg.Outbox.BatchSize != 20 || cfg.Outbox.TaskTimeout != 30*time.Minute || cfg.Outbox.GCDelay != 24*time.Hour {
		t.Fatalf("unexpected outbox defaults: batch=%d timeout=%s gc_delay=%s", cfg.Outbox.BatchSize, cfg.Outbox.TaskTimeout, cfg.Outbox.GCDelay)
	}
	if cfg.AI.RequestTimeout != 90*time.Second {
		t.Fatalf("unexpected AI request timeout: %s", cfg.AI.RequestTimeout)
	}
	if cfg.AI.RerankEnabled || cfg.AI.RerankBaseURL != "" || cfg.AI.RerankModel != "qwen3-rerank" || cfg.AI.RerankCandidates != 30 || cfg.AI.RerankTopK != 8 || cfg.AI.RerankTimeout != 30*time.Second {
		t.Fatalf("unexpected reranker defaults: enabled=%t base_url_set=%t model=%q candidates=%d top_k=%d timeout=%s", cfg.AI.RerankEnabled, cfg.AI.RerankBaseURL != "", cfg.AI.RerankModel, cfg.AI.RerankCandidates, cfg.AI.RerankTopK, cfg.AI.RerankTimeout)
	}
	if !cfg.AI.Enabled {
		t.Fatal("AI should be enabled by default")
	}
	if !cfg.AI.RateLimitEnabled {
		t.Fatal("AI rate limiting should be enabled by default")
	}
	if cfg.AI.RateLimitSearchPerMinute != 30 || cfg.AI.RateLimitAskPerMinute != 10 || cfg.AI.RateLimitReprocessPerMinute != 3 {
		t.Fatalf("unexpected default AI rate limits: search=%d ask=%d reprocess=%d", cfg.AI.RateLimitSearchPerMinute, cfg.AI.RateLimitAskPerMinute, cfg.AI.RateLimitReprocessPerMinute)
	}
}

func TestLoadSupportsDisabledAI(t *testing.T) {
	t.Setenv("AI_ENABLED", "false")
	t.Setenv("AI_PROVIDER", "disabled")
	t.Setenv("AI_EMBEDDING_DIMENSION", "0")
	t.Setenv("AISVC_CALL_TIMEOUT", "1ms")
	t.Setenv("ETCD_ENABLED", "true")
	t.Setenv("ETCD_LEASE_TTL", "1s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load disabled AI config: %v", err)
	}
	if cfg.AI.Enabled {
		t.Fatal("AI should be disabled")
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
	t.Setenv("AI_REQUEST_TIMEOUT", "2m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.HTTP.MaxBodyBytes != 2048 || cfg.Postgres.MaxOpenConns != 48 || cfg.Postgres.MaxIdleConns != 12 {
		t.Fatalf("unexpected HTTP/Postgres config: body=%d open=%d idle=%d", cfg.HTTP.MaxBodyBytes, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns)
	}
	if cfg.Identity.DefaultQuotaBytes != 21474836480 || cfg.Upload.SessionTTL != 12*time.Hour || cfg.Upload.CleanupBatch != 25 {
		t.Fatalf("unexpected identity/upload config: quota=%d session_ttl=%s cleanup_batch=%d", cfg.Identity.DefaultQuotaBytes, cfg.Upload.SessionTTL, cfg.Upload.CleanupBatch)
	}
	if cfg.Outbox.BatchSize != 50 || cfg.Outbox.GCDelay != 6*time.Hour {
		t.Fatalf("unexpected outbox config: batch=%d gc_delay=%s", cfg.Outbox.BatchSize, cfg.Outbox.GCDelay)
	}
	if cfg.AI.RequestTimeout != 2*time.Minute {
		t.Fatalf("unexpected AI request timeout: %s", cfg.AI.RequestTimeout)
	}
}

func TestLoadPrefersPostgresPrimaryDSN(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://legacy@legacy-db/yunpan")
	t.Setenv("POSTGRES_PRIMARY_DSN", "postgres://writer@haproxy/yunpan")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Postgres.PrimaryDSN != "postgres://writer@haproxy/yunpan" || cfg.Postgres.DSN != cfg.Postgres.PrimaryDSN {
		t.Fatalf("expected primary writer endpoint to be authoritative, got primary_dsn=%q dsn_matches=%t", cfg.Postgres.PrimaryDSN, cfg.Postgres.DSN == cfg.Postgres.PrimaryDSN)
	}
}

func TestLoadFallsBackToLegacyPostgresDSN(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://legacy@legacy-db/yunpan")
	t.Setenv("POSTGRES_PRIMARY_DSN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Postgres.PrimaryDSN != "postgres://legacy@legacy-db/yunpan" || cfg.Postgres.DSN != cfg.Postgres.PrimaryDSN {
		t.Fatalf("expected legacy DSN compatibility fallback, got primary_dsn=%q dsn_matches=%t", cfg.Postgres.PrimaryDSN, cfg.Postgres.DSN == cfg.Postgres.PrimaryDSN)
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
		t.Fatalf("unexpected configured AI rate limits: search=%d ask=%d reprocess=%d", cfg.AI.RateLimitSearchPerMinute, cfg.AI.RateLimitAskPerMinute, cfg.AI.RateLimitReprocessPerMinute)
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

func TestLoadRejectsInvalidRerankTopK(t *testing.T) {
	t.Setenv("AI_RERANK_ENABLED", "true")
	t.Setenv("AI_RERANK_CANDIDATES", "4")
	t.Setenv("AI_RERANK_TOP_K", "5")
	if _, err := Load(); err == nil {
		t.Fatal("expected reranker top-k larger than candidates to fail")
	}
}

func TestLoadRejectsDashScopeRerankWithoutBaseURL(t *testing.T) {
	t.Setenv("AI_PROVIDER", "dashscope")
	t.Setenv("DASHSCOPE_API_KEY", "test-key")
	t.Setenv("AI_RERANK_ENABLED", "true")
	t.Setenv("AI_RERANK_BASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected DashScope reranker base URL to be required")
	}
}

func TestLoadSupportsRedisRoleIsolationWithLegacyFallback(t *testing.T) {
	t.Setenv("REDIS_ADDR", "legacy:6379")
	t.Setenv("REDIS_PASSWORD", "legacy-password")
	t.Setenv("REDIS_DB", "2")
	// A partially migrated environment may override only one role. Every
	// unspecified role must still inherit the legacy connection settings.
	t.Setenv("REDIS_BROKER_ADDR", "broker:6379")
	t.Setenv("REDIS_COORDINATION_DB", "4")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load role-isolated config: %v", err)
	}
	if cfg.Redis.BrokerAddr != "broker:6379" || cfg.Redis.BrokerPassword != "legacy-password" || cfg.Redis.BrokerDB != 2 {
		t.Fatalf("unexpected broker config: addr=%q db=%d password_set=%t", cfg.Redis.BrokerAddr, cfg.Redis.BrokerDB, cfg.Redis.BrokerPassword != "")
	}
	if cfg.Redis.CoordinationAddr != "legacy:6379" || cfg.Redis.CoordinationPassword != "legacy-password" || cfg.Redis.CoordinationDB != 4 {
		t.Fatalf("unexpected coordination config: addr=%q db=%d password_set=%t", cfg.Redis.CoordinationAddr, cfg.Redis.CoordinationDB, cfg.Redis.CoordinationPassword != "")
	}
	if cfg.Redis.CacheAddr != "legacy:6379" || cfg.Redis.CachePassword != "legacy-password" || cfg.Redis.CacheDB != 2 {
		t.Fatalf("unexpected cache config: addr=%q db=%d password_set=%t", cfg.Redis.CacheAddr, cfg.Redis.CacheDB, cfg.Redis.CachePassword != "")
	}
}

func TestLoadSupportsCacheRollbackSwitch(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Cache.Enabled {
		t.Fatal("cache should be disabled")
	}
}

func TestLoadRejectsInvalidCacheBounds(t *testing.T) {
	t.Setenv("CACHE_TTL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected zero cache TTL to fail")
	}
}

func TestLoadRejectsUnknownAIServiceRole(t *testing.T) {
	t.Setenv("AI_SERVICE_ROLE", "sidecar")
	if _, err := Load(); err == nil {
		t.Fatal("expected unknown AI service role to fail validation")
	}
}
