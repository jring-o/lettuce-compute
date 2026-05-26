package logging

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger creates a configured slog logger.
// level: one of "debug", "info", "warn", "error".
// format: "json" for JSON output, "text" for human-readable output.
func NewLogger(level string, format string) *slog.Logger {
	lvl := parseLevel(level)

	opts := &slog.HandlerOptions{
		Level:     lvl,
		AddSource: lvl <= slog.LevelDebug,
	}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// SetDefault sets the provided logger as the default slog logger.
func SetDefault(logger *slog.Logger) {
	slog.SetDefault(logger)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
