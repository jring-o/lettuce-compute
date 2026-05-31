package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsLoggingFields(t *testing.T) {
	c := Defaults()
	if !c.LogToFile {
		t.Error("LogToFile should default to true")
	}
	if !c.LogToStderr {
		t.Error("LogToStderr should default to true")
	}
	if c.LogMaxSizeMB != 10 {
		t.Errorf("LogMaxSizeMB = %d, want 10", c.LogMaxSizeMB)
	}
	if c.LogMaxBackups != 5 {
		t.Errorf("LogMaxBackups = %d, want 5", c.LogMaxBackups)
	}
	if c.LogFile != "" {
		t.Errorf("LogFile should default to empty (resolved at runtime), got %q", c.LogFile)
	}
}

func TestLogFilePath(t *testing.T) {
	c := Defaults()
	c.DataDir = filepath.Join("custom", "data")

	want := filepath.Join("custom", "data", "logs", "volunteer.log")
	if got := c.LogFilePath(); got != want {
		t.Errorf("LogFilePath() = %q, want %q", got, want)
	}

	// Explicit override wins.
	c.LogFile = filepath.Join("else", "where.log")
	if got := c.LogFilePath(); got != c.LogFile {
		t.Errorf("LogFilePath() = %q, want explicit %q", got, c.LogFile)
	}
}

func TestSetGetByPathLoggingFields(t *testing.T) {
	tests := []struct {
		path  string
		value string
	}{
		{"log_file", "/var/log/volunteer.log"},
		{"log_to_file", "false"},
		{"log_to_stderr", "false"},
		{"log_max_size_mb", "25"},
		{"log_max_backups", "9"},
		{"log_max_age_days", "30"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c := Defaults()
			if err := c.SetByPath(tt.path, tt.value); err != nil {
				t.Fatalf("SetByPath(%q, %q) error: %v", tt.path, tt.value, err)
			}
			got, err := c.GetByPath(tt.path)
			if err != nil {
				t.Fatalf("GetByPath(%q) error: %v", tt.path, err)
			}
			if got != tt.value {
				t.Errorf("round-trip %q: got %q, want %q", tt.path, got, tt.value)
			}
		})
	}
}

func TestSetByPathLoggingInvalid(t *testing.T) {
	c := Defaults()
	if err := c.SetByPath("log_to_file", "maybe"); err == nil {
		t.Error("expected error for invalid boolean")
	}
	if err := c.SetByPath("log_max_size_mb", "big"); err == nil {
		t.Error("expected error for invalid integer")
	}
}

func TestValidateLoggingNegatives(t *testing.T) {
	for _, path := range []string{"log_max_size_mb", "log_max_backups", "log_max_age_days"} {
		c := Defaults()
		if err := c.SetByPath(path, "-1"); err != nil {
			t.Fatalf("SetByPath(%q): %v", path, err)
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate() should reject negative %s", path)
		}
	}
}

// Existing config files predate the log_* keys; loading one must keep the
// file/stderr defaults on (true) rather than silently disabling logging.
func TestLoadPreservesLoggingDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	legacy := "data_dir: " + dir + "\nlog_level: info\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.LogToFile || !c.LogToStderr {
		t.Errorf("legacy config should keep logging on by default: ToFile=%v ToStderr=%v", c.LogToFile, c.LogToStderr)
	}
	if c.LogMaxSizeMB != 10 || c.LogMaxBackups != 5 {
		t.Errorf("legacy config should keep rotation defaults: size=%d backups=%d", c.LogMaxSizeMB, c.LogMaxBackups)
	}
}
