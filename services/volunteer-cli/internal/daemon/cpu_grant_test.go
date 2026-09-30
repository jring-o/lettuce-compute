package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression tests for the CPU grant: each task is given the cores its leaf
// needs, not an equal split of the budget.
//
// The reproduction: on a 4-core budget a GREP unit (min_cpu_cores 2; it can
// use 3 or 4) started beside Beyblade units (one core each) was given the same
// share as each Beyblade — 1.33 cores — and ran past its 6 h deadline, while
// every Beyblade left part of its share idle. A waiting GREP unit could also
// wait indefinitely behind a stream of Beyblades, each of which fit the moment
// one core freed. And max_concurrent_tasks, written as 1 into nearly every
// config, kept one task running on a 32-core machine.
//
// Now a task is granted whole cores between its leaf's min_cpu_cores and
// max_cpu_cores when it starts — its minimum plus what the budget has free
// once the units waiting to start beside it have their minimums set aside —
// and keeps them for its run; a narrow unit may not take cores a wide unit is
// waiting for; how many run at once follows from the budgets.

// grantTestLeafs is the fleet's two leafs as a head sends them: GREP declaring
// 2–4 cores and Beyblade 1 (no max: a head too old to send one, or a leaf
// that declares none). Both are container leafs.
func grantTestLeafs() []CachedLeafInfo {
	return []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", State: "ACTIVE",
			ExecutionSpec:        &CachedExecutionSpec{Image: "ghcr.io/example/grep:1", MaxMemoryMB: 1024},
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
		{ID: "leaf-bb", Slug: "beyblade", State: "ACTIVE",
			ExecutionSpec:        &CachedExecutionSpec{Image: "ghcr.io/example/bb:1", MaxMemoryMB: 256},
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1}},
	}
}

// grantTestDaemon is a machine with the given CPU budget, no memory budget, eight
// slots in its pool, the fleet's two leafs, and every admission guard but the
// budgets isolated away.
func grantTestDaemon(t *testing.T, cores int) *Daemon {
	t.Helper()
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{},
		resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger()))
	d.cfg.ResourceLimits.MaxCPUCores = cores
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	d.leafCache.PopulateForTest(d.multiClient.Servers()[0].Name, &CachedHeadInfo{Name: d.multiClient.Servers()[0].Name, Leafs: grantTestLeafs()})
	return d
}

func grantTestGrep(id string) *runtime.WorkUnit {
	return headContainerUnit(id, "leaf-grep", "ghcr.io/example/grep:1", 1024)
}
func grantTestBB(id string) *runtime.WorkUnit {
	return headContainerUnit(id, "leaf-bb", "ghcr.io/example/bb:1", 256)
}

