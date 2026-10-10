package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	search "github.com/11bronx11/bx_yunpan/backend/internal/search"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	mode := flag.String("mode", "backfill", "backfill, status, activate, or rollback")
	target := flag.String("target-version", env("SEARCH_INDEX_EPOCH", "v1"), "target index version")
	epoch := flag.Int64("index-epoch", search.ParseIndexEpoch(env("SEARCH_INDEX_EPOCH", "v1")), "numeric index epoch")
	batch := flag.Int("batch-size", envInt("SEARCH_INDEX_BATCH_SIZE", 100), "snapshot batch size")
	activate := flag.Bool("activate", false, "activate the new version after a successful backfill")
	flag.Parse()
	if *epoch <= 0 || *batch <= 0 || *batch > 10000 {
		fail(errors.New("index-epoch and batch-size must be positive; batch-size must be <= 10000"))
	}

	dsn := env("POSTGRES_PRIMARY_DSN", env("POSTGRES_DSN", ""))
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable", env("POSTGRES_USER", "yunpan"), env("POSTGRES_PASSWORD", "yunpan"), env("POSTGRES_PORT", "5432"), env("POSTGRES_DB", "yunpan"))
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		fail(fmt.Errorf("open postgres: %w", err))
	}
	sqlDB, err := db.DB()
	if err != nil {
		fail(fmt.Errorf("open postgres pool: %w", err))
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	projector := search.NewProjectorWithEpoch(db, *target, *epoch)
	if external := externalIndex(); external != nil {
		projector.WithExternalIndex(external)
	}

	switch *mode {
	case "backfill":
		report, err := projector.Backfill(ctx, search.BackfillOptions{TargetVersion: *target, IndexEpoch: *epoch, BatchSize: *batch, Activate: *activate})
		if err != nil {
			fail(err)
		}
		writeJSON(report)
	case "status":
		for _, backend := range []string{search.BackendPostgres, search.BackendES, search.BackendQdrant} {
			rows, err := projector.IndexVersions(ctx, backend, *target)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				fail(err)
			}
			if len(rows) > 0 {
				writeJSON(map[string]any{"backend": backend, "versions": rows})
			}
		}
	case "activate":
		activateTarget(ctx, projector, *target)
	case "rollback":
		rollbackTarget(ctx, projector, *target)
	default:
		fail(errors.New("mode must be backfill, status, activate, or rollback"))
	}
}

func externalIndex() search.Index {
	cfg := config.Search{ESEndpoint: os.Getenv("SEARCH_ES_ENDPOINT"), ESUsername: os.Getenv("SEARCH_ES_USERNAME"), ESPassword: os.Getenv("SEARCH_ES_PASSWORD"), QdrantEndpoint: os.Getenv("SEARCH_QDRANT_ENDPOINT"), QdrantAPIKey: os.Getenv("SEARCH_QDRANT_API_KEY")}
	client := &http.Client{Timeout: durationEnv("SEARCH_EXTERNAL_WRITE_TIMEOUT", 10*time.Second)}
	var indexes []search.Index
	if cfg.ESEndpoint != "" {
		indexes = append(indexes, search.NewElasticsearchIndex(cfg.ESEndpoint, cfg.ESUsername, cfg.ESPassword, client))
	}
	if cfg.QdrantEndpoint != "" {
		indexes = append(indexes, search.NewQdrantIndex(cfg.QdrantEndpoint, cfg.QdrantAPIKey, envInt("AI_EMBEDDING_DIMENSION", 1024), client))
	}
	if len(indexes) == 0 {
		return nil
	}
	return search.NewExternalIndex(indexes...)
}

func activateTarget(ctx context.Context, projector *search.Projector, target string) {
	for _, backend := range []string{search.BackendPostgres, search.BackendES, search.BackendQdrant} {
		rows, err := projector.IndexVersions(ctx, backend, target)
		if err != nil {
			fail(err)
		}
		for _, row := range rows {
			if row.State == "building" {
				if err := projector.ActivateVersion(ctx, row.ID); err != nil {
					fail(err)
				}
			}
		}
	}
}

func rollbackTarget(ctx context.Context, projector *search.Projector, target string) {
	for _, backend := range []string{search.BackendPostgres, search.BackendES, search.BackendQdrant} {
		for _, projection := range []string{search.ProjectionFile, search.ProjectionContent} {
			if err := projector.RollbackVersion(ctx, backend, projection, target); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				fail(err)
			}
		}
	}
}

func writeJSON(value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		fail(err)
	}
	fmt.Println(string(encoded))
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	var value int
	if _, err := fmt.Sscanf(os.Getenv(name), "%d", &value); err == nil && value > 0 {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	if parsed, err := time.ParseDuration(os.Getenv(name)); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

func fail(err error) {
	slog.Error("search backfill failed", "error", err)
	os.Exit(1)
}
