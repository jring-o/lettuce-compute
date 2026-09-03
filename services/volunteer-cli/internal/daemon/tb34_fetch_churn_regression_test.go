package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-34 regression tests: v0.10.7 clients over-asked batches (a bad per-unit estimate
// divided into the deficit), returned the tail by design (TB-32's arrival guard), and
// nothing learned — the next round recomputed the same ask from the same estimates, the
// fetch gate and arrival acceptance shared one threshold (no deadband), and the DCF only
// learns from completions, which returned units never produce. Live shape: 170–283
// give-backs per host per day; one traced unit handed to one host and returned un-run
// 29 times. Three fixes are pinned here: the fetch-gate hysteresis latch, the
// arrival-estimate learning, and the kept+1 batch-feedback cap (plus the un-run
// give-back wire flag TB-35's head half keys on).

// TestTB34_FetchGateHysteresis: once the buffer fills to the hours target, the fetch
// gate must STAY closed until the buffered work drains below the low-water mark
// (half the target), instead of reopening the moment the fill dips one unit under
// the line — the single-threshold hover that produced the request-and-refuse loop.
func TestTB34_FetchGateHysteresis(t *testing.T) {
	// 2 h × 1 slot = 7200 s target; benchmark 1 ⇒ est seconds == RscFpopsEst.
	d := newBufferTestDaemon(t, 2.0, 1, 1.0)
	for i := 1; i <= 3; i++ {
		d.prefetchQueue.Push(bufItem(fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i), 2400))
	}
	if !d.workBufferFull() {
		t.Fatal("7200s buffered against a 7200s target must report full")
	}

	// Drain to 4800 s (67% of target): UNDER the target but ABOVE the low-water
	// mark — the gate must stay closed. Pre-fix it reopened here, and the ~1-unit
	// top-up it asked for is exactly the hover the tester's logs show.
	d.prefetchQueue.Pop()
	if !d.workBufferFull() {
		t.Error("fetch gate reopened one unit under the target — hysteresis latch missing (TB-34)")
	}

	// Drain to 2400 s (33% < the 50% low-water mark): the latch releases and one
	// refill round may fill back to the target.
	d.prefetchQueue.Pop()
	if d.workBufferFull() {
		t.Error("fetch gate still closed below the low-water mark — the buffer would run dry")
	}
}

// TestTB34_ArrivalEstimateCorrectsOverAsk is the filed repro's arithmetic: a leaf-level
// 60 s estimate against a 7200 s deficit asks for the 64-unit ceiling, but the units
// that actually arrive are 3600 s each. The arrival-side estimate must correct the very
// next ask — the DCF cannot (it learns only from completions, and the returned tail
// never completes, which is what made the loop self-sustaining).
func TestTB34_ArrivalEstimateCorrectsOverAsk(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 1.0) // 7200 s target, empty buffer
	leaf := CachedLeafInfo{ID: "leaf-x", EstimatedDurationSeconds: 60}

	first := d.requestBatchSize(leaf, d.leafEstSeconds(leaf))
	if first != maxBatchPerRequest {
		t.Fatalf("first ask = %d, want %d (the over-ask this bug is about)", first, maxBatchPerRequest)
	}

	// One 3600 s unit arrives (benchmark 1 ⇒ fpops == seconds); the estimate learns.
	d.noteArrivalEstimate("leaf-x", 3600)
	second := d.requestBatchSize(leaf, d.leafEstSeconds(leaf))
	if second != 2 {
		t.Errorf("ask after a 3600s arrival = %d, want 2 (7200s deficit ÷ learned 3600s/unit)", second)
	}
}

// TestTB34_ReturnedTailCapsNextAsk drives the fetcher end to end through a mock head:
// a 64-unit ask whose tail the buffer returns must (a) flag each return as an un-run
// give-back on the wire (the TB-35 discriminator) and (b) cap the NEXT ask for that
// head+leaf at kept+1 — the bound that holds even while the estimates are still wrong.
func TestTB34_ReturnedTailCapsNextAsk(t *testing.T) {
	var askedMax []int32
	call := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			askedMax = append(askedMax, req.MaxAssignments)
			call++
			asgs := make([]*lettucev1.WorkUnitAssignment, 3)
			for i := range asgs {
				asgs[i] = &lettucev1.WorkUnitAssignment{
					WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-0000000%02d%03d", call, i),
					LeafId:        "leaf-1",
					Runtime:       "native",
					InputData:     []byte("input"),
					ExecutionSpec: &lettucev1.ExecutionSpec{},
				}
			}
			return &lettucev1.RequestWorkUnitResponse{Assignments: asgs}, nil
		},
	}
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(16, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.batchSizeFn = func(CachedLeafInfo, float64) int32 { return 64 }
	// Accept the first arrival of each round, refuse the rest — the arrival guard's
	// shape when a batch overshoots the target.
	acceptedThisRound := 0
	f.bufferAcceptsFn = func(*runtime.WorkUnit) (bool, string) {
		acceptedThisRound++
		if acceptedThisRound == 1 {
			return true, ""
		}
		return false, "work buffer full (over the hours target)"
	}

	leaf := CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}
	pushed, stop := f.requestAndBuffer(context.Background(), servers[0], leaf, []string{leaf.ID}, nil)
	if stop || pushed != 1 {
		t.Fatalf("round 1: pushed=%d stop=%v, want pushed=1 (tail of 2 returned)", pushed, stop)
	}
	if req := mc.lastAbandonReq; req == nil || !req.UnrunGiveback {
		t.Fatalf("buffer give-back must set unrun_giveback on the wire, got %+v", mc.lastAbandonReq)
	}

	acceptedThisRound = 0
	if _, _ = f.requestAndBuffer(context.Background(), servers[0], leaf, []string{leaf.ID}, nil); len(askedMax) != 2 {
		t.Fatalf("expected a second RequestWorkUnit, got %d calls", len(askedMax))
	}
	if askedMax[0] != 64 {
		t.Fatalf("round 1 asked %d, want the uncapped 64", askedMax[0])
	}
	if askedMax[1] != 2 {
		t.Errorf("round 2 asked %d, want 2 (kept 1 + 1 after a returned tail) — nothing learned (TB-34)", askedMax[1])
	}
}

