package dependencies

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"gorm.io/gorm"
)

// OpenRoleDB opens exactly one explicitly supplied DSN. It never falls back
// to a business connection when an AI setting is absent.
func OpenRoleDB(ctx context.Context, cfg config.Config, dsn string, readonly bool) (*gorm.DB, *sql.DB, error) {
	if dsn == "" {
		return nil, nil, errors.New("explicit role DSN required")
	}
	var err error
	if readonly {
		dsn, err = readOnlyDSN(dsn)
		if err != nil {
			return nil, nil, err
		}
	}
	cfg.Postgres.PrimaryDSN = dsn
	cfg.Postgres.DSN = dsn
	if cfg.Postgres.MaxOpenConns < 1 || cfg.Postgres.MaxOpenConns > 6 {
		cfg.Postgres.MaxOpenConns = 6
	}
	cfg.Postgres.MaxIdleConns = min(2, cfg.Postgres.MaxOpenConns)
	cfg.Postgres.ConnMaxLifetime = 30 * time.Minute
	cfg.Postgres.ConnMaxIdleTime = 5 * time.Minute
	return openPostgres(ctx, cfg)
}

func OpenAI(ctx context.Context, cfg config.Config, role string) (*gorm.DB, *sql.DB, error) {
	dsn := cfg.AISplit.CommitDSN
	if role == "ai-query" {
		dsn = cfg.AISplit.QueryDSN
	}
	if role == "ai-committer" {
		dsn = cfg.AISplit.CommitDSN
	}
	if role != "ai-query" {
		dsn = ProtocolDSN(dsn, cfg.Async.ProtocolVersion)
	}
	db, sqlDB, err := OpenRoleDB(ctx, cfg, dsn, role == "ai-query")
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*gorm.DB, *sql.DB, error) { _ = sqlDB.Close(); return nil, nil, e }
	var canWrite, canStage, canReadStage, canReadCheckpoints bool
	if err := db.Raw(`SELECT has_table_privilege(current_user,'ai_objects','INSERT,UPDATE,DELETE') OR has_table_privilege(current_user,'ai_commit_receipts','INSERT,UPDATE,DELETE'),COALESCE(has_table_privilege(current_user,to_regclass('ai_artifact_manifests'),'INSERT'),false),COALESCE(has_table_privilege(current_user,to_regclass('ai_artifact_manifests'),'SELECT'),false),has_table_privilege(current_user,'ai_summary_checkpoints','SELECT')`).Row().Scan(&canWrite, &canStage, &canReadStage, &canReadCheckpoints); err != nil {
		return fail(err)
	}
	if role == "ai-query" && (canWrite || canStage || canReadStage || canReadCheckpoints) {
		return fail(errors.New("AI query role has excess privileges"))
	}
	if role != "ai-query" && (!canWrite || cfg.Async.ProtocolVersion != 3 && !canStage) {
		return fail(errors.New("AI background worker requires staging and publication privileges"))
	}
	var canReadNodes bool
	if err := db.Raw("SELECT COALESCE(has_table_privilege(current_user,to_regclass('ai_summary_nodes'),'SELECT'),false)").Scan(&canReadNodes).Error; err != nil {
		return fail(err)
	}
	if role == "ai-query" && canReadNodes {
		return fail(errors.New("AI query role can read private summary nodes"))
	}
	if role == "ai-summary-worker" && !(canReadCheckpoints || cfg.Async.ProtocolVersion == 3 && canReadNodes) {
		return fail(errors.New("AI summary compute requires private checkpoint reads"))
	}
	if role == "ai-committer" && !canWrite {
		return fail(errors.New("AI committer requires publication privileges"))
	}
	return db, sqlDB, nil
}
