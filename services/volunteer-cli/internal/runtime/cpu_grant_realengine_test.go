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

// CPU grant test against a REAL container engine (TB-75): two
// containers granted 3 and 1 of 4 cores each use their own grant, and no more
// than the 4 between them, while each wants four. The mocked engine cannot
// prove it: the field reproduction was `podman stats` reading ~200 % per
// container under a "2-core" limit.
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

func TestRealEngine_EachContainerIsHeldToItsGrant(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	// Four busy loops per container: each WANTS four cores, so only its
	// quota holds it to its grant.
	imageID := buildRealEngineImage(t, `for i in 1 2 3 4; do (while :; do :; done) & done; sleep 60`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dc := cr.Client()

	grants := []int{3, 1} // a wide task's grant and a narrow one's, 4 cores between them
	var ids []string
	for i, cores := range grants {
		quota, period := CFSQuota(cores)
		id, err := dc.ContainerCreate(ctx, &ContainerConfig{
			Image: imageID, CPUQuota: quota, CPUPeriod: period, NetworkMode: "none",
			Labels: map[string]string{WorkUnitIDLabel: fmt.Sprintf("grant-%d", i), DataDirLabel: cr.dataDir},
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

	// Let the loops settle, then read the engine's own accounting: each
	// container at its own grant (with sampling slack), the pair within the
	// four cores granted.
	time.Sleep(5 * time.Second)
	total := 0.0
	for i, id := range ids {
		pct := containerCPUPercent(t, id)
		t.Logf("container %s granted %d cores: %.1f%% CPU (quota %d)", shortImageID(id), grants[i], pct, containerCPUQuota(t, id))
		want := float64(grants[i] * 100)
		if pct > want+30 {
			t.Errorf("container granted %d cores used %.1f%%: its quota is not holding it to its grant", grants[i], pct)
		}
		if pct < want-50 {
			t.Errorf("container granted %d cores used only %.1f%%; the busy loops did not run, so the test proves nothing", grants[i], pct)
		}
		total += pct
	}
	if total > 430 {
		t.Errorf("two containers granted 4 cores between them used %.1f%% (> 4 cores)", total)
	}
}

// TestRealEngine_ThrottlingShowsATaskThatWantsMore: under the hardened
// posture every task container runs with, a container granted 1 core that
// runs 4 busy threads is stopped by its quota in most of its periods, while
// one granted 2 cores that runs 2 is stopped in few — the reading the
// daemon's throttle notice is built on, taken the way it takes it (the
// engine's stats, else the container's own cpu.stat).
func TestRealEngine_ThrottlingShowsATaskThatWantsMore(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	imageID := buildRealEngineImage(t, `i=0; while [ $i -lt ${LOOPS:-4} ]; do (while :; do :; done) & i=$((i+1)); done; sleep 60`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dc := cr.Client()

	start := func(name string, cores, loops int) string {
		quota, period := CFSQuota(cores)
		id, err := dc.ContainerCreate(ctx, &ContainerConfig{
			Image: imageID, CPUQuota: quota, CPUPeriod: period, CPUShares: LowestCPUShares, NetworkMode: "none",
			Env:            []string{fmt.Sprintf("LOOPS=%d", loops)},
			User:           "65534:65534",
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Labels:         map[string]string{WorkUnitIDLabel: name, DataDirLabel: cr.dataDir},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = dc.ContainerStop(context.Background(), id, 2*time.Second)
			_ = dc.ContainerRemove(context.Background(), id)
		})
		if err := dc.ContainerStart(ctx, id); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return id
	}
	wants := start("grant-wants-more", 1, 4)
	fits := start("grant-fits", 2, 2)

	time.Sleep(3 * time.Second)
	before := map[string]CPUThrottle{}
	for _, id := range []string{wants, fits} {
		r, err := ContainerCPUThrottling(ctx, dc, id)
		if err != nil {
			t.Fatalf("no throttling reading for %s: %v", shortImageID(id), err)
		}
		before[id] = r
	}
	time.Sleep(10 * time.Second)
	share := func(id string) float64 {
		r, err := ContainerCPUThrottling(ctx, dc, id)
		if err != nil {
			t.Fatalf("no throttling reading for %s: %v", shortImageID(id), err)
		}
		periods := r.Periods - before[id].Periods
		if periods < 50 {
			t.Fatalf("container %s ran only %d periods in 10 s; the busy loops did not run", shortImageID(id), periods)
		}
		s := float64(r.Throttled-before[id].Throttled) / float64(periods)
		t.Logf("container %s: stopped in %d of %d periods (%.0f%%)", shortImageID(id), r.Throttled-before[id].Throttled, periods, s*100)
		return s
	}
	if s := share(wants); s < 0.5 {
		t.Errorf("a container granted 1 core running 4 busy threads was stopped in only %.0f%% of its periods; want at least 50%%", s*100)
	}
	if s := share(fits); s > 0.2 {
		t.Errorf("a container granted 2 cores running 2 busy threads was stopped in %.0f%% of its periods; want under 20%%", s*100)
	}
}
