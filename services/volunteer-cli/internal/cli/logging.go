package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/logging"
)

// newLogger builds the logger every command shares: JSON records fanned out to
// stderr and a size-rotated file under <DataDir>/logs/ by default, honoring the
// configured log level and log_* settings. The returned closeLogger flushes and
// releases the log file and should be deferred by the caller.
//
// Logging setup never takes the CLI down: if the log file cannot be opened we
// warn once and fall back to stderr-only logging.
func newLogger(cfg *config.Config) (logger *slog.Logger, closeLogger func()) {
	logger, closer, err := logging.New(logging.Options{
		Level:      parseSlogLevel(cfg.EffectiveLogLevel()),
		File:       cfg.LogFilePath(),
		ToFile:     cfg.LogToFile,
		ToStderr:   cfg.LogToStderr,
		MaxSizeMB:  cfg.LogMaxSizeMB,
		MaxBackups: cfg.LogMaxBackups,
		MaxAgeDays: cfg.LogMaxAgeDays,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: file logging disabled: %v\n", err)
		logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: parseSlogLevel(cfg.EffectiveLogLevel()),
		}))
		return logger, func() {}
	}
	return logger, func() { _ = closer.Close() }
}

// normalizeLogLevel folds a log level to the canonical lowercase spelling
// parseSlogLevel understands, and refuses anything else. parseSlogLevel maps an
// unrecognized value to info, which is the right default for a config file that
// has already been validated but the wrong answer for a flag typed by hand: it
// turned `--log-level DEBUG` into a silent no-op (TB-1). Callers validating a
// flag should refuse; callers rendering an already-stored value should not.
func normalizeLogLevel(level string) (string, error) {
	canonical := strings.ToLower(strings.TrimSpace(level))
	switch canonical {
	case "debug", "info", "warn", "error":
		return canonical, nil
	default:
		return "", fmt.Errorf("invalid log level %q: must be one of debug, info, warn, error", level)
	}
}
