package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TB-75 regression test against a REAL container engine: two containers each
// granted one core of a 2-core budget use at most two cores between them,
// and a running container's quota can be rewritten in place through the
// engine's update call — the mechanism the live equal split depends on. The
// mocked engine cannot prove either: the field reproduction was `podman
// stats` reading ~200 % per container under a "2-core" limit, and whether
// Podman's Docker-compatible API honors the update endpoint is exactly the
// question the decision left to a real machine.
//
// Gated like the other real-engine tests: LETTUCE_TEST_REAL_ENGINE=1.

// realEngineCLI returns the engine binary the real-engine tests drive for
// harness plumbing (stats, inspect).
func realEngineCLI() string {
	if backend := DetectContainerBackend(BundledPodmanPath()); backend.Backend == BackendPodman {
		return backend.BinaryPath
	}
	return "docker"
}

// containerCPUQuota reads a container's CFS quota as the engine holds it.
func containerCPUQuota(t *testing.T, id string) int64 {
	t.Helper()
	out, err := exec.Command(realEngineCLI(), "inspect", "--format", "{{.HostConfig.CpuQuota}}", id).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect %s: %v\n%s", id, err, out)
	}
	q, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse quota %q: %v", out, err)
	}
	return q
}

// containerCPUPercent reads one container's CPU use from the engine's
// stats, as a percentage of one core (200 = two cores busy).
func containerCPUPercent(t *testing.T, id string) float64 {
	t.Helper()
	out, err := exec.Command(realEngineCLI(), "stats", "--no-stream", "--format", "{{.CPUPerc}}", id).CombinedOutput()
	if err != nil {
		t.Fatalf("stats %s: %v\n%s", id, err, out)
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(string(out)), "%"), 64)
	if err != nil {
		t.Fatalf("parse stats %q: %v", out, err)
	}
	return pct
}

func TestRealEngine_TB75_TwoContainersShareTheBudgetAndQuotasUpdateLive(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	// Four busy loops per container: each container WANTS four cores, so
	// only the quota can hold the pair to two.
	imageID := buildRealEngineImage(t, `for i in 1 2 3 4; do (while :; do :; done) & done; sleep 60`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dc := cr.Client()

	var ids []string
	for i := 0; i < 2; i++ {
		quota, period := CFSQuota(CPUShareCores(2, 2)) // one core each of a 2-core budget
		id, err := dc.ContainerCreate(ctx, &ContainerConfig{
			Image: imageID, CPUQuota: quota, CPUPeriod: period, NetworkMode: "none",
			Labels: map[string]string{WorkUnitIDLabel: fmt.Sprintf("tb75-%d", i), DataDirLabel: cr.dataDir},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, id)
		t.Cleanup(func() {
			_ = dc.ContainerStop(context.Background(), id, 2*time.Second)
			_ = dc.ContainerRemove(context.Background(), id)
		})
		if err := dc.ContainerStart(ctx, id); err != nil {
			t.Fatalf("start: %v", err)
		}
	}

	// Let the loops settle, then read the engine's own accounting: the pair
	// must stay at or under two cores (with a little sampling slack).
	time.Sleep(5 * time.Second)
	total := 0.0
	for _, id := range ids {
		pct := containerCPUPercent(t, id)
		t.Logf("container %s: %.1f%% CPU (quota %d)", shortImageID(id), pct, containerCPUQuota(t, id))
		total += pct
	}
	if total > 230 {
		t.Errorf("two containers granted one core each used %.1f%% combined (> 2 cores): the per-container quota is not holding the budget", total)
	}
	if total < 150 {
		t.Errorf("two containers granted one core each used only %.1f%% combined; the busy loops did not run, so the test proves nothing", total)
	}

	// The live adjustment: rewrite the first container's quota to half a
	// core and read it back from the engine.
	if err := cr.SetContainerCPU(ctx, ids[0], 0.5); err != nil {
		t.Fatalf("SetContainerCPU (the engine's update call): %v", err)
	}
	if got := containerCPUQuota(t, ids[0]); got != 50000 {
		t.Errorf("quota after the live update = %d, want 50000 (half a core)", got)
	}
	if got := containerCPUQuota(t, ids[1]); got != 100000 {
		t.Errorf("the other container's quota changed to %d; want 100000 untouched", got)
	}
	time.Sleep(4 * time.Second)
	if pct := containerCPUPercent(t, ids[0]); pct > 80 {
		t.Errorf("container capped at half a core still uses %.1f%%", pct)
	}
}