// TestTB34_CapabilityAbandonIsNotAGiveback guards the discriminator's other half: an
// abandon that carries information about the unit (here: an invalid work-unit id; same
// path as no-runtime and failed-prepare abandons) must NOT be flagged un-run give-back —
// the head keeps billing those against the unit's budget so poison units still die.
func TestTB34_CapabilityAbandonIsNotAGiveback(t *testing.T) {
	mc := &mockClient{}
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(8, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)

	leaf := CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", State: "ACTIVE"}
	f.bufferBatch(context.Background(), servers[0], leaf, []*lettucev1.WorkUnitAssignment{
		{WorkUnitId: "../../etc/passwd", LeafId: "leaf-1", Runtime: "native", ExecutionSpec: &lettucev1.ExecutionSpec{}},
	})
	if req := mc.lastAbandonReq; req == nil || req.UnrunGiveback {
		t.Fatalf("capability abandon must NOT carry unrun_giveback, got %+v", mc.lastAbandonReq)
	}
}

// TestTB34_ShutdownReturnIsAGiveback: the daemon's buffered-unit return path
// (shutdown clear / failed slot start) is an un-run give-back and must say so.
func TestTB34_ShutdownReturnIsAGiveback(t *testing.T) {
	mc := &mockClient{}
	d := newFetcherTestDaemon([]*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}})
	d.abandonItem(&PreFetchItem{
		WU:   &runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000001", LeafID: "leaf-1"},
		Conn: d.multiClient.Servers()[0],
	}, "volunteer shutdown")
	if req := mc.lastAbandonReq; req == nil || !req.UnrunGiveback {
		t.Fatalf("shutdown return must carry unrun_giveback, got %+v", mc.lastAbandonReq)
	}
}

// TestTB34_RemainingTimeRefillTrigger pins the tester's design input: a running unit
// counts toward the refill trigger at its REMAINING time, not its full booking, so
// "2 h buffered" that is really 1 h of runway refills on time. Acceptance
// (bufferedSeconds) keeps the conservative full booking.
func TestTB34_RemainingTimeRefillTrigger(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 1.0) // 7200 s target

	// Occupy the slot with a blocking 7200 s unit.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	slotID := <-d.slotManager.available
	if err := d.slotManager.StartSlot(ctx, slotID, &PreFetchItem{
		WU:   &runtime.WorkUnit{ID: "00000000-0000-4000-8000-0000000000aa", LeafID: "leaf-1", RscFpopsEst: 7200},
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

	// Rewind the slot's start to 45 min ago: full booking 7200 s, remaining
	// ≈ 4500 s — clearly ABOVE the 3600 s low-water line, so the comparison
	// cannot flake on scheduler latency (an exact-boundary probe did, on CI).
	slot := d.slotManager.slots[slotID]
	slot.mu.Lock()
	slot.startedAt = time.Now().Add(-45 * time.Minute)
	slot.mu.Unlock()

	if got := d.bufferedSeconds(); got < 7000 {
		t.Fatalf("bufferedSeconds = %g, want the full 7200 booking (acceptance stays conservative)", got)
	}
	rem := d.bufferedRemainingSeconds()
	if rem < 4300 || rem > 4700 {
		t.Fatalf("bufferedRemainingSeconds = %g, want ≈4500 (est − 45min run)", rem)
	}
	// 4500 s remaining is NOT yet under the 3600 s low-water line…
	if d.bufferBelowLowWater() {
		t.Error("bufferBelowLowWater true well above the mark, want false")
	}
	// …but at 90 minutes in (1800 s remaining < 3600 s) the refill trigger opens
	// while the unit is STILL RUNNING — the next unit is fetched before the slot
	// idles, which the full-booking view delayed until the unit finished.
	slot.mu.Lock()
	slot.startedAt = time.Now().Add(-90 * time.Minute)
	slot.mu.Unlock()
	if !d.bufferBelowLowWater() {
		t.Error("bufferBelowLowWater false at 1800s remaining against a 3600s low-water mark (TB-34: refill starts too late)")
	}
}
