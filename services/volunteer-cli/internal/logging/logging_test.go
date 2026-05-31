package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWritesJSONToFile(t *testing.T) {
	dir := t.TempDir()
	// Nested path exercises directory creation.
	path := filepath.Join(dir, "logs", "volunteer.log")

	logger, closer, err := New(Options{
		Level:      slog.LevelInfo,
		File:       path,
		ToFile:     true,
		ToStderr:   false,
		MaxSizeMB:  10,
		MaxBackups: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello world", "answer", 42)
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%q)", err, line)
	}
	if rec["msg"] != "hello world" {
		t.Errorf("msg = %v, want %q", rec["msg"], "hello world")
	}
	if rec["answer"] != float64(42) {
		t.Errorf("answer = %v, want 42", rec["answer"])
	}
}

func TestNewRespectsLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "volunteer.log")

	logger, closer, err := New(Options{Level: slog.LevelInfo, File: path, ToFile: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Debug("debug-filtered-out")
	logger.Info("info-kept")
	closer.Close()

	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, "debug-filtered-out") {
		t.Errorf("debug record should have been filtered at info level: %s", got)
	}
	if !strings.Contains(got, "info-kept") {
		t.Errorf("info record missing: %s", got)
	}
}

func TestNewRotationBoundsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "volunteer.log")

	logger, closer, err := New(Options{
		Level:      slog.LevelInfo,
		File:       path,
		ToFile:     true,
		MaxSizeMB:  1, // rotate at ~1MB
		MaxBackups: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// ~10KB per record × 300 ≈ 3MB of logs, forcing multiple rotations.
	blob := strings.Repeat("x", 10*1024)
	for i := 0; i < 300; i++ {
		logger.Info("rotate", "i", i, "blob", blob)
	}
	closer.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	logFiles := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "volunteer") {
			logFiles++
		}
	}
	if logFiles < 2 {
		t.Errorf("expected rotation to produce backup files, found %d: %v", logFiles, entries)
	}

	// The active file must stay bounded near MaxSize rather than grow unbounded.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if max := int64(2 * 1024 * 1024); info.Size() > max {
		t.Errorf("active log file is %d bytes, expected it bounded under %d", info.Size(), max)
	}
}

func TestNewBothSinksDisabledDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "volunteer.log")

	// Level above what we emit, so the stderr fallback stays quiet during tests.
	logger, closer, err := New(Options{Level: slog.LevelError, File: path, ToFile: false, ToStderr: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("not written anywhere durable")
	if err := closer.Close(); err != nil {
		t.Fatalf("close should be a no-op: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no log file should be created when file logging is disabled (stat err: %v)", err)
	}
}
