package daemon

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// PB-30 regression coverage: the world-writable sandbox dirs (PB-23/PB-29) are
// contained solely by a 0o700 data dir, but MkdirAll never tightens a
// PRE-EXISTING looser directory — so `--data-dir` pointed at an existing 0755
// dir silently voided the shield. Daemon startup must enforce the invariant.
// POSIX-only: on Windows the check is deliberately skipped (ACL model).

func TestEnsureDataDirPrivate_TightensExistingLooseDir(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforceable on Windows; covered in the podman VM")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Undo any umask narrowing so the premise (a genuinely loose dir) holds.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	tightened, err := EnsureDataDirPrivate(dir)
	if err != nil {
		t.Fatalf("EnsureDataDirPrivate: %v", err)
	}
	if !tightened {
		t.Error("a pre-existing 0755 data dir must be reported as tightened (PB-30)")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %04o after enforcement, want 0700 (PB-30)", got)
	}
}

func TestEnsureDataDirPrivate_CreatesPrivateAndNoopsWhenAlreadyPrivate(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforceable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "fresh")

	tightened, err := EnsureDataDirPrivate(dir)
	if err != nil {
		t.Fatalf("EnsureDataDirPrivate (create): %v", err)
	}
	if tightened {
		t.Error("creating a fresh dir is not a tighten")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("fresh data dir mode = %04o, want 0700", got)
	}

	tightened, err = EnsureDataDirPrivate(dir)
	if err != nil {
		t.Fatalf("EnsureDataDirPrivate (noop): %v", err)
	}
	if tightened {
		t.Error("an already-private dir must not be reported as tightened")
	}
}