// TestGrantFollowsTheLeafRangeAndWhatWaits is the decided rule: a task
// gets its leaf's minimum, plus the free cores the units waiting to start
// beside it do not need, up to its leaf's maximum. On 4 cores GREP (2–4)
// is granted 2 with two Beyblades waiting, 3 with one, 4 alone; 8 cores
// still stop at its maximum of 4; a Beyblade is granted its one core
// whatever is free; a leaf that declares no maximum gets exactly its
// minimum.
func TestGrantFollowsTheLeafRangeAndWhatWaits(t *testing.T) {
	d := grantTestDaemon(t, 4)
	for _, tc := range []struct {
		name    string
		unit    *runtime.WorkUnit
		waiting []*runtime.WorkUnit
		want    int
	}{
		{"GREP, two Beyblades waiting", grantTestGrep("g"), []*runtime.WorkUnit{grantTestBB("b1"), grantTestBB("b2")}, 2},
		{"GREP, one Beyblade waiting", grantTestGrep("g"), []*runtime.WorkUnit{grantTestBB("b1")}, 3},
		{"GREP alone", grantTestGrep("g"), nil, 4},
		{"GREP, another GREP waiting", grantTestGrep("g"), []*runtime.WorkUnit{grantTestGrep("g2")}, 2},
		{"Beyblade, a GREP waiting", grantTestBB("b"), []*runtime.WorkUnit{grantTestGrep("g")}, 1},
		{"Beyblade alone", grantTestBB("b"), nil, 1},
		{"GREP, itself in the waiting list", grantTestGrep("g"), []*runtime.WorkUnit{grantTestGrep("g")}, 4},
	} {
		got := d.grantCPU(tc.unit, tc.waiting)
		if got.Cores != tc.want || got.BudgetCores != 4 {
			t.Errorf("%s: grant %v, want %d of 4", tc.name, got, tc.want)
		}
	}

	d.cfg.ResourceLimits.MaxCPUCores = 8
	if got := d.grantCPU(grantTestGrep("g"), nil).Cores; got != 4 {
		t.Errorf("GREP alone on 8 cores: granted %d, want its leaf's maximum 4", got)
	}

	// A head too old to send the maximum: the range is min–min.
	d.leafCache.PopulateForTest(d.multiClient.Servers()[0].Name, &CachedHeadInfo{Name: d.multiClient.Servers()[0].Name, Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2}},
	}})
	if got := d.grantCPU(grantTestGrep("g"), nil).Cores; got != 2 {
		t.Errorf("GREP with no maximum from its head, alone on 8 cores: granted %d, want its minimum 2", got)
	}
	// A leaf the cache does not know: one core.
	if got := d.grantCPU(&runtime.WorkUnit{ID: "u", LeafID: "leaf-unknown"}, nil).Cores; got != 1 {
		t.Errorf("unknown leaf: granted %d, want 1", got)
	}
}

// TestGrantCountsWhatRunsAndOnlyWhatCanStart: cores already granted to
// running tasks are not free; a waiting unit that could not start anyway (its
// memory does not fit) has no cores held back for it; and a unit that finds
// less than its minimum free (resumed after the budget was lowered) is given
// its minimum, never more than the budget.
func TestGrantCountsWhatRunsAndOnlyWhatCanStart(t *testing.T) {
	d := grantTestDaemon(t, 4)
	bb := grantTestBB("running")
	bb.CPUGrant = runtime.CPUGrant{Cores: 1, BudgetCores: 4}
	occupy(d, 0, bb, nil)
	if got := d.grantCPU(grantTestGrep("g"), nil).Cores; got != 3 {
		t.Errorf("GREP beside a running one-core task on 4 cores: granted %d, want 3", got)
	}

	release(d, 0)
	d.cfg.ResourceLimits.MaxMemoryMB = 1200 // GREP's 1024 MB fits; not a Beyblade beside it
	if got := d.grantCPU(grantTestGrep("g"), []*runtime.WorkUnit{grantTestBB("b1")}).Cores; got != 4 {
		t.Errorf("GREP with a Beyblade waiting that the memory budget cannot run beside it: granted %d, want 4 (nothing held back for it)", got)
	}

	d.cfg.ResourceLimits.MaxMemoryMB = 0
	wide := grantTestGrep("wide")
	wide.CPUGrant = runtime.CPUGrant{Cores: 4, BudgetCores: 4}
	occupy(d, 0, wide, nil)
	d.cfg.ResourceLimits.MaxCPUCores = 3 // lowered while it runs
	if got := d.grantCPU(grantTestGrep("g"), nil); got.Cores != 2 || got.BudgetCores != 3 {
		t.Errorf("GREP resumed into an overcommitted budget: granted %v, want its minimum 2 of 3", got)
	}
}

