package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	dsn := env("POSTGRES_DSN", "postgres://yunpan:yunpan@127.0.0.1:5432/yunpan?sslmode=disable")
	source := env("MIGRATIONS_SOURCE", "file://migrations")
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
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
		fail("usage: migrate [up|down-one|version]", nil)
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
