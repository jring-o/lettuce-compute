package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-32 regression tests: the hours-based work buffer counted hours, not
// runnability. A buffer full of units that admission (canAccommodateWU) refuses
// next to the running work pinned an idle slot for hours — the fetcher took the
// buffer-full early exit forever, never requesting the small units other
// attached leafs would hand out instantly. Live shape (tester QuaXeros,
// 2026-07-31, v0.10.5): 2 slots, max_memory_mb 8192, one 6000 MB GREP running,
// 6–11 GREPs buffered; active_slots pinned at 1 for ~9 slot-hours in a day.

// tb32StarvedDaemon builds that tester-shaped state: 2 slots, an 8192 MB memory
// budget, work_buffer_hours 2 (target 14400 s), benchmark 1 FPOPS (so estimated
// seconds == RscFpopsEst), one 6000 MB unit ACTIVE in a slot, and four
// 7200 s / 6000 MB units buffered. Every buffered unit is refused admission
// beside the running one (6000 + 6000 > 8192) while the second slot sits idle.
func tb32StarvedDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newBufferTestDaemon(t, 2.0, 2, 1.0)
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	tb32Starve(t, d)
	return d
}

// tb32Starve applies the starved shape to a daemon whose slotManager (2 slots)
// and prefetchQueue are already set: pins free RAM, occupies one slot with a
// blocking 6000 MB unit, and buffers four 7200 s / 6000 MB units.
func tb32Starve(t *testing.T, d *Daemon) {
	t.Helper()

	// Pin real free RAM well above every unit so only the configured budget
	// (guard #1) decides admission, keeping the test hermetic on small CI hosts.
	origFree := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 64000, true }
	t.Cleanup(func() { freeSystemMemoryMB = origFree })

	// One 6000 MB unit running: occupy a slot with a blocking execution.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	slotID := <-d.slotManager.available
	if err := d.slotManager.StartSlot(ctx, slotID, &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:            "00000000-0000-4000-8000-0000000000aa",
			LeafID:        "leaf-grep",
			RscFpopsEst:   7200,
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 6000},
		},
		Prep: &runtime.PrepareResult{WorkDir: t.TempDir()},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			<-blockCh
			return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
		}},
		Conn:      &ServerConnection{Name: "test-head", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	if got := d.slotManager.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount = %d, want 1", got)
	}

	// Four more 6000 MB units buffered — 28800 s queued + 7200 s running against
	// the 14400 s target: the hours math says full, several times over.
	for i := 1; i <= 4; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{
			ID:            fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", i),
			LeafID:        "leaf-grep",
			RscFpopsEst:   7200,
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 6000},
		}}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
}

// TestTB32_FullBufferOfUnrunnableUnitsDoesNotStopFetching is the core TB-32
// regression: with a slot idle and NOTHING buffered admissible to it, the work
// buffer must not report full — reporting full is what made the fetcher issue
// zero RequestWorkUnit calls while head-side work the idle slot could run
// existed the whole time (proven live: the slot filled the same second one
// request finally went out).
func TestTB32_FullBufferOfUnrunnableUnitsDoesNotStopFetching(t *testing.T) {
	d := tb32StarvedDaemon(t)

	// Sanity: every buffered unit is refused admission beside the running one —
	// the same "configured memory budget" refusal the tester's log shows.
	for _, it := range d.prefetchQueue.Items() {
		if ok, reason := d.canAccommodateWU(it.WU); ok {
			t.Fatalf("buffered unit %s unexpectedly admissible (%s)", it.WU.ID, reason)
		}
	}
	// Sanity: the hours math alone says full, several times over.
	if d.bufferedSeconds() < d.bufferTargetSeconds() {
		t.Fatalf("bufferedSeconds = %g below target %g — state does not reproduce the defect",
			d.bufferedSeconds(), d.bufferTargetSeconds())
	}

	// THE regression: a slot is idle and no buffered unit can occupy it, so the
	// buffer must NOT gate fetching, whatever the hours say.
	if d.workBufferFull() {
		t.Error("workBufferFull = true while a slot is idle and no buffered unit is admissible — the fetcher would never request the admissible units that could fill it (TB-32)")
	}

	// Counterfactual: once ONE admissible unit is buffered, the idle slot can be
	// filled from the buffer, so the hours target governs again and fetching stops.
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{
		ID:            "00000000-0000-4000-8000-0000000000ff",
		LeafID:        "leaf-small",
		RscFpopsEst:   600,
		ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 512},
	}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !d.workBufferFull() {
		t.Error("workBufferFull = false with an admissible unit buffered for the idle slot — the buffer is genuinely full and fetching should stop")
	}
}