// TestAdmissionBooksRunningTasksAtTheirGrants: a running task holds the
// cores it was granted, so on 4 cores a GREP granted 3 leaves room for one
// Beyblade, not two; the grants together never exceed the budget.
func TestAdmissionBooksRunningTasksAtTheirGrants(t *testing.T) {
	d := grantTestDaemon(t, 4)
	g := grantTestGrep("g")
	g.CPUGrant = d.grantCPU(g, []*runtime.WorkUnit{grantTestBB("b1")})
	if g.CPUGrant.Cores != 3 {
		t.Fatalf("setup: GREP granted %d, want 3", g.CPUGrant.Cores)
	}
	occupy(d, 0, g, nil)
	if ok, why := d.canAccommodateWU(grantTestBB("b1")); !ok {
		t.Fatalf("a one-core unit refused beside a GREP granted 3 of 4: %s", why)
	}
	b1 := grantTestBB("b1")
	b1.CPUGrant = d.grantCPU(b1, nil)
	occupy(d, 1, b1, nil)
	ok, why := d.canAccommodateWU(grantTestBB("b2"))
	if ok {
		t.Fatal("a second one-core unit was admitted beside grants of 3 and 1 on a 4-core budget")
	}
	if !strings.Contains(why, "configured CPU budget: 4 core(s) booked") {
		t.Errorf("refusal = %q, want the CPU budget with 4 cores booked", why)
	}
}

// TestRunningTaskCapIsOptional: max_running_tasks, when set, caps how
// many run whatever the cores allow; unset, the cores decide.
func TestRunningTaskCapIsOptional(t *testing.T) {
	d := grantTestDaemon(t, 4)
	occupy(d, 0, grantTestBB("b1"), nil)
	if ok, why := d.canAccommodateWU(grantTestBB("b2")); !ok {
		t.Fatalf("with no cap a second one-core unit was refused on 4 cores: %s", why)
	}
	d.cfg.MaxRunningTasks = 1
	ok, why := d.canAccommodateWU(grantTestBB("b2"))
	if ok || !strings.Contains(why, "running tasks cap: 1 task(s) running, max_running_tasks is 1") {
		t.Errorf("with max_running_tasks 1 and one task running: ok %v, reason %q", ok, why)
	}
	if got := d.maxSlots(); got != 1 {
		t.Errorf("maxSlots with max_running_tasks 1 = %d, want 1", got)
	}
}

// TestSlotsFollowTheBudgets: how many tasks the buffer is sized for is
// the most units of one enabled leaf the CPU and memory budgets run together
// — 4 Beyblades on 4 cores, 2 GREPs if GREP were the only leaf, and never
// more than the memory budget holds.
func TestSlotsFollowTheBudgets(t *testing.T) {
	d := grantTestDaemon(t, 4)
	if got := d.maxSlots(); got != 4 {
		t.Errorf("4 cores with a one-core leaf enabled: maxSlots = %d, want 4", got)
	}
	d.leafCache.PopulateForTest(d.multiClient.Servers()[0].Name, &CachedHeadInfo{Name: d.multiClient.Servers()[0].Name, Leafs: grantTestLeafs()[:1]})
	if got := d.maxSlots(); got != 2 {
		t.Errorf("4 cores with only GREP (min 2) enabled: maxSlots = %d, want 2", got)
	}
	d.cfg.ResourceLimits.MaxCPUCores = 16
	d.cfg.ResourceLimits.MaxMemoryMB = 2048
	if got := d.maxSlots(); got != 2 {
		t.Errorf("16 cores, 2048 MB and GREP's 1024 MB units: maxSlots = %d, want 2 (the memory budget binds)", got)
	}
}

// TestTakenCoresAreNotAnIdleSlot: with every core granted, a slot the
// task count calls idle is not starved — no unit could start for want of
// cores — so no warning or starved-backfill fetch is set off by it.
func TestTakenCoresAreNotAnIdleSlot(t *testing.T) {
	d := grantTestDaemon(t, 4)
	g := grantTestGrep("g")
	g.CPUGrant = runtime.CPUGrant{Cores: 4, BudgetCores: 4}
	occupy(d, 0, g, nil)
	if d.occupiedSlots() >= d.maxSlots() {
		t.Fatalf("setup: %d of %d slots occupied; the test needs slots the count calls idle", d.occupiedSlots(), d.maxSlots())
	}
	if d.idleSlotStarved() {
		t.Error("idleSlotStarved = true with all 4 cores granted to a running task")
	}
	release(d, 0)
	if !d.idleSlotStarved() {
		t.Error("idleSlotStarved = false on an idle machine with an empty buffer")
	}
}

