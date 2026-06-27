package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadDetectsUnknownKeys is the issue #51 regression: a config carried over
// from an older release can contain keys this version no longer recognizes. They
// must still load (the recognized keys apply) but be surfaced as advisories
// rather than silently dropped.
func TestLoadDetectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
data_dir: /tmp/lv
work_buffer_hours: 3
totally_made_up_key: 5
leafs:
  mode: ALL
  some_removed_option: true
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Recognized keys are still applied despite the unknown ones.
	if cfg.WorkBufferHours != 3 {
		t.Errorf("work_buffer_hours = %v, want 3 (recognized key still applied)", cfg.WorkBufferHours)
	}

	warnings := cfg.DeprecatedKeyWarnings()
	if len(warnings) == 0 {
		t.Fatal("expected warnings for unknown keys, got none")
	}
	joined := strings.Join(warnings, "\n")
	for _, key := range []string{"totally_made_up_key", "some_removed_option"} {
		if !strings.Contains(joined, key) {
			t.Errorf("warnings should name unknown key %q; got:\n%s", key, joined)
		}
	}
}

// TestLoadRenamedKeyGivesActionableHint is the concrete upgrade case from issue
// #51: a pre-rename key (work_buffer_size, since renamed to work_buffer_hours)
// must produce a warning that names the current key, not just "unknown".
func TestLoadRenamedKeyGivesActionableHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "data_dir: /tmp/lv\nwork_buffer_size: 5\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := strings.Join(cfg.DeprecatedKeyWarnings(), "\n")
	if !strings.Contains(warnings, "work_buffer_size") {
		t.Errorf("warning should name the old key; got: %q", warnings)
	}
	if !strings.Contains(warnings, "work_buffer_hours") {
		t.Errorf("warning should point at the new key work_buffer_hours; got: %q", warnings)
	}
}

// TestLoadCleanConfigHasNoUnknownKeyWarnings verifies a config using only known
// keys produces no advisories.
func TestLoadCleanConfigHasNoUnknownKeyWarnings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
data_dir: /tmp/lv
work_buffer_hours: 2
leafs:
  mode: ALL
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if w := cfg.DeprecatedKeyWarnings(); len(w) != 0 {
		t.Errorf("clean config produced unexpected warnings: %v", w)
	}
}
