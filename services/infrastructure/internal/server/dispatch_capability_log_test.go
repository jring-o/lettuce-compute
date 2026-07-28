package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// capabilityStarveLines returns the capability-mismatch WARN records in buf. The
// in-flight-cap line has its own message and its own helper (starveLines) — the two
// causes are deliberately distinguishable in a log, because the operator's next
// step differs.
func capabilityStarveLines(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "advertised capabilities") {
			out = append(out, rec)
		}
	}
	return out
}

// gpuLeafFor builds an ACTIVE native leaf that demands a GPU with vramMB of VRAM.
func gpuLeafFor(id types.ID, vramMB int) *leaf.Leaf {
	lf := nativeLeaf(id, 1, false, 0)
	lf.ResourceRequirements.GPURequired = true
	lf.ResourceRequirements.MinGPUVRAMMB = vramMB
	return lf
}

// TestHandOut_CapabilityMismatch_IsLogged is TB-21's head-side half, and closes
// the piece TB-15 deferred. A machine refused on a capability dimension got an
// empty response identical to "no work exists", and on a production head the only
// record was a Debug-only tally — so diagnosing one took a database session, and
// the missing dimension had to be found by reading code. The head must now name
// the cause and print the budgets it judged the machine on.
func TestHandOut_CapabilityMismatch_IsLogged(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	now := time.Now()
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(gpuLeafFor(leafID, 8192), leafRepo)
	c.stageUnit(types.NewID(), leafID, 1, 0)

	// A machine with a GPU, but only 4096 MB of it allowed — the shape a 8 GB card
	// at the default 50% produces against an 8 GiB leaf. Its in-flight quota is
	// untouched, so the pre-existing WARN cannot be what fires.
	vol, host := types.NewID(), types.NewID()
	opts := hostOpts(vol, host, 4)
	opts.HasGPU = true
	opts.MaxGPUVRAMMB = 4096
	opts.GPUVendors = []string{"NVIDIA"}

	if r, _ := c.HandOut(vol, opts, 1); len(r) != 0 {
		t.Fatalf("hand-out = %d units, want 0 (leaf needs 8192 MB VRAM, machine allows 4096)", len(r))
	}

	lines := capabilityStarveLines(buf)
	if len(lines) != 1 {
		t.Fatalf("capability-mismatch log lines = %d, want 1 — a machine refused on a dimension it "+
			"may not be able to check must say so at a level a production head records; got log:\n%s",
			len(lines), buf.String())
	}
	rec := lines[0]
	if lvl, _ := rec["level"].(string); lvl != "WARN" {
		t.Errorf("level = %q, want WARN", lvl)
	}
	if got, _ := rec["host_id"].(string); got != host.String() {
		t.Errorf("host_id = %q, want %q", got, host.String())
	}
	// The whole diagnostic is "which of these is below what some leaf asked for",
	// so the machine's advertised budgets have to be in the line. Reading them out
	// of the database was the expensive half of every previous instance.
	if got, _ := rec["max_gpu_vram_mb"].(float64); int(got) != 4096 {
		t.Errorf("max_gpu_vram_mb = %v, want 4096", rec["max_gpu_vram_mb"])
	}
	if _, ok := rec["max_disk_mb"]; !ok {
		t.Errorf("line must carry max_disk_mb: %v", rec)
	}
	if _, ok := rec["max_cpu_cores"]; !ok {
		t.Errorf("line must carry max_cpu_cores: %v", rec)
	}
	tallyKey := "refused_" + rejectCapabilityMismatch.String()
	if got, _ := rec[tallyKey].(float64); int(got) < 1 {
		t.Errorf("line must carry the reject tally %q, got %v", tallyKey, rec)
	}
}

// TestHandOut_CapabilityMismatch_NotLoggedWhenWorkIsHandedOut is the wolf: the WARN
// must key on "this machine got nothing", not on "some candidate was refused".
// Leafs a machine cannot run are refused on every poll of a healthy fleet, and a
// line per poll would bury the real ones.
func TestHandOut_CapabilityMismatch_NotLoggedWhenWorkIsHandedOut(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	now := time.Now()
	c.now = func() time.Time { return now }

	// One leaf this machine cannot run, one it can.
	gpuID, plainID := types.NewID(), types.NewID()
	c.warm(gpuLeafFor(gpuID, 8192), leafRepo)
	c.warm(nativeLeaf(plainID, 1, false, 0), leafRepo)
	c.stageUnit(types.NewID(), gpuID, 1, 0)
	c.stageUnit(types.NewID(), plainID, 1, 0)

	vol, host := types.NewID(), types.NewID()
	opts := hostOpts(vol, host, 4)

	if r, _ := c.HandOut(vol, opts, 2); len(r) == 0 {
		t.Fatal("hand-out = 0 units, want at least the runnable one")
	}
	if lines := capabilityStarveLines(buf); len(lines) != 0 {
		t.Errorf("logged a capability-mismatch WARN while handing out work: %v", lines)
	}
}
