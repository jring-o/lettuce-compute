package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// newLogger must resolve the file under <DataDir>/logs/volunteer.log and write
// JSON records there so operators can attach the log without shell redirection.
func TestNewLoggerWritesToDataDir(t *testing.T) {
	dir := t.TempDir()
	c := config.Defaults()
	c.DataDir = dir
	c.LogToStderr = false // keep the test output clean; the file is what we assert
	c.LogLevel = "info"

	logger, closeLogger := newLogger(c)
	logger.Info("daemon up", "servers", 2)
	closeLogger()

	path := filepath.Join(dir, "logs", "volunteer.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", path, err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if rec["msg"] != "daemon up" {
		t.Errorf("msg = %v, want %q", rec["msg"], "daemon up")
	}
}

// An explicit --log-file (cfg.LogFile) overrides the default data-dir path.
func TestNewLoggerHonorsExplicitFile(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "custom", "vol.log")
	c := config.Defaults()
	c.DataDir = dir
	c.LogFile = explicit
	c.LogToStderr = false

	logger, closeLogger := newLogger(c)
	logger.Info("hi")
	closeLogger()

	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("expected log at explicit path %s: %v", explicit, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "volunteer.log")); !os.IsNotExist(err) {
		t.Errorf("default path should not be used when log_file is set")
	}
}
