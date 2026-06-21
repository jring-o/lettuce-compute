package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateHostID_UniqueAndHex(t *testing.T) {
	a, err := GenerateHostID()
	if err != nil {
		t.Fatalf("GenerateHostID: %v", err)
	}
	b, err := GenerateHostID()
	if err != nil {
		t.Fatalf("GenerateHostID: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("GenerateHostID returned empty")
	}
	if a == b {
		t.Errorf("two generated host ids collided: %q", a)
	}
	if len(a) != hostIDBytes*2 {
		t.Errorf("host id length = %d, want %d hex chars", len(a), hostIDBytes*2)
	}
}

func TestSaveLoadHostID_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.id")

	if HostIDExists(path) {
		t.Fatal("HostIDExists true before any write")
	}
	want, err := GenerateHostID()
	if err != nil {
		t.Fatalf("GenerateHostID: %v", err)
	}
	if err := SaveHostID(path, want); err != nil {
		t.Fatalf("SaveHostID: %v", err)
	}
	if !HostIDExists(path) {
		t.Fatal("HostIDExists false after write")
	}
	got, err := LoadHostID(path)
	if err != nil {
		t.Fatalf("LoadHostID: %v", err)
	}
	if got != want {
		t.Errorf("LoadHostID = %q, want %q", got, want)
	}
}

// LoadOrCreateHostID is stable: it generates once and returns the SAME id thereafter, so a
// machine keeps one identity across restarts (and an install predating the host split picks
// one up transparently).
func TestLoadOrCreateHostID_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.id")

	first, err := LoadOrCreateHostID(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostID (create): %v", err)
	}
	if first == "" {
		t.Fatal("LoadOrCreateHostID returned empty")
	}
	second, err := LoadOrCreateHostID(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostID (load): %v", err)
	}
	if first != second {
		t.Errorf("LoadOrCreateHostID not stable: %q then %q", first, second)
	}
}

// An empty host-id file is treated as absent and replaced with a fresh id.
func TestLoadOrCreateHostID_EmptyFileRegenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.id")
	if err := os.WriteFile(path, []byte("  \n"), 0644); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	got, err := LoadOrCreateHostID(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostID: %v", err)
	}
	if got == "" {
		t.Error("expected a regenerated host id for an empty file")
	}
}
