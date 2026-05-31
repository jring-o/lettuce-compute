// Package logging builds the volunteer CLI's slog logger, fanning output out
// to stderr and/or a size-rotated JSON log file. Centralizing construction here
// means every command (and the long-running daemon) logs identically and gets
// durable, bounded log files with zero configuration.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Options configures logger construction.
type Options struct {
	Level slog.Level // already-parsed slog level

	File       string // resolved log file path (used only when ToFile)
	ToFile     bool   // write logs to the rotating file
	ToStderr   bool   // write logs to stderr
	MaxSizeMB  int    // rotate after the file reaches this size
	MaxBackups int    // number of rotated files to retain
	MaxAgeDays int    // max age of rotated files in days (0 = no limit)
}

// New constructs a JSON slog.Logger that fans out to stderr and/or a
// size-rotated log file according to opts. The returned io.Closer flushes and
// releases the underlying log file; it is a no-op when file logging is disabled
// and is always safe to call (typically via defer on daemon/command shutdown).
//
// If both sinks are disabled the logger falls back to stderr so logs are never
// silently discarded.
func New(opts Options) (*slog.Logger, io.Closer, error) {
	var writers []io.Writer
	var closer io.Closer = noopCloser{}

	if opts.ToFile && opts.File != "" {
		// Ensure the logs directory exists up front so a misconfigured or
		// unwritable path surfaces immediately rather than on the first log line
		// (slog silently swallows handler write errors).
		if err := os.MkdirAll(filepath.Dir(opts.File), 0o755); err != nil {
			return nil, nil, fmt.Errorf("creating log directory: %w", err)
		}
		lj := &lumberjack.Logger{
			Filename:   opts.File,
			MaxSize:    opts.MaxSizeMB,
			MaxBackups: opts.MaxBackups,
			MaxAge:     opts.MaxAgeDays,
			LocalTime:  true,
		}
		writers = append(writers, lj)
		closer = lj
	}
	if opts.ToStderr {
		writers = append(writers, os.Stderr)
	}

	var w io.Writer
	switch len(writers) {
	case 0:
		// Both sinks disabled — fall back to stderr rather than a black hole.
		w = os.Stderr
	case 1:
		w = writers[0]
	default:
		w = io.MultiWriter(writers...)
	}

	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: opts.Level}))
	return logger, closer, nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
