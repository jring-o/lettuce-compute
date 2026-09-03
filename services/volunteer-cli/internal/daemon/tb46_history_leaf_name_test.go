package daemon

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression test for TB-46: history entries recorded only the head's leaf UUID,
// so the `history` command showed "4ba0f9cb-..." where a volunteer needed the leaf
// name — while the daemon had the id→name resolver in hand at the moment it wrote
// the line. The entry must now carry the name when the cache knows the leaf, and
// stay honest (no name) when it does not.
//
// The file is read raw so this compiles against the pre-fix HistoryEntry (which
// had no name field) and fails on the missing JSON key rather than on a build.
func TestTB46_RecordHistoryStoresLeafName(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	d := NewDaemon(DaemonConfig{Config: cfg, Logger: logger})

	const knownLeaf = "4ba0f9cb-0d6e-4f7e-9d0e-1c2b3a4d5e6f"
	d.leafCache.PopulateForTest("SciOS Compute", &CachedHeadInfo{
		Name: "SciOS Compute",
		Leafs: []CachedLeafInfo{{
			ID:   knownLeaf,
			Slug: "extract2-student-crowd-f14-gpu",
			Name: "Extract2 Student Crowd F14 GPU",
		}},
	})

	d.recordHistory(&runtime.WorkUnit{ID: "unit-25", LeafID: knownLeaf}, 400, 390, true, "SciOS Compute")
	d.recordHistory(&runtime.WorkUnit{ID: "unit-26", LeafID: "leaf-the-cache-never-saw"}, 10, 9, true, "SciOS Compute")

	raw, err := os.ReadFile(HistoryFilePath(dir))
	if err != nil {
		t.Fatalf("reading history file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("history lines = %d, want 2:\n%s", len(lines), raw)
	}

	if !strings.Contains(lines[0], `"leaf_name":"Extract2 Student Crowd F14 GPU"`) {
		t.Errorf("a unit of a leaf the cache knows must record the leaf's name beside its id (TB-46); got:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], `"leaf_id":"`+knownLeaf+`"`) {
		t.Errorf("the leaf id must still be recorded; got:\n%s", lines[0])
	}
	if strings.Contains(lines[1], `"leaf_name"`) {
		t.Errorf("an unknown leaf has no name to record — the id is not a name; got:\n%s", lines[1])
	}
}
