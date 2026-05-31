package cli

import (
	"fmt"
	"log/slog"
	"os"

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
		Level:      parseSlogLevel(cfg.LogLevel),
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
			Level: parseSlogLevel(cfg.LogLevel),
		}))
		return logger, func() {}
	}
	return logger, func() { _ = closer.Close() }
}
