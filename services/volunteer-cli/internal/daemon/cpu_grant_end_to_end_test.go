package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression tests for the CPU grant, driven end to end through the slot
// filler, the container runtime and the configuration loader, the way the
// field saw each defect (see cpu_grant_test.go for the rule itself).

// grantRecordingEngine is a container engine that records every container it
// creates — the CPU quota and the environment — and holds each running until
// release is closed. A quota rewritten while the container runs lands in quotas too.
type grantRecordingEngine struct {
	runtime.DockerClient // nil: a call not implemented below panics
	mu                   sync.Mutex
	quotas               map[string]int64  // work unit id -> CPU quota
	limits               map[string]string // work unit id -> LETTUCE_CPU_LIMIT
	byContainer          map[string]string // container id -> work unit id
	release              chan struct{}
}

func newGrantRecordingEngine() *grantRecordingEngine {
	return &grantRecordingEngine{quotas: map[string]int64{}, limits: map[string]string{}, byContainer: map[string]string{}, release: make(chan struct{})}
}

func (e *grantRecordingEngine) ContainerList(context.Context, string) ([]runtime.ContainerSummary, error) {
	return nil, nil
}

func (e *grantRecordingEngine) ImageDeclaredVolumes(context.Context, string) ([]string, error) {
	return nil, nil
}

func (e *grantRecordingEngine) ContainerCreate(_ context.Context, cfg *runtime.ContainerConfig) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	wuID := cfg.Labels[runtime.WorkUnitIDLabel]
	e.quotas[wuID] = cfg.CPUQuota
	for _, kv := range cfg.Env {
		if v, ok := strings.CutPrefix(kv, "LETTUCE_CPU_LIMIT="); ok {
			e.limits[wuID] = v
		}
	}
	id := "c-" + wuID
	e.byContainer[id] = wuID
	return id, nil
}

func (e *grantRecordingEngine) ContainerUpdateCPU(_ context.Context, id string, quota, _ int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotas[e.byContainer[id]] = quota
	return nil
}

func (e *grantRecordingEngine) ContainerStart(context.Context, string) error { return nil }

