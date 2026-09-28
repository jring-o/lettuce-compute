package runtime

import (
	"context"
	"testing"
	"time"
)

// Against a REAL engine: a container unit that keeps three cores busy must
// report about three cores' worth of CPU time and a peak memory below its
// limit. Gated like the other real-engine tests (LETTUCE_TEST_REAL_ENGINE=1).
func TestRealEngine_ContainerResultReportsMeasuredUsage(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	// The shell takes ~100 MB, then three busy loops run for eight seconds:
	// never more than three busy processes at once.
	imageID := buildRealEngineImage(t,
		`x=$(head -c 100000000 /dev/zero | tr '\0' a); `+
			`for i in 1 2 3; do (timeout 8 sh -c 'while :; do :; done') & done; wait; `+
			`echo '{"done": true}' > "$LETTUCE_OUTPUT_DIR/output.json"`)

	const limitMB = 1024
	wu := &WorkUnit{
		ID:      "3c9e6d52-1f7a-4b8e-9c0d-5e6f7a8b9c01",
		LeafID:  "3c9e6d52-1f7a-4b8e-9c0d-5e6f7a8b9c02",
		Runtime: "container",
		ExecutionSpec: ExecutionSpec{
			Image:       imageID,
			MaxMemoryMB: limitMB,
		},
	}
	cr.SetMemoryCeilingMB(2048)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("container exited %d; execution.log:\n%s", result.ExitCode, ExecutionLogTail(prep.WorkDir))
	}

	m := result.Metrics
	cpu := m.CPUSecondsUser + m.CPUSecondsSystem
	wall := float64(m.WallClockSeconds)
	t.Logf("wall %d s, CPU %.1f s user + %.1f s system (%.2f cores), %d cores reported, peak %d MB",
		m.WallClockSeconds, m.CPUSecondsUser, m.CPUSecondsSystem, cpu/wall, m.CPUCoresUsed, m.PeakMemoryMB)
	// Up to one sampling interval at the end of the run goes unmeasured.
	if cpu < 3*8*0.6 {
		t.Errorf("CPU seconds = %.1f; three busy cores for 8 s must report most of 24 s", cpu)
	}
	if cpu > 3.3*wall {
		t.Errorf("CPU seconds = %.1f over %.0f s wall: more than three cores", cpu, wall)
	}
	if m.CPUCoresUsed < 2 || m.CPUCoresUsed > 3 {
		t.Errorf("CPUCoresUsed = %d, want 2..3", m.CPUCoresUsed)
	}
	if m.PeakMemoryMB < 50 || m.PeakMemoryMB >= limitMB {
		t.Errorf("PeakMemoryMB = %d; want the ~100 MB the container held, below its %d MB limit", m.PeakMemoryMB, limitMB)
	}
}
