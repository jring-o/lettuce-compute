package runtime

import (
	"context"
	"testing"
	"time"
)

// TestRealEngine_MemoryKillReportsTheLimitTheContainerRanUnder: a container
// that keeps allocating is killed by the engine at the memory limit the unit
// declares, exits 137, and the result names that limit, the figure the
// daemon's kill note explains the exit by. Gated like the other real-engine
// tests (LETTUCE_TEST_REAL_ENGINE=1).
func TestRealEngine_MemoryKillReportsTheLimitTheContainerRanUnder(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	cr.SetMemoryCeilingMB(1024) // the volunteer's allowance: well above the leaf's figure
	// tail keeps the last lines of its input; /dev/zero has no line breaks, so
	// it holds everything it reads until the memory limit stops it.
	imageID := buildRealEngineImage(t, `tail /dev/zero`)

	wu := &WorkUnit{
		ID:              "3c9e1f42-7b8d-4e6a-9c05-1d2e3f4a5b60",
		LeafID:          "3c9e1f42-7b8d-4e6a-9c05-1d2e3f4a5b61",
		Runtime:         "container",
		DeadlineSeconds: 120,
		ExecutionSpec:   ExecutionSpec{Image: imageID, MaxMemoryMB: 64},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	res, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v (a container that is not stopped by its memory limit runs to the deadline)", err)
	}
	if res.ExitCode != 137 {
		t.Fatalf("exit code %d, want 137 (killed at the memory limit); log tail: %s",
			res.ExitCode, ExecutionLogTail(prep.WorkDir))
	}
	if res.MemoryLimitMB != 64 {
		t.Errorf("result reports a memory limit of %d MB, want the 64 MB the container was created with", res.MemoryLimitMB)
	}
	if peak := res.Metrics.PeakMemoryMB; peak > 0 && peak > 64 {
		t.Errorf("peak working set %d MB above the 64 MB limit", peak)
	}
}
