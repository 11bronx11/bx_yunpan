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
		"PARSER_REMOTE_ENABLED",
	} {
		t.Setenv(name, "false")
	}
	t.Setenv("PARSER_GRPC_ADDR", ":8084")
	t.Setenv("PARSER_GRPC_TARGET", "127.0.0.1:8084")
	t.Setenv("PARSER_ETCD_SERVICE_PREFIX", "/services/document-parser")
	t.Setenv("PARSER_ADVERTISE_ADDR", "")
	t.Setenv("PARSER_CALL_TIMEOUT", "90s")
	t.Setenv("PARSER_MAX_CONCURRENT", "2")
	t.Setenv("AI_RERANK_BASE_URL", "")
	t.Setenv("AI_RERANK_MODEL", "qwen3-rerank")
	t.Setenv("AI_RERANK_CANDIDATES", "30")
	t.Setenv("AI_RERANK_TOP_K", "8")
	t.Setenv("AI_RERANK_TIMEOUT", "30s")
	t.Setenv("OUTBOX_POLL_INTERVAL", "1s")
	t.Setenv("OUTBOX_POLL_MAX_INTERVAL", "30s")
	t.Setenv("OUTBOX_SCAN_TIMEOUT", "10s")
	t.Setenv("OUTBOX_NOTIFY_ENABLED", "true")
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
	if cfg.Outbox.PollInterval != time.Second || cfg.Outbox.MaxPollInterval != 30*time.Second || cfg.Outbox.ScanTimeout != 10*time.Second || !cfg.Outbox.NotifyEnabled {
		t.Fatalf("unexpected outbox wakeup defaults: poll=%s max_poll=%s scan_timeout=%s notify=%t", cfg.Outbox.PollInterval, cfg.Outbox.MaxPollInterval, cfg.Outbox.ScanTimeout, cfg.Outbox.NotifyEnabled)
	}
	if cfg.AI.RequestTimeout != 90*time.Second {
		t.Fatalf("unexpected AI request timeout: %s", cfg.AI.RequestTimeout)
	}
	if cfg.Parser.RemoteEnabled || cfg.Parser.GRPCAddr != ":8084" || cfg.Parser.Target != "127.0.0.1:8084" || cfg.Parser.Prefix != "/services/document-parser" || cfg.Parser.CallTimeout != 90*time.Second || cfg.Parser.MaxConcurrent != 2 {
		t.Fatalf("unexpected parser defaults: %#v", cfg.Parser)
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

func TestLoadUsesRemoteParserSettings(t *testing.T) {
	t.Setenv("PARSER_REMOTE_ENABLED", "true")
	t.Setenv("PARSER_GRPC_ADDR", ":9084")
	t.Setenv("PARSER_GRPC_TARGET", "parser:9084")
	t.Setenv("PARSER_ETCD_SERVICE_PREFIX", "/services/parser-test")
	t.Setenv("PARSER_CALL_TIMEOUT", "45s")
	t.Setenv("PARSER_MAX_CONCURRENT", "6")
	t.Setenv("ETCD_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load remote parser config: %v", err)
	}
	if !cfg.Parser.RemoteEnabled || cfg.Parser.GRPCAddr != ":9084" || cfg.Parser.Target != "parser:9084" || cfg.Parser.Prefix != "/services/parser-test" || cfg.Parser.CallTimeout != 45*time.Second || cfg.Parser.MaxConcurrent != 6 {
		t.Fatalf("unexpected remote parser config: %#v", cfg.Parser)
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
	t.Setenv("OUTBOX_POLL_INTERVAL", "200ms")
	t.Setenv("OUTBOX_POLL_MAX_INTERVAL", "4s")
	t.Setenv("OUTBOX_SCAN_TIMEOUT", "3s")
	t.Setenv("OUTBOX_NOTIFY_ENABLED", "false")
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
	if cfg.Outbox.PollInterval != 200*time.Millisecond || cfg.Outbox.MaxPollInterval != 4*time.Second || cfg.Outbox.ScanTimeout != 3*time.Second || cfg.Outbox.NotifyEnabled {
		t.Fatalf("unexpected outbox wakeup overrides: poll=%s max_poll=%s scan_timeout=%s notify=%t", cfg.Outbox.PollInterval, cfg.Outbox.MaxPollInterval, cfg.Outbox.ScanTimeout, cfg.Outbox.NotifyEnabled)
	}
	if cfg.AI.RequestTimeout != 2*time.Minute {
		t.Fatalf("unexpected AI request timeout: %s", cfg.AI.RequestTimeout)
	}
}

func TestLoadRejectsOutboxMaxPollBelowMinimum(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "2s")
	t.Setenv("OUTBOX_POLL_MAX_INTERVAL", "1s")
	if _, err := Load(); err == nil {
		t.Fatal("expected outbox max poll interval below minimum to fail")
	}
}

