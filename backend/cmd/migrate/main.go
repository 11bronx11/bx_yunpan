package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	dsn := env("POSTGRES_PRIMARY_DSN", env("POSTGRES_DSN", "postgres://yunpan:yunpan@127.0.0.1:5432/yunpan?sslmode=disable"))
	source := env("MIGRATIONS_SOURCE", "file://migrations")
	command := "up"
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "archive" {
		source = env("AI_ARCHIVE_MIGRATIONS_SOURCE", "file://archive-migrations")
		args = args[1:]
		u, e := url.Parse(dsn)
		if e != nil {
			fail("invalid business DSN", nil)
		}
		q := u.Query()
		q.Set("x-migrations-table", "ai_archive_migrations")
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	if len(args) > 0 && args[0] == "control" {
		dsn = os.Getenv("ASYNC_CONTROL_DSN")
		source = env("CONTROL_MIGRATIONS_SOURCE", "file://control-migrations")
		args = args[1:]
		if dsn == "" {
			fail("ASYNC_CONTROL_DSN is required", nil)
		}
	}
	if len(args) > 0 && args[0] == "ai" {
		dsn = os.Getenv("AI_MIGRATION_DSN")
		source = env("AI_MIGRATIONS_SOURCE", "file://ai-migrations")
		args = args[1:]
		if dsn == "" {
			fail("AI_MIGRATION_DSN is required", nil)
		}
	}
	if len(args) > 0 {
		command = args[0]
	}

	migrator, err := migrate.New(source, dsn)
	if err != nil {
		fail("create migrator", err)
	}
	defer func() {
		sourceErr, databaseErr := migrator.Close()
		if sourceErr != nil || databaseErr != nil {
			slog.Warn("close migrator", "source_error", sourceErr, "database_error", databaseErr)
		}
	}()

	switch command {
	case "up":
		err = migrator.Up()
	case "down-one":
		err = migrator.Steps(-1)
	case "version":
		var dirty bool
		var version uint
		version, dirty, err = migrator.Version()
		if err == nil {
			fmt.Printf("version=%d dirty=%t\n", version, dirty)
		}
	default:
		fail("usage: migrate [control|ai|archive] [up|down-one|version]", nil)
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		fail("run migration", err)
	}
	slog.Info("migration complete", "command", command)
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func fail(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
