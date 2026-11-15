// Package testdb provides dedicated, migrated schemas for integration fixtures.
package testdb

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Business(t *testing.T) *gorm.DB { return database(t, "TEST_POSTGRES_DSN", "migrations") }
func Control(t *testing.T) *gorm.DB {
	return database(t, "TEST_CONTROL_POSTGRES_DSN", "control-migrations")
}

func database(t *testing.T, envKey, migrations string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(envKey)
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("test database unavailable")
	}
	schema := "test_" + uuid.NewString()[:8]
	if err := admin.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatal("dedicated test DSN must be a PostgreSQL URL")
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := gorm.Open(postgres.Open(u.String()), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(8)
	t.Cleanup(func() {
		pool.Close()
		if err := admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; err != nil {
			t.Error(err)
		}
		ap, _ := admin.DB()
		ap.Close()
	})
	_, file, _, _ := runtime.Caller(0)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../../"+migrations+"/*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(b)).Error; err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), err)
		}
	}
	return db
}
