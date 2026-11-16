package dependencies

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

// A04: missing explicit connections cannot inherit a business write DSN.
func TestMissingExplicitRoleDSNDoesNotFallback(t *testing.T) {
	cfg := config.Config{}
	cfg.Postgres.PrimaryDSN = "postgres://invalid.invalid:1/business"
	if db, pool, err := OpenRoleDB(context.Background(), cfg, "", false); err == nil || db != nil || pool != nil {
		t.Fatal("missing explicit role DSN fell back")
	}
}

// A04: database identity/schema and actual privileges, not only unequal DSN
// strings, reject accidental business/control connections.
func TestAIRejectsBusinessAndControlDatabases(t *testing.T) {
	for _, key := range []string{"TEST_POSTGRES_DSN", "TEST_CONTROL_POSTGRES_DSN"} {
		t.Run(key, func(t *testing.T) {
			dsn := os.Getenv(key)
			if dsn == "" {
				t.Skip("dedicated migrated test DB required")
			}
			cfg := config.Config{}
			cfg.AISplit.QueryDSN = dsn
			cfg.Postgres.MaxOpenConns = 1
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, pool, err := OpenAI(ctx, cfg, "ai-query")
			if pool != nil {
				_ = pool.Close()
			}
			if err == nil || db != nil {
				t.Fatal("AI role accepted a non-AI database")
			}
		})
	}
}