func (e *grantRecordingEngine) ContainerWait(ctx context.Context, _ string) (int64, error) {
	select {
	case <-e.release:
		return 0, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (e *grantRecordingEngine) ContainerLogs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (e *grantRecordingEngine) ContainerUsage(context.Context, string) (*runtime.ContainerStats, error) {
	return &runtime.ContainerStats{}, nil
}

func (e *grantRecordingEngine) ContainerStop(context.Context, string, time.Duration) error {
	return nil
}
func (e *grantRecordingEngine) ContainerRemove(context.Context, string) error  { return nil }
func (e *grantRecordingEngine) ContainerPause(context.Context, string) error   { return nil }
func (e *grantRecordingEngine) ContainerUnpause(context.Context, string) error { return nil }

// TestTasksAreGrantedTheirLeafsCoresNotAnEqualShare is the field
// reproduction end to end, through the slot filler and the container runtime
// on a recording engine: 4 cores, a GREP unit (min 2) and two Beyblades
// buffered. The GREP container is created with 2 cores and told
// LETTUCE_CPU_LIMIT=2, each Beyblade with 1 — 4 in all. Pre-fix every task
// was given an equal share, 1.33 cores, the GREP unit below its own minimum.
func TestTasksAreGrantedTheirLeafsCoresNotAnEqualShare(t *testing.T) {
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{},
		resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger()))
	d.cfg.ResourceLimits.MaxCPUCores = 4
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = orig }()
	d.leafCache.PopulateForTest(d.multiClient.Servers()[0].Name, &CachedHeadInfo{Name: d.multiClient.Servers()[0].Name, Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2}},
		{ID: "leaf-bb", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1}},
	}})

	engine := newGrantRecordingEngine()
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), engine)
	d.runtimeRegistry.Register(cr)
	d.wireRuntimeLimits(nil, &testLimiter{})

	units := []*runtime.WorkUnit{
		headContainerUnit("grep-1", "leaf-grep", "ghcr.io/example/grep:1", 1024),
		headContainerUnit("bb-1", "leaf-bb", "ghcr.io/example/bb:1", 256),
		headContainerUnit("bb-2", "leaf-bb", "ghcr.io/example/bb:1", 256),
	}
	for _, wu := range units {
		workDir := t.TempDir()
		for _, sub := range []string{"input", "output", "checkpoint"} {
			if err := os.MkdirAll(filepath.Join(workDir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		d.prefetchQueue.Push(&PreFetchItem{
			WU: wu, Prep: &runtime.PrepareResult{WorkDir: workDir}, Runtime: cr,
			Conn:   &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			WUResp: &lettucev1.WorkUnitAssignment{}, FetchedAt: time.Now(),
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.fillSlots(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		engine.mu.Lock()
		created := len(engine.quotas)
		engine.mu.Unlock()
		if created == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // any quota rewrite after the last start

	engine.mu.Lock()
	quotas, limits := engine.quotas, engine.limits
	engine.mu.Unlock()
	want := map[string]struct {
		quota int64
		limit string
	}{"grep-1": {200000, "2"}, "bb-1": {100000, "1"}, "bb-2": {100000, "1"}}
	var total int64
	for id, w := range want {
		if quotas[id] != w.quota || limits[id] != w.limit {
			t.Errorf("%s: quota %d, told LETTUCE_CPU_LIMIT=%q; want %d and %q", id, quotas[id], limits[id], w.quota, w.limit)
		}
		total += quotas[id]
	}
	if total > 400000 {
		t.Errorf("the three tasks were given %d µs per 100 ms between them, above the 4-core budget", total)
	}

	close(engine.release)
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	for i := 0; i < 3; i++ {
		if _, err := d.slotManager.WaitForCompletion(waitCtx); err != nil {
			t.Fatalf("waiting for task %d to finish: %v", i+1, err)
		}
	}
}

// TestWideUnitIsNotStarvedByNarrowOnes: four Beyblades fill a 4-core
// budget and a GREP unit (min 2) waits at the head of the buffer with more
// Beyblades behind it. When one Beyblade finishes, the free core is held for
// the GREP unit rather than given to the next Beyblade; when a second
// finishes, the GREP unit starts. Pre-fix each Beyblade behind it started the
// moment a core freed — the backfill test found a one-core and a two-core
// unit fitting the budget together, so it never counted a jump — and the
// GREP unit waited as long as Beyblades kept arriving.
func TestWideUnitIsNotStarvedByNarrowOnes(t *testing.T) {
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{},
		resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger()))
	d.cfg.ResourceLimits.MaxCPUCores = 4
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = orig }()
	d.leafCache.PopulateForTest(d.multiClient.Servers()[0].Name, &CachedHeadInfo{Name: d.multiClient.Servers()[0].Name, Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2}},
		{ID: "leaf-bb", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1}},
	}})

	finish := map[string]chan struct{}{}
	item := func(id, leaf string) *PreFetchItem {
		done := make(chan struct{})
		finish[id] = done
		return &PreFetchItem{
			WU:     &runtime.WorkUnit{ID: id, LeafID: leaf},
			WUResp: &lettucev1.WorkUnitAssignment{},
			Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
			Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-done:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
			}},
			Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			FetchedAt: time.Now(),
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		for _, ch := range finish {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	running := func() map[string]bool {
		out := map[string]bool{}
		for _, wu := range d.slotManager.ActiveWorkUnits() {
			out[wu.ID] = true
		}
		return out
	}
	finishOne := func(id string) {
		t.Helper()
		close(finish[id])
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		if _, err := d.slotManager.WaitForCompletion(waitCtx); err != nil {
			t.Fatalf("waiting for %s to finish: %v", id, err)
		}
		for deadline := time.Now().Add(5 * time.Second); running()[id] && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}

	for _, id := range []string{"bb-1", "bb-2", "bb-3", "bb-4"} {
		d.prefetchQueue.Push(item(id, "leaf-bb"))
	}
	d.fillSlots(ctx)
	time.Sleep(50 * time.Millisecond)
	if got := len(running()); got != 4 {
		t.Fatalf("setup: %d Beyblades running, want 4", got)
	}
	d.prefetchQueue.Push(item("grep-1", "leaf-grep"))
	for _, id := range []string{"bb-5", "bb-6", "bb-7"} {
		d.prefetchQueue.Push(item(id, "leaf-bb"))
	}

	finishOne("bb-1")
	d.fillSlots(ctx)
	time.Sleep(50 * time.Millisecond)
	if r := running(); r["bb-5"] || r["bb-6"] || r["bb-7"] {
		t.Errorf("a Beyblade behind the waiting GREP unit took the freed core: running %v", r)
	}

	finishOne("bb-2")
	d.fillSlots(ctx)
	time.Sleep(50 * time.Millisecond)
	if r := running(); !r["grep-1"] {
		t.Errorf("two cores free and the GREP unit waiting at the head of the buffer, but it did not start: running %v", r)
	}
}

// TestRetiredMaxConcurrentTasksNoLongerHoldsOneTask: a config written by
// an earlier version carries max_concurrent_tasks: 1, the old default. It no
// longer keeps one task running — how many run follows from the budgets — and
// loading it says the key is retired and names max_running_tasks. Pre-fix the
// same file ran one task at a time on any machine.
func TestRetiredMaxConcurrentTasksNoLongerHoldsOneTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("max_concurrent_tasks: 1\nresource_limits:\n  max_cpu_cores: 4\n  max_memory_mb: 8192\n  max_disk_gb: 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.cfg = cfg
	if got := d.maxSlots(); got != 4 {
		t.Errorf("maxSlots with max_concurrent_tasks: 1 on a 4-core budget = %d, want 4", got)
	}
	var warned bool
	for _, w := range cfg.DeprecatedKeyWarnings() {
		if strings.Contains(w, "max_concurrent_tasks") && strings.Contains(w, "max_running_tasks") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("loading a config with max_concurrent_tasks gave warnings %q, want one naming max_running_tasks", cfg.DeprecatedKeyWarnings())
	}
}
