//go:build darwin

package resource

import (
	"os"
	"path/filepath"
	"testing"
)

// When ioreg cannot run, macOS used to read as "0 seconds idle, no error".
func TestGetIdleSeconds_IoregMissingIsAnError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if secs, err := GetIdleSeconds(); err == nil {
		t.Errorf("GetIdleSeconds() without ioreg = (%d, nil), want an error", secs)
	}
}

// ioreg output with no HIDIdleTime is the same failure.
func TestGetIdleSeconds_NoHIDIdleTimeIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ioreg"), []byte("#!/bin/sh\necho '+-o Root  <class IORegistryEntry>'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if secs, err := GetIdleSeconds(); err == nil {
		t.Errorf("GetIdleSeconds() with no HIDIdleTime = (%d, nil), want an error", secs)
	}
}
