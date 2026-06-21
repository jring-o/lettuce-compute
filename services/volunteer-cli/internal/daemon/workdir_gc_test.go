package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func gcTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mkUUIDDir creates dataDir/tree/<uuid> and returns (uuidName, fullPath).
func mkUUIDDir(t *testing.T, dataDir, tree string) (string, string) {
	t.Helper()
	name := uuid.NewString()
	p := filepath.Join(dataDir, tree, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return name, p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestReapOrphanWorkDirs covers the IO core: orphan <uuid> dirs are removed across all three
// trees; owned dirs are preserved; non-uuid dirs are never touched; a missing tree is a no-op.
func TestReapOrphanWorkDirs(t *testing.T) {
	dataDir := t.TempDir()

	// Native tree: one owned, one orphan, one non-uuid dir.
	_, ownedNative := mkUUIDDir(t, dataDir, "work")
	_, orphanNative := mkUUIDDir(t, dataDir, "work")
	nonUUID := filepath.Join(dataDir, "work", "not-a-uuid")
	if err := os.MkdirAll(nonUUID, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop a file inside the orphan so RemoveAll has to recurse.
	if err := os.WriteFile(filepath.Join(orphanNative, "output.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Container tree: one orphan.
	_, orphanContainer := mkUUIDDir(t, dataDir, "container-work")
	// wasm tree: intentionally NOT created (missing-tree case).

	owned := map[string]struct{}{filepath.Clean(ownedNative): {}}

	removed := reapOrphanWorkDirs(dataDir, owned, gcTestLogger())

	if removed != 2 {
		t.Errorf("removed = %d, want 2 (orphanNative + orphanContainer)", removed)
	}
	if !exists(ownedNative) {
		t.Errorf("owned native dir was removed, must be preserved")
	}
	if exists(orphanNative) {
		t.Errorf("orphan native dir was NOT removed")
	}
	if exists(orphanContainer) {
		t.Errorf("orphan container dir was NOT removed")
	}
	if !exists(nonUUID) {
		t.Errorf("non-uuid dir was removed, must be left untouched")
	}
}

// TestReapOrphanWorkDirs_NoTreesIsNoop verifies a fresh data dir (no work trees yet) reaps
// nothing and does not error.
func TestReapOrphanWorkDirs_NoTreesIsNoop(t *testing.T) {
	if removed := reapOrphanWorkDirs(t.TempDir(), map[string]struct{}{}, gcTestLogger()); removed != 0 {
		t.Errorf("removed = %d on empty data dir, want 0", removed)
	}
}

// TestSlotManagerActiveWorkDirs verifies the accessor returns only active slots' work dirs.
func TestSlotManagerActiveWorkDirs(t *testing.T) {
	sm := NewSlotManager(3, gcTestLogger())
	// slot 0: active with a work dir; slot 1: active but no prep; slot 2: inactive.
	sm.slots[0].active = true
	sm.slots[0].prep = &runtime.PrepareResult{WorkDir: "/data/work/aaa"}
	sm.slots[1].active = true
	sm.slots[2].active = false
	sm.slots[2].prep = &runtime.PrepareResult{WorkDir: "/data/work/ccc"}

	dirs := sm.ActiveWorkDirs()
	if len(dirs) != 1 || dirs[0] != "/data/work/aaa" {
		t.Errorf("ActiveWorkDirs() = %v, want [/data/work/aaa]", dirs)
	}
}

// TestGcOrphanedWorkDirs_PreservesActiveAndBuffered is the end-to-end check: a dir owned by
// an active slot and a dir owned by a queued buffer item both survive; an unowned dir is reaped.
func TestGcOrphanedWorkDirs_PreservesActiveAndBuffered(t *testing.T) {
	dataDir := t.TempDir()
	logger := gcTestLogger()

	_, slotDir := mkUUIDDir(t, dataDir, "work")          // owned by an active slot
	_, bufDir := mkUUIDDir(t, dataDir, "container-work") // owned by a queued buffer item
	_, orphanDir := mkUUIDDir(t, dataDir, "work")        // owned by nobody

	d := &Daemon{
		cfg:           &config.Config{DataDir: dataDir},
		slotManager:   NewSlotManager(2, logger),
		prefetchQueue: NewPreFetchQueue(10, logger),
		logger:        logger,
	}
	// Active slot owns slotDir.
	d.slotManager.slots[0].active = true
	d.slotManager.slots[0].prep = &runtime.PrepareResult{WorkDir: slotDir}
	// Queued buffer item owns bufDir.
	if err := d.prefetchQueue.Push(&PreFetchItem{
		WU:   &runtime.WorkUnit{ID: uuid.NewString()},
		Prep: &runtime.PrepareResult{WorkDir: bufDir},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	d.gcOrphanedWorkDirs()

	if !exists(slotDir) {
		t.Errorf("active-slot work dir was reaped, must be preserved")
	}
	if !exists(bufDir) {
		t.Errorf("buffered work dir was reaped, must be preserved")
	}
	if exists(orphanDir) {
		t.Errorf("orphan work dir was NOT reaped")
	}
}
