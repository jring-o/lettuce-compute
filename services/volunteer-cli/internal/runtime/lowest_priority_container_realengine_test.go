package runtime

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRealEngine_TaskContainerRunsAtTheLowestCPUWeight runs a unit through the
// production container runtime against a REAL engine and checks the weight two
// ways: the engine's own record of the container (HostConfig.CpuShares 2), and
// the container's cgroup as the kernel holds it (cpu.weight 1, read from inside
// the container). The mocked engine can prove only that the request asked.
//
// Gated like the other real-engine tests: LETTUCE_TEST_REAL_ENGINE=1.
func TestRealEngine_TaskContainerRunsAtTheLowestCPUWeight(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	imageID := buildRealEngineImage(t, `cat /sys/fs/cgroup/cpu.weight > "$LETTUCE_OUTPUT_DIR/output.json" 2>&1 || echo unreadable > "$LETTUCE_OUTPUT_DIR/output.json"; sleep 4`)

	wu := &WorkUnit{
		ID:      "3f6a9c20-1b2d-4e3f-8a9b-0c1d2e3f4a5b",
		LeafID:  "3f6a9c20-1b2d-4e3f-8a9b-0c1d2e3f4a5c",
		Runtime: "container",
		ExecutionSpec: ExecutionSpec{
			Image: imageID,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Read the engine's record while the container runs: the runtime removes
	// it once the unit finishes.
	var inspected, inspectErr string
	prep.ContainerIDCallback = func(containerID string) {
		out, err := exec.Command(realEngineCLI(), "inspect", "--format", "{{.HostConfig.CpuShares}}", containerID).CombinedOutput()
		inspected = strings.TrimSpace(string(out))
		if err != nil {
			inspectErr = err.Error()
		}
	}

	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("container exited %d; execution.log:\n%s", result.ExitCode, ExecutionLogTail(prep.WorkDir))
	}

	if inspectErr != "" {
		t.Fatalf("inspect the running container: %s\n%s", inspectErr, inspected)
	}
	if inspected != "2" {
		t.Errorf("engine records HostConfig.CpuShares = %q, want \"2\" (the lowest weight)", inspected)
	}
	weight := strings.TrimSpace(string(result.OutputData))
	t.Logf("engine CpuShares %s; cpu.weight inside the container: %q", inspected, weight)
	if weight != "1" {
		t.Errorf("cpu.weight inside the container = %q, want \"1\" (the default is 100)", weight)
	}
}