func TestLoadRejectsOutboxBatchAboveBound(t *testing.T) {
	t.Setenv("OUTBOX_BATCH_SIZE", "101")
	if _, err := Load(); err == nil {
		t.Fatal("expected outbox batch above the configured bound to fail")
	}
}

func TestLoadRejectsNonPositiveOutboxScanTimeout(t *testing.T) {
	t.Setenv("OUTBOX_SCAN_TIMEOUT", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected non-positive outbox scan timeout to fail")
	}
}

func TestLoadRequiresLeaderTTLAbovePollingWindow(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "2s")
	t.Setenv("OUTBOX_POLL_MAX_INTERVAL", "30s")
	t.Setenv("OUTBOX_LEADER_LOCK_ENABLED", "true")
	t.Setenv("OUTBOX_LEADER_LOCK_TTL", "4s")
	if _, err := Load(); err == nil {
		t.Fatal("expected leader TTL at or below twice the minimum poll interval to fail")
	}
}

func TestLoadSupportsPollingOnlySingleDispatcherMode(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "200ms")
	t.Setenv("OUTBOX_POLL_MAX_INTERVAL", "2s")
	t.Setenv("OUTBOX_SCAN_TIMEOUT", "500ms")
	t.Setenv("OUTBOX_NOTIFY_ENABLED", "false")
	t.Setenv("OUTBOX_LEADER_LOCK_ENABLED", "false")
	t.Setenv("OUTBOX_LEADER_LOCK_TTL", "1s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load polling-only configuration: %v", err)
	}
	if cfg.Outbox.NotifyEnabled || cfg.Locking.DispatcherLeaderEnabled || cfg.Outbox.PollInterval != 200*time.Millisecond || cfg.Outbox.MaxPollInterval != 2*time.Second {
		t.Fatalf("unexpected polling-only configuration: notify=%t leader=%t min=%s max=%s", cfg.Outbox.NotifyEnabled, cfg.Locking.DispatcherLeaderEnabled, cfg.Outbox.PollInterval, cfg.Outbox.MaxPollInterval)
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
	t.Setenv("REDIS_COORDINATION_DB", "4")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load role-isolated config: %v", err)
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

func TestLoadRejectsExternalReadWithoutEndpoints(t *testing.T) {
	t.Setenv("SEARCH_READ_BACKEND", "es_qdrant")
	t.Setenv("SEARCH_WRITE_BACKENDS", "es_qdrant")
	t.Setenv("SEARCH_ES_ENDPOINT", "")
	t.Setenv("SEARCH_QDRANT_ENDPOINT", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected external read endpoints to be required")
	}
}

func TestLoadAcceptsExternalSearchSettings(t *testing.T) {
	t.Setenv("SEARCH_INDEX_ENABLED", "true")
	t.Setenv("SEARCH_READ_BACKEND", "es_qdrant")
	t.Setenv("SEARCH_WRITE_BACKENDS", "postgres,es_qdrant")
	t.Setenv("SEARCH_ES_ENDPOINT", "http://elasticsearch:9200")
	t.Setenv("SEARCH_QDRANT_ENDPOINT", "http://qdrant:6333")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Search.ExternalTimeout != 2*time.Second || cfg.Search.VectorOversample != 4 || !cfg.Search.ExternalFallback {
		t.Fatalf("unexpected search settings: %#v", cfg.Search)
	}
}