// tb32FetchDaemon reshapes a fetcher-test daemon into the starved tester state
// and registers two leafs on its head: the 6000 MB "grep" leaf that saturated
// the buffer and a 512 MB "small" leaf that could fill the idle slot.
func tb32FetchDaemon(t *testing.T, mc *mockClient) (*Daemon, []*ServerConnection) {
	t.Helper()
	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "test-head", Available: true},
	}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = 2
	d.cfg.MaxConcurrentTasks = 2
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	d.benchmarkFPOPS = 1.0
	d.slotManager = NewSlotManager(2, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	tb32Starve(t, d)

	d.leafCache.PopulateForTest("test-head", &CachedHeadInfo{
		Name: "test-head",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
				ExecutionSpec:            &CachedExecutionSpec{MaxMemoryMB: 6000},
				EstimatedDurationSeconds: 7200},
			{ID: "leaf-small", Slug: "small", Name: "Small", State: "ACTIVE",
				ExecutionSpec:            &CachedExecutionSpec{MaxMemoryMB: 512},
				EstimatedDurationSeconds: 600},
		},
		DefaultWeights: map[string]int{"grep": 100, "small": 100},
	})
	d.weightedSelector.SetLeafWeights("test-head", map[string]int{"grep": 100, "small": 100})
	d.weightedSelector.SetHeadWeights(map[string]int{"test-head": 100})
	return d, servers
}

// TestTB32_BackfillRequestsOnlyLeafsThatFitTheIdleSlot proves the fetch-side
// half: in a starved-backfill round the fetcher never re-requests the leaf
// whose units cannot be admitted beside the running work (more of it cannot end
// the starvation), and the request that does go out fills the idle slot's
// buffer from the leaf that fits.
func TestTB32_BackfillRequestsOnlyLeafsThatFitTheIdleSlot(t *testing.T) {
	var mu sync.Mutex
	var requested [][]string
	mc := &mockClient{}
	mc.requestWorkUnitFn = func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		requested = append(requested, append([]string{}, req.LeafIds...))
		mu.Unlock()
		if len(req.LeafIds) == 1 && req.LeafIds[0] == "leaf-small" {
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{{
					WorkUnitId:    "00000000-0000-4000-8000-0000000000f1",
					LeafId:        "leaf-small",
					Runtime:       "native",
					InputData:     []byte("input"),
					RscFpopsEst:   600,
					ExecutionSpec: &lettucev1.ExecutionSpec{MaxMemoryMb: 512},
				}},
			}, nil
		}
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	d, _ := tb32FetchDaemon(t, mc)

	if !d.starvedBackfill() {
		t.Fatal("starvedBackfill = false — state does not reproduce the defect")
	}

	fetcher := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	got, err := fetcher.fetchOne(context.Background())
	if err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if got != 1 {
		t.Fatalf("fetchOne buffered %d units, want 1 (the small leaf's unit)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requested) == 0 {
		t.Fatal("no RequestWorkUnit issued at all — the starved slot would idle forever")
	}
	for _, ids := range requested {
		for _, id := range ids {
			if id == "leaf-grep" {
				t.Errorf("backfill round requested leaf-grep, whose units cannot fill the idle slot; requests: %v", requested)
			}
		}
	}
}

