package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRealEngine_CPUTimeLimitHalvesCPUUseWithoutPressure runs the daemon's
// own loop against a real container engine: one container unit whose program
// keeps one core busy, given its one core, under a 50 % CPU time limit (5 s
// running of every 10 s). Over three periods the container's own cgroup
// accounting shows about half a core used, and no CPU "full" pressure — the
// task is paused, not held back by a quota, so nothing stalls inside it. A
// quota-based limit is what showed 47–72 % pressure in the same setup.
func TestRealEngine_CPUTimeLimitHalvesCPUUseWithoutPressure(t *testing.T) {
	if os.Getenv(realEngineEnv) == "" {
		t.Skipf("real-engine test: set %s=1 (requires a working podman or docker)", realEngineEnv)
	}
	realEngineExecutors(t)
	backend := runtime.DetectContainerBackend(runtime.BundledPodmanPath())
	if backend.Backend == runtime.BackendNone {
		t.Skip("no container backend detected on this host")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cr, err := runtime.NewContainerRuntimeForBackend(t.TempDir(), logger, backend)
	if err != nil {
		t.Skipf("container backend %s detected but not initializable: %v", backend.Backend, err)
	}
	t.Cleanup(func() { cr.Client().Close() })
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := cr.Client().Ping(pingCtx); err != nil {
		t.Skipf("container backend %s socket not reachable: %v", backend.Backend, err)
	}
	imageID := buildRealEngineImage(t, `while :; do :; done`)

	var mu sync.Mutex
	served := false
	mc := &mockClient{requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if served {
			return nil, status.Error(codes.NotFound, "no work")
		}
		served = true
		return &lettucev1.RequestWorkUnitResponse{Assignments: []*lettucev1.WorkUnitAssignment{{
			WorkUnitId: "7a0c1e5d-2b3f-4c8d-9e0f-1a2b3c4d5e6f", LeafId: "7a0c1e5d-2b3f-4c8d-9e0f-1a2b3c4d5e70",
			Runtime: "container", InputData: []byte("{}"), DeadlineSeconds: 600,
			ExecutionSpec: &lettucev1.ExecutionSpec{Image: imageID},
		}}}, nil
	}}
	d := newTestDaemon(mc, &mockRuntime{canHandle: false})
	d.runtimeRegistry.Register(cr)
	d.wireRuntimeLimits(nil, &testLimiter{})
	// The limit as a volunteer writes it.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("resource_limits:\n  max_cpu_cores: 1\n  max_memory_mb: 2048\n  max_disk_gb: 10\n  max_cpu_time_pct: 50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d.cfg.ResourceLimits = loaded.ResourceLimits
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	for _, srv := range d.multiClient.Servers() {
		srv.Config.TrustedRuntimes = []string{"CONTAINER"}
		d.cfg.Servers = append(d.cfg.Servers, config.ServerConfig{Name: srv.Name, TrustedRuntimes: []string{"CONTAINER"}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var containerID string
	for deadline := time.Now().Add(2 * time.Minute); containerID == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if sm := d.slotManager; sm != nil {
			for _, pt := range sm.GetActivePersistableTasks() {
				containerID = pt.ContainerID
			}
		}
	}
	if containerID == "" {
		t.Fatal("the container unit never started")
	}

	read := func() (usageUsec, fullUsec int64) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			stat, err1 := cr.Client().ContainerExecOutput(ctx, containerID, []string{"cat", "/sys/fs/cgroup/cpu.stat"})
			pressure, err2 := cr.Client().ContainerExecOutput(ctx, containerID, []string{"cat", "/sys/fs/cgroup/cpu.pressure"})
			if err1 != nil || err2 != nil {
				continue // paused: exec waits for the running part
			}
			return cgroupField(t, string(stat), "usage_usec"), pressureTotal(t, string(pressure), "full")
		}
		t.Fatal("could not read the container's cgroup")
		return 0, 0
	}
	time.Sleep(3 * time.Second)
	u0, f0 := read()
	t0 := time.Now()
	time.Sleep(30 * time.Second)
	u1, f1 := read()
	wall := time.Since(t0).Seconds()

	cores := float64(u1-u0) / 1e6 / wall
	fullShare := float64(f1-f0) / 1e6 / wall
	t.Logf("over %.1f s at 50 %% CPU time: %.2f cores used, CPU full pressure %.1f %%", wall, cores, fullShare*100)
	if cores < 0.35 || cores > 0.65 {
		t.Errorf("a one-core busy loop at 50 %% CPU time used %.2f cores on average, want about 0.5", cores)
	}
	if fullShare > 0.05 {
		t.Errorf("CPU full pressure %.1f %% under the CPU time limit, want none: a paused task does not stall", fullShare*100)
	}
}

// cgroupField reads one "key value" line of a cgroup file.
func cgroupField(t *testing.T, text, key string) int64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				t.Fatalf("parse %s: %v", key, err)
			}
			return n
		}
	}
	t.Fatalf("no %s in %q", key, text)
	return 0
}

// pressureTotal reads the total= microseconds of the some or full line of a
// cgroup pressure file.
func pressureTotal(t *testing.T, text, kind string) int64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != kind {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					t.Fatalf("parse %s total: %v", kind, err)
				}
				return n
			}
		}
	}
	t.Fatalf("no %s line in %q", kind, fmt.Sprint(text))
	return 0
}
