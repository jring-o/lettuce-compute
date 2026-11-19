package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// How many tasks the machine counts as able to run at once when its leafs are
// held by different limits.
//
// The reproduction: a Mac mini running the container Beyblade, which the
// Podman VM (1,280 MB, 2 vCPUs) runs one of at a time, and the native
// Beyblade, which the volunteer set to "Run at most 1". The two run together,
// one of each, but the client counted its slots as the most units of any ONE
// leaf — one — so with one native task running it believed every slot was
// busy: no idle slot could be reported, an empty container queue raised
// nothing, and the work buffer and the fetch were sized for one task. Once
// the native leaf's units filled that one-task buffer, the fetcher asked no
// head, though a container unit could have started beside the native one.

// capMixUnitSeconds is how long one unit of either leaf runs here (2,460 s, the
// reproduction's units), booked through the CPU benchmark.
const capMixUnitSeconds = 2460

// capMixMini is the reproduction's machine: 4 cores and 4,096 MB configured,
// a Podman VM of 1,280 MB and 2 vCPUs (one 768 MB container unit at a time), no
// running-task cap, a four-slot pool, work_buffer_hours 2, and the head
// "server-a" trusted for CONTAINER and NATIVE with the native leaf capped at 1
// task at a time. trustContainer false leaves the head untrusted for container
// work. The returned function counts the requests per leaf id.
func capMixMini(t *testing.T, trustContainer bool) (*Daemon, func() map[string]int) {
	t.Helper()
	d, rc, _, _ := tb85Mini(t, 4096, 4, 1280, 2)
	d.cfg.MaxRunningTasks = 0
	d.cfg.WorkBufferHours = 2
	d.benchmarkFPOPS = 1
	d.slotManager = NewSlotManager(4, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "native"})
	trusted := []string{"NATIVE"}
	if trustContainer {
		trusted = append(trusted, "CONTAINER")
	}
	srv := d.multiClient.Servers()[0]
	srv.Config.Name = "server-a"
	srv.Config.TrustedRuntimes = trusted
	sc := srv.Config
	sc.LeafPreferences = config.LeafPreferences{MaxRunning: map[string]int{"beyblade-native": 1}}
	d.cfg.Servers = []config.ServerConfig{sc}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: tb85Leafs(1),
		DefaultWeights: map[string]int{"beyblade-container": 100, "beyblade-native": 100}})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"beyblade-container": 100, "beyblade-native": 100})

	var mu sync.Mutex
	reqs := map[string]int{}
	rc.mockClient.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range req.LeafIds {
			reqs[id]++
		}
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	return d, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(reqs))
		for k, v := range reqs {
			out[k] = v
		}
		return out
	}
}

// capMixNative is one native Beyblade unit, as the head sends it.
func capMixNative(i int) *runtime.WorkUnit {
	wu := headNativeUnit(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "leaf-bb-n", 128)
	wu.RscFpopsEst = capMixUnitSeconds
	wu.SourceHead = "server-a"
	return wu
}

// TestSlotCount_CappedLeafBesideAVMBoundContainerLeaf is the reproduction with
// its outcome inverted: the machine counts the two tasks its budgets run
// together, reports the second slot idle while one native task runs, and with
// the native leaf's buffer at its own target still asks the head for the
// container leaf. Pre-fix: one slot, (0 idle, 1), the one-slot target (7,200 s)
// full at 7,380 s, and no request at all.
func TestSlotCount_CappedLeafBesideAVMBoundContainerLeaf(t *testing.T) {
	d, requests := capMixMini(t, true)

	if got := d.maxSlots(); got != 2 {
		t.Errorf("maxSlots = %d, want 2: one native task (capped at 1) and one container task (the VM holds one) run together", got)
	}
	if got := len(d.RunPreview().Together); got != d.maxSlots() {
		t.Errorf("the run preview starts %d tasks together, the slot count is %d; the two are the same placement", got, d.maxSlots())
	}

	// One native task running, nothing buffered: the container slot is idle.
	occupy(d, 0, capMixNative(1000), nil)
	if ok, why := d.canAccommodateWU(headContainerUnit("c-1", "leaf-bb-c", "ghcr.io/example/beyblade:1", 768)); !ok {
		t.Fatalf("setup: a container unit cannot start beside the native task: %s", why)
	}
	if idle, total := d.starvedIdleSlots(); idle != 1 || total != 2 {
		t.Errorf("starvedIdleSlots = (%d, %d), want (1, 2): the container slot is idle with nothing to start", idle, total)
	}

	// Two more native units queued: the native leaf holds 3 × 2,460 s = 7,380 s,
	// over its own one-task target of 7,200 s, so it is not asked for; the
	// buffer as a whole (two tasks, 14,400 s) is not full, so the container leaf is.
	for i := 0; i < 2; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: capMixNative(i), FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	if got := d.bufferTargetSeconds(); got != 14400 {
		t.Errorf("bufferTargetSeconds = %g, want 14400 (two tasks × 2 h)", got)
	}
	if full, _ := d.leafClassBufferFull(tb85Leafs(1)[1]); !full {
		t.Error("the native leaf's own buffer class is not full at 7,380 s against its 7,200 s target")
	}
	if d.workBufferFull() && !d.starvedBackfill() {
		t.Errorf("workBufferFull = true and starvedBackfill = false with %g s buffered against a %g s target: the fetch is shut while a container unit could start",
			d.bufferedSeconds(), d.bufferTargetSeconds())
	}
	if d.workInHandForEverySlot() {
		t.Error("workInHandForEverySlot = true: the queued native units cannot start, so the container slot has no work in hand")
	}

	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	f.shouldFetchFunc = func() bool { return true }
	f.leafDiskGateFn = nil // the disk gate is not under test
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && requests()["leaf-bb-c"] == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	got := requests()
	if got["leaf-bb-c"] == 0 {
		t.Errorf("the fetcher asked for the container leaf 0 times in 2 s (requests %v): a container unit could start beside the native task", got)
	}
	if got["leaf-bb-n"] != 0 {
		t.Errorf("the native leaf was asked for %d times with its own buffer class full", got["leaf-bb-n"])
	}
}