// TestTB32_BatchOverflowReturnsToHeadInsteadOfWaitingForDeadline proves the
// arrival-time guard: units over the hours target that cannot start in the
// starving slot are abandoned back to the head at once (it re-dispatches them
// in seconds), instead of being held unrunnable until the 90 %-of-deadline drop
// (observed live: 10 fetch-and-drop events for 5 units across two hosts in one
// day, zero compute). An over-target unit that CAN start is kept — and only
// until the slot's need is met.
func TestTB32_BatchOverflowReturnsToHeadInsteadOfWaitingForDeadline(t *testing.T) {
	mc := &mockClient{}
	d, servers := tb32FetchDaemon(t, mc)
	fetcher := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)

	asg := func(id string, memMB int32, fpops float64) *lettucev1.WorkUnitAssignment {
		return &lettucev1.WorkUnitAssignment{
			WorkUnitId:    id,
			LeafId:        "leaf-x",
			Runtime:       "native",
			InputData:     []byte("input"),
			RscFpopsEst:   fpops,
			ExecutionSpec: &lettucev1.ExecutionSpec{MaxMemoryMb: memMB},
		}
	}
	abandons := func() int {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		return mc.abandonCalls
	}

	// Two more big units on an already-full buffer: both returned, none buffered.
	pushed, _ := fetcher.bufferBatch(context.Background(), servers[0],
		CachedLeafInfo{ID: "leaf-grep", Slug: "grep"},
		[]*lettucev1.WorkUnitAssignment{
			asg("00000000-0000-4000-8000-0000000000e1", 6000, 7200),
			asg("00000000-0000-4000-8000-0000000000e2", 6000, 7200),
		})
	if pushed != 0 {
		t.Fatalf("bufferBatch pushed %d over-target unrunnable units, want 0", pushed)
	}
	if got := abandons(); got != 2 {
		t.Fatalf("abandoned %d units, want 2 (both returned for re-dispatch)", got)
	}
	if got := d.prefetchQueue.Len(); got != 4 {
		t.Fatalf("queue length = %d, want 4 (unchanged)", got)
	}

	// An over-target unit that CAN start feeds the starving slot — kept.
	pushed, _ = fetcher.bufferBatch(context.Background(), servers[0],
		CachedLeafInfo{ID: "leaf-small", Slug: "small"},
		[]*lettucev1.WorkUnitAssignment{asg("00000000-0000-4000-8000-0000000000e3", 512, 600)})
	if pushed != 1 {
		t.Fatalf("bufferBatch pushed %d admissible backfill units, want 1", pushed)
	}

	// The slot's need is now met from the buffer: a further over-target unit is
	// returned, so the buffer cannot grow without bound.
	pushed, _ = fetcher.bufferBatch(context.Background(), servers[0],
		CachedLeafInfo{ID: "leaf-small", Slug: "small"},
		[]*lettucev1.WorkUnitAssignment{asg("00000000-0000-4000-8000-0000000000e4", 512, 600)})
	if pushed != 0 {
		t.Fatalf("bufferBatch pushed %d units past a satisfied buffer, want 0", pushed)
	}
	if got := abandons(); got != 3 {
		t.Fatalf("abandoned %d units, want 3", got)
	}
}

// TestTB32_SlotStarvationWarnsThrottled proves the visibility half: the whole
// condition used to live at INFO/DEBUG (36,542 buffer-full DEBUG lines in one
// tester's 5-hour window, nothing above INFO). A slot idle past the threshold
// with only inadmissible work buffered WARNs, naming the head-of-buffer unit's
// blocking reason; repeats are throttled; recovery resets the state.
func TestTB32_SlotStarvationWarnsThrottled(t *testing.T) {
	d := tb32StarvedDaemon(t)
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// First observations stamp the state but stay quiet below the threshold.
	d.trackSlotStarvation()
	d.trackSlotStarvation()
	if buf.Len() != 0 {
		t.Fatalf("warned before the %s threshold: %s", slotStarveWarnAfter, buf.String())
	}

	// Age the state past the threshold: the WARN fires and names the reason.
	d.slotStarveMu.Lock()
	d.slotStarvedSince = time.Now().Add(-slotStarveWarnAfter - time.Minute)
	d.slotStarveMu.Unlock()
	d.trackSlotStarvation()
	first := buf.String()
	if first == "" {
		t.Fatal("no WARN after the idle threshold elapsed")
	}
	if !strings.Contains(first, "configured memory budget") {
		t.Errorf("WARN does not carry the head-of-buffer blocking reason: %s", first)
	}

	// An immediate re-check is throttled.
	d.trackSlotStarvation()
	if buf.String() != first {
		t.Errorf("second WARN within the %s throttle interval: %s", slotStarveWarnInterval, buf.String())
	}

	// Recovery: an admissible unit ends the starvation and resets the state.
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{
		ID:            "00000000-0000-4000-8000-0000000000fe",
		LeafID:        "leaf-small",
		RscFpopsEst:   600,
		ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 512},
	}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	d.trackSlotStarvation()
	d.slotStarveMu.Lock()
	since := d.slotStarvedSince
	d.slotStarveMu.Unlock()
	if !since.IsZero() {
		t.Error("starvation state not reset after an admissible unit was buffered")
	}
}
