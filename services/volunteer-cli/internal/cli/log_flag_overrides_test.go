package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TB-1 / TB-5 regression coverage for the two global logging flags.
//
// TB-1: `--log-level DEBUG` was matched case-sensitively against a lowercase
// switch whose default arm was info, so an uppercase value — or any typo — was
// neither honored nor refused; the volunteer simply got no debug logs and no
// explanation, which cost us the diagnostic data bug reports are made of.
//
// TB-5: the same flag was copied into the shared in-memory config, and the many
// command paths that call cfg.Save afterwards (registration on every `start`,
// `config set`, `heads trust`, `schedule set`, …) flushed it to config.yaml. A
// one-time override silently became the permanent setting.

// writeDefaultConfig writes a valid default config to a temp file and returns
// its path, so a command under test has something to load and save back.
func writeDefaultConfig(t *testing.T, dir string) string {
	t.Helper()
	c := config.Defaults()
	c.DataDir = dir
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := c.Save(cfgFile); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return cfgFile
}

// TestParseSlogLevelFoldsCase covers TB-1's parsing half directly: an uppercase
// level must mean what it says rather than falling through to info.
func TestParseSlogLevelFoldsCase(t *testing.T) {
	for _, in := range []string{"DEBUG", "Debug", " debug "} {
		if got := parseSlogLevel(in); got.String() != "DEBUG" {
			t.Errorf("parseSlogLevel(%q) = %v, want DEBUG", in, got)
		}
	}
	for _, in := range []string{"WARN", "Error"} {
		if got := parseSlogLevel(in); got.String() == "INFO" {
			t.Errorf("parseSlogLevel(%q) fell through to INFO", in)
		}
	}
}

// TestLogLevelFlagRejectsUnknownValue covers TB-1's validation half: a value the
// daemon cannot honor must be refused at flag time, not silently downgraded.
func TestLogLevelFlagRejectsUnknownValue(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	err := runCLI(t, "config", "get", "log_level", "--config", cfgFile, "--data-dir", dir, "--log-level", "verbose")
	if err == nil {
		t.Fatal("--log-level verbose was accepted; expected the command to refuse an unknown level")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error does not name the rejected value: %v", err)
	}
}

// TestLogLevelFlagAcceptsUppercase covers the other side of TB-1: a correct
// level in the wrong case must be accepted and take effect, not be refused by
// the new validation and not be quietly downgraded by the old switch.
func TestLogLevelFlagAcceptsUppercase(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	if err := runCLI(t, "config", "get", "log_level", "--config", cfgFile, "--data-dir", dir, "--log-level", "DEBUG"); err != nil {
		t.Fatalf("--log-level DEBUG was refused: %v", err)
	}
	if got := cfg.EffectiveLogLevel(); got != "debug" {
		t.Errorf("effective log level = %q, want %q", got, "debug")
	}
	if lvl := parseSlogLevel(cfg.EffectiveLogLevel()); lvl.String() != "DEBUG" {
		t.Errorf("logger would run at %v, want DEBUG", lvl)
	}
}

// TestLogLevelFlagReachesTheLogger closes the last step of TB-1. The symptom the
// tester reported was not "the accessor returns the wrong string" but "I asked
// for debug logs and got none", so the assertion that matters is about the
// logger every command actually builds, not the config accessor feeding it.
func TestLogLevelFlagReachesTheLogger(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	if err := runCLI(t, "config", "get", "log_level", "--config", cfgFile, "--data-dir", dir, "--log-level", "DEBUG"); err != nil {
		t.Fatalf("config get: %v", err)
	}
	logger, closeLogger := newLogger(cfg)
	defer closeLogger()
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("the logger built after `--log-level DEBUG` is not enabled at debug")
	}

	// Without the flag it must NOT be enabled at debug — otherwise the check
	// above would pass no matter what the flag did.
	if err := runCLI(t, "config", "get", "log_level", "--config", cfgFile, "--data-dir", dir); err != nil {
		t.Fatalf("config get: %v", err)
	}
	plain, closePlain := newLogger(cfg)
	defer closePlain()
	if plain.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("the logger is enabled at debug with no flag set; the assertion above cannot fail")
	}
}

// TestLogLevelFlagDoesNotPersist is TB-5: `config set` saves the whole config,
// so before the fix it carried the flag override to disk with it. The flag must
// change only this run.
func TestLogLevelFlagDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	// A command that deliberately writes config.yaml, run with the override.
	if err := runCLI(t, "config", "set", "max_concurrent_tasks", "2",
		"--config", cfgFile, "--data-dir", dir, "--log-level", "debug"); err != nil {
		t.Fatalf("config set: %v", err)
	}

	saved, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if saved.LogLevel != "info" {
		t.Errorf("log_level persisted as %q; a --log-level flag must not rewrite config.yaml (was %q)", saved.LogLevel, "info")
	}
	// The deliberate change did land, so the test is not passing because the
	// save was skipped altogether.
	if saved.MaxConcurrentTasks != 2 {
		t.Errorf("max_concurrent_tasks = %d, want 2 — the config was not saved at all", saved.MaxConcurrentTasks)
	}
}

// TestLogFileFlagDoesNotPersist is TB-5 for the sibling flag, which shares the
// wiring: --log-file must redirect this run's log without becoming the
// configured log_file.
func TestLogFileFlagDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)
	overrideLog := filepath.Join(dir, "one-off.log")

	if err := runCLI(t, "config", "set", "max_concurrent_tasks", "3",
		"--config", cfgFile, "--data-dir", dir, "--log-file", overrideLog); err != nil {
		t.Fatalf("config set: %v", err)
	}

	// The override is in force for this process...
	if got := cfg.LogFilePath(); got != overrideLog {
		t.Errorf("LogFilePath() = %q, want the --log-file override %q", got, overrideLog)
	}
	// ...but was not written to disk.
	saved, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if saved.LogFile != "" {
		t.Errorf("log_file persisted as %q; a --log-file flag must not rewrite config.yaml", saved.LogFile)
	}
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "one-off.log") {
		t.Errorf("config.yaml mentions the one-time --log-file override:\n%s", raw)
	}
}
