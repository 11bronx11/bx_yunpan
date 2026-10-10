package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	App       App
	HTTP      HTTP
	Postgres  Postgres
	Redis     Redis
	Cache     Cache
	Storage   Storage
	Worker    Worker
	Auth      Auth
	Identity  Identity
	Upload    Upload
	Outbox    Outbox
	TaskQueue TaskQueue
	Search    Search
	Sharing   Sharing
	AI        AI
	AIService AIService
	Tracing   Tracing
	Registry  Registry
	Locking   Locking
}

type App struct {
	Env        string
	LogLevel   string
	InstanceID string
}

type HTTP struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ProbeTimeout      time.Duration
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxBodyBytes      int64
	MaxHeaderBytes    int
}

type Postgres struct {
	// PrimaryDSN is the stable writer endpoint used by every process during
	// the HA migration. DSN remains as the compatibility alias for older
	// callers and is kept equal to PrimaryDSN when loaded from the environment.
	PrimaryDSN      string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type Redis struct {
	// Addr/Password/DB are the compatibility aliases for a single Redis
	// instance.  The role-specific fields let Broker, Coordination and Cache
	// move to independent instances without changing callers at once.
	Addr                 string
	Password             string
	DB                   int
	CoordinationAddr     string
	CoordinationPassword string
	CoordinationDB       int
	CacheAddr            string
	CachePassword        string
	CacheDB              int
}

type Cache struct {
	Enabled    bool
	TTL        time.Duration
	L1MaxItems int
}

type Storage struct {
	Endpoint         string
	PublicEndpoint   string
	PublicPathPrefix string
	AccessKey        string
	SecretKey        string
	Bucket           string
	Region           string
	Secure           bool
	PublicSecure     bool
	ReadURLTTL       time.Duration
}

type Worker struct {
	Concurrency int
	Queues      map[string]int
	MetricsAddr string
}

type Auth struct {
	Issuer       string
	SigningSeed  string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	CookieSecure bool
	CookieDomain string
}

type Identity struct {
	DefaultQuotaBytes int64
}

type Upload struct {
	SessionTTL      time.Duration
	PartURLTTL      time.Duration
	CleanupInterval time.Duration
	CleanupBatch    int
}

type Outbox struct {
	PollInterval    time.Duration
	BatchSize       int
	MaxRetry        int
	TaskTimeout     time.Duration
	GCDelay         time.Duration
	V2EventsEnabled bool
}

// TaskQueue controls the RabbitMQ task execution plane. Redis remains
// available for cache/coordination during the migration, but is no longer a
// task broker for new workers.
type TaskQueue struct {
	URL            string
	Exchange       string
	Prefetch       int
	PublishTimeout time.Duration
	StopTimeout    time.Duration
}

// Search controls the rebuildable derived index plane. PostgreSQL remains the
// default read path until an external index has passed snapshot/shadow gates.
type Search struct {
	Enabled          bool
	WriteBackends    []string
	ReadBackend      string
	ShadowSampleRate float64
	IndexEpoch       string
	ESEndpoint       string
	ESUsername       string
	ESPassword       string
	QdrantEndpoint   string
	QdrantAPIKey     string
	ExternalTimeout  time.Duration
	WriteTimeout     time.Duration
	BatchSize        int
	VectorOversample int
	ExternalFallback bool
}

type Sharing struct {
	Secret    string
	AccessTTL time.Duration
}

// Tracing 描述 OpenTelemetry 接入参数。
type Tracing struct {
	Enabled bool
	// Endpoint 是 OTLP gRPC collector 地址（host:port，不带 scheme）。
	Endpoint string
	// SamplerRatio 配合 ParentBased 使用，开发默认 1.0 全采。
	SamplerRatio float64
	Environment  string
	InstanceID   string
}

// AIService 描述 aisvc 进程与 API 侧客户端的连接参数。
type AIService struct {
	// Role controls the P3 split. combined preserves the current compatibility
	// process; query serves gRPC only; indexer consumes the ai queue only.
	Role string
	// GRPCAddr 是 aisvc 的 gRPC 监听地址。
	GRPCAddr string
	// Target 是 API 侧的 dial 目标：host:port 或 etcd resolver URL。
	Target string
	// CallTimeout 是 API 侧单次调用的总预算，必须不小于 AI.RequestTimeout，
	// 否则单次 Provider 调用还没跑完就被上游掐断。
	CallTimeout           time.Duration
	SearchMaxAttempts     int
	ReprocessMaxAttempts  int
	RetryBaseBackoff      time.Duration
	RetryMaxBackoff       time.Duration
	BreakerWindow         time.Duration
	BreakerMinRequests    int
	BreakerFailureRate    float64
	BreakerOpenTimeout    time.Duration
	BreakerHalfOpenProbes int
}

// Registry 描述 etcd 服务注册发现参数。
type Registry struct {
	Enabled bool
	// Endpoints 是 etcd 集群地址列表。
	Endpoints   []string
	Username    string
	Password    string
	DialTimeout time.Duration
	// Prefix 是注册键前缀，实例键为 {Prefix}/{instanceID}。
	Prefix string
	// LeaseTTL 是租约时长：实例进程消失后最多这么久被摘除。
	LeaseTTL time.Duration
	// AdvertiseAddr 是写进 etcd 供其他实例 dial 的地址，容器里必须是
	// 对端可解析的地址而不是 :8082 这种监听串。
	AdvertiseAddr string
}

// Locking 描述 Outbox dispatcher 选主用的 Redis 锁参数。
type Locking struct {
	// DispatcherLeaderEnabled 打开后多副本 dispatcher 只有持锁者投递。
	DispatcherLeaderEnabled bool
	// LeaderKey 是选主锁的 Redis 键。
	LeaderKey string
	// LeaderTTL 是锁的过期时间，持锁期间由 watchdog 续期。
	LeaderTTL time.Duration
}

type AI struct {
	Enabled                     bool
	Provider                    string
	APIKey                      string
	BaseURL                     string
	ChatModel                   string
	EmbeddingModel              string
	VisionModel                 string
	RerankEnabled               bool
	RerankBaseURL               string
	RerankModel                 string
	RerankCandidates            int
	RerankTopK                  int
	RerankTimeout               time.Duration
	Dimension                   int
	MaxObjectBytes              int64
	RequestTimeout              time.Duration
	RateLimitEnabled            bool
	RateLimitSearchPerMinute    int
	RateLimitAskPerMinute       int
	RateLimitReprocessPerMinute int
}

func Load() (Config, error) {
	legacyRedisAddr := env("REDIS_ADDR", "127.0.0.1:6379")
	legacyRedisPassword := os.Getenv("REDIS_PASSWORD")
	legacyRedisDB := envInt("REDIS_DB", 0)
	postgresPrimaryDSN := os.Getenv("POSTGRES_PRIMARY_DSN")
	if strings.TrimSpace(postgresPrimaryDSN) == "" {
		postgresPrimaryDSN = env("POSTGRES_DSN", "postgres://yunpan:yunpan@127.0.0.1:5432/yunpan?sslmode=disable")
	}
	cfg := Config{
		App: App{
			Env:        env("APP_ENV", "development"),
			LogLevel:   env("LOG_LEVEL", "info"),
			InstanceID: serviceInstanceID(),
		},
		HTTP: HTTP{
			Addr:              env("HTTP_ADDR", ":8081"),
			ShutdownTimeout:   envDuration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
			ProbeTimeout:      envDuration("HTTP_PROBE_TIMEOUT", 2*time.Second),
			ReadHeaderTimeout: envDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			IdleTimeout:       envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			MaxBodyBytes:      envInt64("HTTP_MAX_BODY_BYTES", 1<<20),
			MaxHeaderBytes:    envInt("HTTP_MAX_HEADER_BYTES", 64<<10),
		},
		Postgres: Postgres{
			PrimaryDSN:      postgresPrimaryDSN,
			DSN:             postgresPrimaryDSN,
			MaxOpenConns:    envInt("POSTGRES_MAX_OPEN_CONNS", 32),
			MaxIdleConns:    envInt("POSTGRES_MAX_IDLE_CONNS", 8),
			ConnMaxLifetime: envDuration("POSTGRES_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnMaxIdleTime: envDuration("POSTGRES_CONN_MAX_IDLE_TIME", 5*time.Minute),
		},
		Redis: Redis{
			Addr: legacyRedisAddr, Password: legacyRedisPassword, DB: legacyRedisDB,
			CoordinationAddr:     env("REDIS_COORDINATION_ADDR", legacyRedisAddr),
			CoordinationPassword: env("REDIS_COORDINATION_PASSWORD", legacyRedisPassword),
			CoordinationDB:       envInt("REDIS_COORDINATION_DB", legacyRedisDB),
			CacheAddr:            env("REDIS_CACHE_ADDR", legacyRedisAddr),
			CachePassword:        env("REDIS_CACHE_PASSWORD", legacyRedisPassword),
			CacheDB:              envInt("REDIS_CACHE_DB", legacyRedisDB),
		},
		Cache: Cache{
			Enabled:    envBool("CACHE_ENABLED", true),
			TTL:        envDuration("CACHE_TTL", 5*time.Minute),
			L1MaxItems: envInt("CACHE_L1_MAX_ITEMS", 512),
		},
		Storage: Storage{
			Endpoint:         env("S3_ENDPOINT", "127.0.0.1:9000"),
			PublicEndpoint:   env("S3_PUBLIC_ENDPOINT", "127.0.0.1:3000"),
			PublicPathPrefix: env("S3_PUBLIC_PATH_PREFIX", "/storage"),
			AccessKey:        env("S3_ACCESS_KEY", "yunpan"),
			SecretKey:        env("S3_SECRET_KEY", "yunpan-dev-secret"),
			Bucket:           env("S3_BUCKET", "yunpan"),
			Region:           env("S3_REGION", "us-east-1"),
			Secure:           envBool("S3_SECURE", false),
			PublicSecure:     envBool("S3_PUBLIC_SECURE", false),
			ReadURLTTL:       envDuration("S3_READ_URL_TTL", 10*time.Minute),
		},
		Worker: Worker{
			Concurrency: envInt("WORKER_CONCURRENCY", 8),
			MetricsAddr: env("WORKER_METRICS_ADDR", ":8083"),
			Queues: map[string]int{
				"ai":          envInt("WORKER_QUEUE_AI", 4),
				"media":       envInt("WORKER_QUEUE_MEDIA", 4),
				"object":      envInt("WORKER_QUEUE_OBJECT", 2),
				"maintenance": envInt("WORKER_QUEUE_MAINTENANCE", 1),
			},
		},
		Auth: Auth{
			Issuer:       env("AUTH_ISSUER", "bx-yunpan"),
			SigningSeed:  env("AUTH_SIGNING_SEED", "bx-yunpan-development-signing-seed"),
			AccessTTL:    envDuration("AUTH_ACCESS_TTL", 15*time.Minute),
			RefreshTTL:   envDuration("AUTH_REFRESH_TTL", 30*24*time.Hour),
			CookieSecure: envBool("AUTH_COOKIE_SECURE", false),
			CookieDomain: os.Getenv("AUTH_COOKIE_DOMAIN"),
		},
		Identity: Identity{
			DefaultQuotaBytes: envInt64("USER_DEFAULT_QUOTA_BYTES", 10*1024*1024*1024),
		},
		Upload: Upload{
			SessionTTL:      envDuration("UPLOAD_SESSION_TTL", 24*time.Hour),
			PartURLTTL:      envDuration("UPLOAD_PART_URL_TTL", 15*time.Minute),
			CleanupInterval: envDuration("UPLOAD_CLEANUP_INTERVAL", 5*time.Minute),
			CleanupBatch:    envInt("UPLOAD_CLEANUP_BATCH", 100),
		},
		Outbox: Outbox{
			PollInterval:    envDuration("OUTBOX_POLL_INTERVAL", time.Second),
			BatchSize:       envInt("OUTBOX_BATCH_SIZE", 20),
			MaxRetry:        envInt("OUTBOX_TASK_MAX_RETRY", 8),
			TaskTimeout:     envDuration("OUTBOX_TASK_TIMEOUT", 30*time.Minute),
			GCDelay:         envDuration("OBJECT_GC_DELAY", 24*time.Hour),
			V2EventsEnabled: envBool("OUTBOX_V2_EVENTS_ENABLED", false),
		},
		TaskQueue: TaskQueue{
			URL:            env("RABBITMQ_URL", "amqp://guest:guest@127.0.0.1:5672/"),
			Exchange:       env("RABBITMQ_EXCHANGE", "yunpan.tasks"),
			Prefetch:       envInt("RABBITMQ_PREFETCH", 16),
			PublishTimeout: envDuration("RABBITMQ_PUBLISH_TIMEOUT", 5*time.Second),
			StopTimeout:    envDuration("TASK_CANCEL_GRACE", 10*time.Second),
		},
		Search: Search{
			Enabled:          envBool("SEARCH_INDEX_ENABLED", false),
			WriteBackends:    envList("SEARCH_WRITE_BACKENDS", []string{"postgres"}),
			ReadBackend:      env("SEARCH_READ_BACKEND", "postgres"),
			ShadowSampleRate: envFloat("SEARCH_SHADOW_SAMPLE_RATE", 0),
			IndexEpoch:       env("SEARCH_INDEX_EPOCH", "v1"),
			ESEndpoint:       env("SEARCH_ES_ENDPOINT", ""),
			ESUsername:       env("SEARCH_ES_USERNAME", ""),
			ESPassword:       os.Getenv("SEARCH_ES_PASSWORD"),
			QdrantEndpoint:   env("SEARCH_QDRANT_ENDPOINT", ""),
			QdrantAPIKey:     os.Getenv("SEARCH_QDRANT_API_KEY"),
			ExternalTimeout:  envDuration("SEARCH_EXTERNAL_TIMEOUT", 2*time.Second),
			WriteTimeout:     envDuration("SEARCH_EXTERNAL_WRITE_TIMEOUT", 10*time.Second),
			BatchSize:        envInt("SEARCH_INDEX_BATCH_SIZE", 100),
			VectorOversample: envInt("SEARCH_VECTOR_OVERSAMPLE", 4),
			ExternalFallback: envBool("SEARCH_EXTERNAL_FALLBACK", true),
		},
		Sharing: Sharing{
			Secret:    env("SHARE_SECRET", "bx-yunpan-development-share-secret"),
			AccessTTL: envDuration("SHARE_ACCESS_TTL", 15*time.Minute),
		},
		AI: AI{
			Enabled:                     envBool("AI_ENABLED", true),
			Provider:                    env("AI_PROVIDER", "fake"),
			APIKey:                      os.Getenv("DASHSCOPE_API_KEY"),
			BaseURL:                     env("AI_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
			ChatModel:                   env("AI_CHAT_MODEL", "qwen-plus"),
			EmbeddingModel:              env("AI_EMBEDDING_MODEL", "text-embedding-v4"),
			VisionModel:                 env("AI_VISION_MODEL", "qwen-vl-plus"),
			RerankEnabled:               envBool("AI_RERANK_ENABLED", false),
			RerankBaseURL:               env("AI_RERANK_BASE_URL", ""),
			RerankModel:                 env("AI_RERANK_MODEL", "qwen3-rerank"),
			RerankCandidates:            envInt("AI_RERANK_CANDIDATES", 30),
			RerankTopK:                  envInt("AI_RERANK_TOP_K", 8),
			RerankTimeout:               envDuration("AI_RERANK_TIMEOUT", 30*time.Second),
			Dimension:                   envInt("AI_EMBEDDING_DIMENSION", 1024),
			MaxObjectBytes:              int64(envInt("AI_MAX_OBJECT_MIB", 32)) * 1024 * 1024,
			RequestTimeout:              envDuration("AI_REQUEST_TIMEOUT", 90*time.Second),
			RateLimitEnabled:            envBool("AI_RATE_LIMIT_ENABLED", true),
			RateLimitSearchPerMinute:    envInt("AI_RATE_LIMIT_SEARCH_PER_MINUTE", 30),
			RateLimitAskPerMinute:       envInt("AI_RATE_LIMIT_ASK_PER_MINUTE", 10),
			RateLimitReprocessPerMinute: envInt("AI_RATE_LIMIT_REPROCESS_PER_MINUTE", 3),
		},
		AIService: AIService{
			Role:                  env("AI_SERVICE_ROLE", "combined"),
			GRPCAddr:              env("AISVC_GRPC_ADDR", ":8082"),
			Target:                env("AISVC_GRPC_TARGET", "127.0.0.1:8082"),
			CallTimeout:           envDuration("AISVC_CALL_TIMEOUT", 120*time.Second),
			SearchMaxAttempts:     envInt("AISVC_SEARCH_MAX_ATTEMPTS", 3),
			ReprocessMaxAttempts:  envInt("AISVC_REPROCESS_MAX_ATTEMPTS", 3),
			RetryBaseBackoff:      envDuration("AISVC_RETRY_BASE_BACKOFF", 20*time.Millisecond),
			RetryMaxBackoff:       envDuration("AISVC_RETRY_MAX_BACKOFF", time.Second),
			BreakerWindow:         envDuration("AISVC_BREAKER_WINDOW", 10*time.Second),
			BreakerMinRequests:    envInt("AISVC_BREAKER_MIN_REQUESTS", 20),
			BreakerFailureRate:    envFloat("AISVC_BREAKER_FAILURE_RATE", 0.5),
			BreakerOpenTimeout:    envDuration("AISVC_BREAKER_OPEN_TIMEOUT", 5*time.Second),
			BreakerHalfOpenProbes: envInt("AISVC_BREAKER_HALF_OPEN_PROBES", 3),
		},
		Tracing: Tracing{
			Enabled:      envBool("OTEL_TRACES_ENABLED", false),
			Endpoint:     env("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317"),
			SamplerRatio: envFloat("OTEL_TRACES_SAMPLER_ARG", 1.0),
			Environment:  env("APP_ENV", "development"),
			InstanceID:   serviceInstanceID(),
		},
		Registry: Registry{
			Enabled:       envBool("ETCD_ENABLED", false),
			Endpoints:     envList("ETCD_ENDPOINTS", []string{"etcd:2379"}),
			Username:      env("ETCD_USERNAME", ""),
			Password:      env("ETCD_PASSWORD", ""),
			DialTimeout:   envDuration("ETCD_DIAL_TIMEOUT", 5*time.Second),
			Prefix:        env("ETCD_SERVICE_PREFIX", "/services/aisvc"),
			LeaseTTL:      envDuration("ETCD_LEASE_TTL", 10*time.Second),
			AdvertiseAddr: env("AISVC_ADVERTISE_ADDR", ""),
		},
		Locking: Locking{
			DispatcherLeaderEnabled: envBool("OUTBOX_LEADER_LOCK_ENABLED", true),
			LeaderKey:               env("OUTBOX_LEADER_LOCK_KEY", "yunpan:outbox:dispatcher:leader"),
			LeaderTTL:               envDuration("OUTBOX_LEADER_LOCK_TTL", 15*time.Second),
		},
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.App.InstanceID) == "" {
		errs = append(errs, errors.New("SERVICE_INSTANCE_ID is required or hostname must be available"))
	}
	if strings.TrimSpace(c.HTTP.Addr) == "" {
		errs = append(errs, errors.New("HTTP_ADDR is required"))
	}
	if c.HTTP.ShutdownTimeout <= 0 || c.HTTP.ProbeTimeout <= 0 || c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.IdleTimeout <= 0 || c.HTTP.MaxBodyBytes <= 0 || c.HTTP.MaxHeaderBytes <= 0 {
		errs = append(errs, errors.New("HTTP timeouts and max body size must be positive"))
	}
	primaryDSN := c.Postgres.PrimaryDSN
	if primaryDSN == "" {
		primaryDSN = c.Postgres.DSN
	}
	if strings.TrimSpace(primaryDSN) == "" {
		errs = append(errs, errors.New("POSTGRES_PRIMARY_DSN or POSTGRES_DSN is required"))
	}
	if c.Postgres.MaxOpenConns <= 0 || c.Postgres.MaxIdleConns < 0 || c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns || c.Postgres.ConnMaxLifetime <= 0 || c.Postgres.ConnMaxIdleTime <= 0 {
		errs = append(errs, errors.New("Postgres pool settings are invalid"))
	}
	for name, addr := range map[string]string{
		"REDIS_ADDR":              c.Redis.Addr,
		"REDIS_COORDINATION_ADDR": c.Redis.CoordinationAddr, "REDIS_CACHE_ADDR": c.Redis.CacheAddr,
	} {
		if strings.TrimSpace(addr) == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	for name, db := range map[string]int{
		"REDIS_DB":              c.Redis.DB,
		"REDIS_COORDINATION_DB": c.Redis.CoordinationDB, "REDIS_CACHE_DB": c.Redis.CacheDB,
	} {
		if db < 0 {
			errs = append(errs, fmt.Errorf("%s cannot be negative", name))
		}
	}
	if c.Cache.TTL <= 0 || c.Cache.L1MaxItems <= 0 {
		errs = append(errs, errors.New("CACHE_TTL and CACHE_L1_MAX_ITEMS must be positive"))
	}
	if strings.TrimSpace(c.Storage.Endpoint) == "" || strings.TrimSpace(c.Storage.PublicEndpoint) == "" || strings.TrimSpace(c.Storage.Bucket) == "" || strings.TrimSpace(c.Storage.Region) == "" {
		errs = append(errs, errors.New("S3_ENDPOINT, S3_PUBLIC_ENDPOINT, S3_BUCKET, and S3_REGION are required"))
	}
	if prefix := c.Storage.PublicPathPrefix; prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		errs = append(errs, errors.New("S3_PUBLIC_PATH_PREFIX must start with / and must not end with /"))
	}
	if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
		errs = append(errs, errors.New("S3 credentials are required"))
	}
	if c.Storage.ReadURLTTL <= 0 {
		errs = append(errs, errors.New("S3_READ_URL_TTL must be positive"))
	}
	if strings.TrimSpace(c.Worker.MetricsAddr) == "" {
		errs = append(errs, errors.New("WORKER_METRICS_ADDR is required"))
	}
	if c.Worker.Concurrency <= 0 {
		errs = append(errs, errors.New("WORKER_CONCURRENCY must be positive"))
	}
	for name, weight := range c.Worker.Queues {
		if weight <= 0 {
			errs = append(errs, fmt.Errorf("worker queue %s must have a positive weight", name))
		}
	}
	if c.Auth.Issuer == "" || c.Auth.SigningSeed == "" || c.Auth.AccessTTL <= 0 || c.Auth.RefreshTTL <= 0 {
		errs = append(errs, errors.New("auth issuer, signing seed, and positive token TTLs are required"))
	}
	if c.App.Env == "production" && c.Auth.SigningSeed == "bx-yunpan-development-signing-seed" {
		errs = append(errs, errors.New("AUTH_SIGNING_SEED must be changed in production"))
	}
	if c.Identity.DefaultQuotaBytes <= 0 {
		errs = append(errs, errors.New("USER_DEFAULT_QUOTA_BYTES must be positive"))
	}
	if c.Upload.SessionTTL <= 0 || c.Upload.PartURLTTL <= 0 || c.Upload.CleanupInterval <= 0 || c.Upload.CleanupBatch <= 0 {
		errs = append(errs, errors.New("upload TTLs, cleanup interval, and cleanup batch must be positive"))
	}
	if c.Outbox.PollInterval <= 0 || c.Outbox.BatchSize <= 0 || c.Outbox.MaxRetry < 0 || c.Outbox.TaskTimeout <= 0 || c.Outbox.GCDelay < 0 {
		errs = append(errs, errors.New("outbox settings are invalid"))
	}
	if strings.TrimSpace(c.TaskQueue.URL) == "" || strings.TrimSpace(c.TaskQueue.Exchange) == "" || c.TaskQueue.Prefetch <= 0 || c.TaskQueue.PublishTimeout <= 0 || c.TaskQueue.StopTimeout <= 0 || c.TaskQueue.StopTimeout > time.Minute {
		errs = append(errs, errors.New("RabbitMQ task queue settings are invalid"))
	}
	if c.Search.ShadowSampleRate < 0 || c.Search.ShadowSampleRate > 1 {
		errs = append(errs, errors.New("SEARCH_SHADOW_SAMPLE_RATE must be in [0, 1]"))
	}
	if c.Search.ExternalTimeout <= 0 || c.Search.WriteTimeout <= 0 || c.Search.BatchSize <= 0 || c.Search.BatchSize > 10000 || c.Search.VectorOversample <= 0 || c.Search.VectorOversample > 100 {
		errs = append(errs, errors.New("search external timeout, batch size, and oversample settings are invalid"))
	}
	if strings.TrimSpace(c.Search.IndexEpoch) == "" {
		errs = append(errs, errors.New("SEARCH_INDEX_EPOCH is required"))
	}
	validSearchBackend := func(value string) bool {
		return value == "postgres" || value == "es_qdrant"
	}
	if !validSearchBackend(c.Search.ReadBackend) && c.Search.ReadBackend != "shadow" {
		errs = append(errs, errors.New("SEARCH_READ_BACKEND must be postgres, shadow, or es_qdrant"))
	}
	if len(c.Search.WriteBackends) == 0 {
		errs = append(errs, errors.New("SEARCH_WRITE_BACKENDS must not be empty"))
	}
	for _, backend := range c.Search.WriteBackends {
		if !validSearchBackend(backend) {
			errs = append(errs, fmt.Errorf("unsupported search write backend %q", backend))
		}
	}
	if contains(c.Search.WriteBackends, "es_qdrant") && (strings.TrimSpace(c.Search.ESEndpoint) == "" || strings.TrimSpace(c.Search.QdrantEndpoint) == "") {
		errs = append(errs, errors.New("SEARCH_WRITE_BACKENDS=es_qdrant requires SEARCH_ES_ENDPOINT and SEARCH_QDRANT_ENDPOINT"))
	}
	if c.Search.Enabled && c.Search.ReadBackend == "es_qdrant" && !contains(c.Search.WriteBackends, "es_qdrant") {
		errs = append(errs, errors.New("SEARCH_READ_BACKEND=es_qdrant requires es_qdrant in SEARCH_WRITE_BACKENDS"))
	}
	if c.Search.ReadBackend == "es_qdrant" {
		if strings.TrimSpace(c.Search.ESEndpoint) == "" || strings.TrimSpace(c.Search.QdrantEndpoint) == "" {
			errs = append(errs, errors.New("SEARCH_ES_ENDPOINT and SEARCH_QDRANT_ENDPOINT are required for es_qdrant reads"))
		}
	}
	if c.Sharing.Secret == "" || c.Sharing.AccessTTL <= 0 {
		errs = append(errs, errors.New("share secret and positive access TTL are required"))
	}
	if c.App.Env == "production" && c.Sharing.Secret == "bx-yunpan-development-share-secret" {
		errs = append(errs, errors.New("SHARE_SECRET must be changed in production"))
	}
	if c.AI.Enabled {
		if c.AI.Provider != "fake" && c.AI.Provider != "dashscope" {
			errs = append(errs, errors.New("AI_PROVIDER must be fake or dashscope when AI is enabled"))
		}
		if c.AI.Provider == "dashscope" && c.AI.APIKey == "" {
			errs = append(errs, errors.New("DASHSCOPE_API_KEY is required for dashscope"))
		}
		if c.AI.Dimension != 1024 || c.AI.MaxObjectBytes <= 0 || c.AI.RequestTimeout <= 0 {
			errs = append(errs, errors.New("AI embedding dimension must be 1024 and AI size/timeout settings must be positive"))
		}
		if c.AI.RerankCandidates <= 0 || c.AI.RerankTopK <= 0 || c.AI.RerankTopK > c.AI.RerankCandidates || c.AI.RerankTimeout <= 0 {
			errs = append(errs, errors.New("AI reranker candidate/top-k/timeout settings are invalid"))
		}
		if c.AI.RerankEnabled && strings.TrimSpace(c.AI.RerankModel) == "" {
			errs = append(errs, errors.New("AI_RERANK_MODEL is required when reranking is enabled"))
		}
		if c.AI.RerankEnabled && c.AI.Provider == "dashscope" && strings.TrimSpace(c.AI.RerankBaseURL) == "" {
			errs = append(errs, errors.New("AI_RERANK_BASE_URL is required for DashScope qwen3-rerank"))
		}
		if c.AI.RateLimitEnabled && (c.AI.RateLimitSearchPerMinute <= 0 || c.AI.RateLimitAskPerMinute <= 0 || c.AI.RateLimitReprocessPerMinute <= 0) {
			errs = append(errs, errors.New("enabled AI rate limits must be positive"))
		}
		if strings.TrimSpace(c.AIService.GRPCAddr) == "" || strings.TrimSpace(c.AIService.Target) == "" {
			errs = append(errs, errors.New("AISVC_GRPC_ADDR and AISVC_GRPC_TARGET are required"))
		}
		if c.AIService.Role != "combined" && c.AIService.Role != "query" && c.AIService.Role != "indexer" {
			errs = append(errs, errors.New("AI_SERVICE_ROLE must be combined, query, or indexer"))
		}
		// 三层 deadline 收敛：API 调用预算不能短于 aisvc 单次 Provider 调用超时，
		// 否则请求必然在下游还没做完时就被上游掐断。
		if c.AIService.CallTimeout < c.AI.RequestTimeout {
			errs = append(errs, errors.New("AISVC_CALL_TIMEOUT must not be shorter than AI_REQUEST_TIMEOUT"))
		}
		if c.AIService.SearchMaxAttempts <= 0 || c.AIService.ReprocessMaxAttempts <= 0 {
			errs = append(errs, errors.New("AISVC retry attempts must be positive"))
		}
		if c.AIService.RetryBaseBackoff <= 0 || c.AIService.RetryMaxBackoff < c.AIService.RetryBaseBackoff {
			errs = append(errs, errors.New("AISVC retry backoff settings are invalid"))
		}
		if c.AIService.BreakerWindow <= 0 || c.AIService.BreakerMinRequests <= 0 || c.AIService.BreakerOpenTimeout <= 0 || c.AIService.BreakerHalfOpenProbes <= 0 {
			errs = append(errs, errors.New("AISVC breaker settings must be positive"))
		}
		if c.AIService.BreakerFailureRate <= 0 || c.AIService.BreakerFailureRate > 1 {
			errs = append(errs, errors.New("AISVC_BREAKER_FAILURE_RATE must be in (0, 1]"))
		}
	}
	if c.Tracing.Enabled {
		if strings.TrimSpace(c.Tracing.Endpoint) == "" {
			errs = append(errs, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is required when tracing is enabled"))
		}
		if c.Tracing.SamplerRatio < 0 || c.Tracing.SamplerRatio > 1 {
			errs = append(errs, errors.New("OTEL_TRACES_SAMPLER_ARG must be in [0, 1]"))
		}
	}
	if c.AI.Enabled && c.Registry.Enabled {
		if len(c.Registry.Endpoints) == 0 {
			errs = append(errs, errors.New("ETCD_ENDPOINTS is required when etcd is enabled"))
		}
		if strings.TrimSpace(c.Registry.Prefix) == "" {
			errs = append(errs, errors.New("ETCD_SERVICE_PREFIX is required when etcd is enabled"))
		}
		if c.Registry.DialTimeout <= 0 {
			errs = append(errs, errors.New("ETCD_DIAL_TIMEOUT must be positive"))
		}
		// 租约太短会在网络抖动时误摘除健康实例：KeepAlive 的心跳间隔是
		// TTL/3，低于 3s 基本没有重试余量。
		if c.Registry.LeaseTTL < 3*time.Second {
			errs = append(errs, errors.New("ETCD_LEASE_TTL must be at least 3s"))
		}
	}
	if c.Locking.DispatcherLeaderEnabled {
		if strings.TrimSpace(c.Locking.LeaderKey) == "" {
			errs = append(errs, errors.New("OUTBOX_LEADER_LOCK_KEY is required when leader lock is enabled"))
		}
		// 锁 TTL 必须明显长于一轮轮询，否则每轮都在临界点上抢锁，
		// 主从会来回抖动。
		if c.Locking.LeaderTTL <= c.Outbox.PollInterval*2 {
			errs = append(errs, errors.New("OUTBOX_LEADER_LOCK_TTL must be longer than twice OUTBOX_POLL_INTERVAL"))
		}
	}
	return errors.Join(errs...)
}

func serviceInstanceID() string {
	if value := strings.TrimSpace(os.Getenv("SERVICE_INSTANCE_ID")); value != "" {
		return value
	}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		return strings.TrimSpace(hostname)
	}
	return "unknown"
}

// envList 解析逗号分隔的列表，忽略空白项。
func envList(name string, fallback []string) []string {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	items := make([]string, 0, 2)
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	if len(items) == 0 {
		return fallback
	}
	return items
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
