package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadProgressFile_Value: a normal progress.txt parses to its numeric value.
func TestReadProgressFile_Value(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "progress.txt"), []byte("42.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadProgressFile(dir); got != 42.5 {
		t.Errorf("ReadProgressFile = %v, want 42.5", got)
	}
}

// TestReadProgressFile_SymlinkRefused is the BG-15c guard for the progress reader:
// the progress file is leaf-controlled, so a symlinked progress.txt must be refused
// (and its target never read) rather than followed. Routing through the shared
// symlink-safe reader makes a symlink yield 0, not the target's contents.
func TestReadProgressFile_SymlinkRefused(t *testing.T) {
	root := t.TempDir()

	// A secret whose contents happen to parse as a large float; if the reader followed
	// the symlink, ReadProgressFile would return 100 (clamped) instead of 0.
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("9999"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(dir, "progress.txt")); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	if got := ReadProgressFile(dir); got != 0 {
		t.Errorf("ReadProgressFile followed a symlinked progress.txt: got %v, want 0", got)
	}
}