// TestSlotCount_TwoCappedLeafs: two native leafs each capped at one task at a
// time on 8 cores run two tasks between them, and with one running the other
// slot is idle. Pre-fix: one slot, (0 idle, 1).
func TestSlotCount_TwoCappedLeafs(t *testing.T) {
	srv := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true}
	srv.Config.Name = "server-a"
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.cfg.ResourceLimits.MaxCPUCores = 8
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	d.cfg.WorkBufferHours = 2
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	sc := srv.Config
	sc.LeafPreferences = config.LeafPreferences{MaxRunning: map[string]int{"leaf-a": 1, "leaf-b": 1}}
	d.cfg.Servers = []config.ServerConfig{sc}
	native := func(id string) CachedLeafInfo {
		return CachedLeafInfo{ID: id, Slug: id, Name: id, State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/" + id}, MaxMemoryMB: 256}}
	}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: []CachedLeafInfo{native("leaf-a"), native("leaf-b")}})

	if got := d.maxSlots(); got != 2 {
		t.Errorf("maxSlots = %d, want 2: each leaf runs one task at a time, so two run together", got)
	}
	if got := len(d.RunPreview().Together); got != 2 {
		t.Errorf("the run preview starts %d tasks together, want 2", got)
	}
	occupy(d, 0, &runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000001", LeafID: "leaf-a", Runtime: runtime.RuntimeNative, SourceHead: "server-a"}, nil)
	if idle, total := d.starvedIdleSlots(); idle != 1 || total != 2 {
		t.Errorf("starvedIdleSlots = (%d, %d), want (1, 2): leaf-b's task could start beside leaf-a's", idle, total)
	}
}

// TestSlotCount_NeverBelowTheMostParallelLeaf is a guard on what the change
// leaves alone: a memory-heavy leaf beside a light one runs fewer tasks
// together than the light leaf alone, and the machine still counts the light
// leaf's figure, as before, because a buffer that holds only the light leaf's
// work runs that many. It passes on the code before the change too.
func TestSlotCount_NeverBelowTheMostParallelLeaf(t *testing.T) {
	srv := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.cfg.ResourceLimits.MaxCPUCores = 8
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	d.slotManager = NewSlotManager(8, d.logger)
	native := func(id string, memMB int32) CachedLeafInfo {
		return CachedLeafInfo{ID: id, Slug: id, Name: id, State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/" + id}, MaxMemoryMB: memMB}}
	}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: []CachedLeafInfo{native("heavy", 6144), native("light", 2048)}})

	if got := len(d.RunPreview().Together); got != 2 {
		t.Fatalf("setup: the run preview starts %d tasks together, want 2 (6 GB + 2 GB fill the 8 GB budget)", got)
	}
	if got := d.maxSlots(); got != 4 {
		t.Errorf("maxSlots = %d, want 4: the light leaf alone runs four (8 GB / 2 GB)", got)
	}
}

// TestSlotCount_LeafsThisMachineIsNotHandedAreNotCounted is a guard on the new
// count: a container leaf whose head this machine does not trust for container
// work is never asked for, so it adds no task to what runs together, and the
// capped native leaf is all the machine counts. With the trust given, the two
// run together. (Before the change the count was one either way.)
func TestSlotCount_LeafsThisMachineIsNotHandedAreNotCounted(t *testing.T) {
	d, _ := capMixMini(t, false)
	if got := d.maxSlots(); got != 1 {
		t.Errorf("maxSlots = %d, want 1: the container leaf is never requested from an untrusted head", got)
	}
	d, _ = capMixMini(t, true)
	if got := d.maxSlots(); got != 2 {
		t.Errorf("maxSlots = %d with the head trusted for container work, want 2", got)
	}
}
