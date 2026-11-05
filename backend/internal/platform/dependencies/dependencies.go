package dependencies

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	redis "github.com/redis/go-redis/v9"
	"github.com/uptrace/opentelemetry-go-extra/otelgorm"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/health"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
)

type Set struct {
	// Redis is retained as the compatibility alias. New code should select the
	// client matching its responsibility explicitly.
	Redis             *redis.Client
	Tasks             *taskqueue.Broker
	RedisCoordination *redis.Client
	RedisCache        *redis.Client
	Storage           *minio.Client
	Presigner         *storageurl.Presigner
	GORM              *gorm.DB
	SQL               *sql.DB
	Bucket            string
}

func Open(ctx context.Context, cfg config.Config) (*Set, error) {
	redisCoordination := newRedisClient(cfg.Redis.CoordinationAddr, cfg.Redis.CoordinationPassword, cfg.Redis.CoordinationDB)
	redisCache := platformcache.NewRedisClient(cfg.Redis.CacheAddr, cfg.Redis.CachePassword, cfg.Redis.CacheDB)
	tasks := taskqueue.New(taskqueue.Config{URL: cfg.TaskQueue.URL, Exchange: cfg.TaskQueue.Exchange, PublishTimeout: cfg.TaskQueue.PublishTimeout})
	closeRedis := func() { _ = redisCoordination.Close(); _ = redisCache.Close(); _ = tasks.Close() }
	storageClient, err := minio.New(cfg.Storage.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""),
		Secure: cfg.Storage.Secure,
		Region: cfg.Storage.Region,
	})
	if err != nil {
		closeRedis()
		return nil, fmt.Errorf("create object storage client: %w", err)
	}
	presigner, err := storageurl.New(cfg.Storage.PublicEndpoint, cfg.Storage.AccessKey, cfg.Storage.SecretKey, cfg.Storage.Region, cfg.Storage.PublicSecure, cfg.Storage.PublicPathPrefix)
	if err != nil {
		closeRedis()
		return nil, fmt.Errorf("create object storage presigner: %w", err)
	}
	gormDB, sqlDB, err := openPostgres(ctx, cfg)
	if err != nil {
		closeRedis()
		return nil, err
	}

	return &Set{
		Redis:             redisCoordination,
		Tasks:             tasks,
		RedisCoordination: redisCoordination,
		RedisCache:        redisCache,
		Storage:           storageClient,
		Presigner:         presigner,
		GORM:              gormDB,
		SQL:               sqlDB,
		Bucket:            cfg.Storage.Bucket,
	}, nil
}

func openPostgres(ctx context.Context, cfg config.Config) (*gorm.DB, *sql.DB, error) {
	databaseLogger := gormlogger.New(log.New(os.Stdout, "", log.LstdFlags), gormlogger.Config{
		SlowThreshold: 500 * time.Millisecond, LogLevel: gormlogger.Warn, ParameterizedQueries: true,
	})
	primaryDSN := cfg.Postgres.PrimaryDSN
	if primaryDSN == "" {
		primaryDSN = cfg.Postgres.DSN
	}
	gormDB, err := gorm.Open(postgres.Open(primaryDSN), &gorm.Config{TranslateError: true, Logger: databaseLogger})
	if err != nil {
		return nil, nil, fmt.Errorf("create gorm database: %w", err)
	}
	// Database SQL spans follow the active request or task trace when tracing is enabled.
	if err := gormDB.Use(otelgorm.NewPlugin()); err != nil {
		return nil, nil, fmt.Errorf("install gorm tracing plugin: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("get sql database: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.Postgres.ConnMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}
	return gormDB, sqlDB, nil
}

func (s *Set) Close() {
	if s.Tasks != nil {
		_ = s.Tasks.Close()
	}
	if s.RedisCoordination != nil {
		_ = s.RedisCoordination.Close()
	}
	if s.RedisCache != nil {
		_ = s.RedisCache.Close()
	}
	if s.SQL != nil {
		_ = s.SQL.Close()
	}
}

func (s *Set) Probes() []health.Probe {
	return []health.Probe{
		health.ProbeFunc{
			ProbeName: "postgres",
			Func:      s.SQL.PingContext,
		},
		health.ProbeFunc{
			ProbeName: "object_storage",
			Func: func(ctx context.Context) error {
				_, err := s.Storage.BucketExists(ctx, s.Bucket)
				return err
			},
		},
	}
}

// AdvisoryProbes expose degraded derived/async planes without removing the
// core API from service. PostgreSQL remains able to commit business state and
// Outbox rows while RabbitMQ, Redis Coordination, or Cache is recovering.
func (s *Set) AdvisoryProbes() []health.Probe {
	return []health.Probe{
		health.ProbeFunc{ProbeName: "rabbitmq", Func: s.Tasks.Health},
		health.ProbeFunc{ProbeName: "redis_coordination", Func: func(ctx context.Context) error { return s.RedisCoordination.Ping(ctx).Err() }},
		health.ProbeFunc{ProbeName: "redis_cache", Func: func(ctx context.Context) error { return s.RedisCache.Ping(ctx).Err() }},
	}
}

func newRedisClient(addr, password string, db int) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
}