// TestAdoptedTaskKeepsItsGrant: a task adopted from the previous
// session is still held to the grant it started with, so it is booked at it
// again; a state file from an older client, which recorded none, books its
// leaf's minimum. The grant is recorded with the task for that.
func TestAdoptedTaskKeepsItsGrant(t *testing.T) {
	d := grantTestDaemon(t, 4)
	if got := d.adoptedCPUGrant(grantTestGrep("g"), 3); got.Cores != 3 || got.BudgetCores != 4 {
		t.Errorf("adopted GREP recorded at 3 cores: booked %v, want 3 of 4", got)
	}
	if got := d.adoptedCPUGrant(grantTestGrep("g"), 0); got.Cores != 2 {
		t.Errorf("adopted GREP with no recorded grant: booked %v, want its minimum 2", got)
	}

	g := grantTestGrep("g")
	g.CPUGrant = runtime.CPUGrant{Cores: 3, BudgetCores: 4}
	slot := occupy(d, 0, g, nil)
	slot.mu.Lock()
	slot.prep = &runtime.PrepareResult{WorkDir: t.TempDir()}
	slot.conn = &ServerConnection{Name: "test", Config: config.ServerConfig{GRPCAddress: "head.example:443"}}
	slot.mu.Unlock()
	tasks := d.slotManager.GetActivePersistableTasks()
	if len(tasks) != 1 || tasks[0].CPUGrantCores != 3 {
		t.Errorf("persisted tasks = %+v, want the GREP with cpu_grant_cores 3", tasks)
	}
	if cur := d.GetCurrentTasks(); len(cur) != 1 || cur[0].CPUCores != 3 {
		t.Errorf("current tasks = %+v, want the GREP shown with 3 cores", cur)
	}
}

// TestWeightsCountCoreSeconds: a unit that holds three cores for an
// hour uses three times the machine of a one-core unit for an hour, so the
// weights book it at three times the seconds — at fetch (the fewest cores it
// will be granted), at completion (the cores it was granted) and when the
// balance is seeded from history. At equal weights the one-core leaf is then
// due next.
func TestWeightsCountCoreSeconds(t *testing.T) {
	now := time.Now()
	ws := NewWeightedSelector()
	ws.now = func() time.Time { return now }
	ws.RecordAssignment("srv", "leaf-grep", "g1", 3600, 3)
	ws.RecordAssignment("srv", "leaf-bb", "b1", 3600, 1)
	if g, b := ws.BookedSeconds("srv", "leaf-grep"), ws.BookedSeconds("srv", "leaf-bb"); g != 3*3600 || b != 3600 {
		t.Errorf("booked at fetch: grep %v, bb %v; want 10800 and 3600", g, b)
	}
	ws.RecordCompletion("srv", "leaf-grep", "g1", 1800, 4)
	if got := ws.BookedSeconds("srv", "leaf-grep"); got != 4*1800 {
		t.Errorf("GREP's booking after it ran 1800 s on 4 cores = %v, want 7200", got)
	}
	leafs := []CachedLeafInfo{{ID: "leaf-grep", Slug: "grep"}, {ID: "leaf-bb", Slug: "beyblade"}}
	if next := ws.SelectLeaf("srv", leafs); next != "leaf-bb" {
		t.Errorf("at equal weights the leaf due next = %q, want leaf-bb (fewer core-seconds booked)", next)
	}

	seeded := NewWeightedSelector()
	seeded.now = func() time.Time { return now }
	seeded.SeedFromHistory([]HistoryEntry{
		{ServerName: "srv", LeafID: "leaf-grep", CompletedAt: now, CPUSeconds: 1000, CPUCores: 3},
		{ServerName: "srv", LeafID: "leaf-bb", CompletedAt: now, CPUSeconds: 1000},
	})
	if g, b := seeded.BookedSeconds("srv", "leaf-grep"), seeded.BookedSeconds("srv", "leaf-bb"); g != 3000 || b != 1000 {
		t.Errorf("seeded from history: grep %v, bb %v; want 3000 (3 cores) and 1000 (a run recorded with no cores counts one)", g, b)
	}
}
