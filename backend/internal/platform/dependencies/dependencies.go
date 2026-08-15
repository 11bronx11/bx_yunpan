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
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/health"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

type Set struct {
	Redis     *redis.Client
	Storage   *minio.Client
	Presigner *storageurl.Presigner
	GORM      *gorm.DB
	SQL       *sql.DB
	Bucket    string
}

func Open(ctx context.Context, cfg config.Config) (*Set, error) {
	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	storageClient, err := minio.New(cfg.Storage.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""),
		Secure: cfg.Storage.Secure,
		Region: cfg.Storage.Region,
	})
	if err != nil {
		_ = redisClient.Close()
		return nil, fmt.Errorf("create object storage client: %w", err)
	}
	presigner, err := storageurl.New(cfg.Storage.PublicEndpoint, cfg.Storage.AccessKey, cfg.Storage.SecretKey, cfg.Storage.Region, cfg.Storage.PublicSecure, cfg.Storage.PublicPathPrefix)
	if err != nil {
		_ = redisClient.Close()
		return nil, fmt.Errorf("create object storage presigner: %w", err)
	}
	databaseLogger := gormlogger.New(log.New(os.Stdout, "", log.LstdFlags), gormlogger.Config{
		SlowThreshold: 500 * time.Millisecond, LogLevel: gormlogger.Warn, ParameterizedQueries: true,
	})
	gormDB, err := gorm.Open(postgres.Open(cfg.Postgres.DSN), &gorm.Config{TranslateError: true, Logger: databaseLogger})
	if err != nil {
		_ = redisClient.Close()
		return nil, fmt.Errorf("create gorm database: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		_ = redisClient.Close()
		return nil, fmt.Errorf("get sql database: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.Postgres.ConnMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &Set{
		Redis:     redisClient,
		Storage:   storageClient,
		Presigner: presigner,
		GORM:      gormDB,
		SQL:       sqlDB,
		Bucket:    cfg.Storage.Bucket,
	}, nil
}

func (s *Set) Close() {
	_ = s.Redis.Close()
	_ = s.SQL.Close()
}

func (s *Set) Probes() []health.Probe {
	return []health.Probe{
		health.ProbeFunc{
			ProbeName: "postgres",
			Func:      s.SQL.PingContext,
		},
		health.ProbeFunc{
			ProbeName: "redis",
			Func: func(ctx context.Context) error {
				return s.Redis.Ping(ctx).Err()
			},
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
