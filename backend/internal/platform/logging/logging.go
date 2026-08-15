package logging

import (
	"log/slog"
	"os"
	"strings"
)

func New(levelName, environment string) *slog.Logger {
	level := new(slog.LevelVar)
	switch strings.ToLower(levelName) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With(
		"service", "bx-yunpan",
		"environment", environment,
	)
}
