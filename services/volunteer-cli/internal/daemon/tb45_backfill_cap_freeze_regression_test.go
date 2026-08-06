package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-45 regression tests: the backfill starvation cap (TB-22) stopped PopFit's
// SCAN at a capped head-of-queue unit instead of merely protecting that unit,
// so a fitting unit buffered behind it was never even tested — one of two
// slots idled 43 minutes beside an admissible 768 MB unit (tester QuaXeros,
// 2026-08-06, budget 8192: a 7000 MB GREP running, a capped 7000 MB GREP at
// the head, Beyblades backfilling). And the watchdog built for exactly this
// outcome stayed silent: idleSlotStarved judged reachability with a
// whole-queue canAccommodateWU scan — a DIFFERENT predicate than the picker —
// found the fitting unit, concluded "fillSlots will" start it, and muted the
// TB-32 WARN. The cap may block only backfills that could actually delay the
// capped unit's own admission, and the watchdog must ask the picker's own
// scan.

// tb45Item builds a buffered item declaring memMB.
func tb45Item(id string, memMB int32) *PreFetchItem {
	return &PreFetchItem{WU: &runtime.WorkUnit{
		ID:            id,
		LeafID:        "leaf-" + id,
		ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: memMB},
	}, FetchedAt: time.Now()}
}

// TestTB45_CappedHeadDoesNotFreezeHarmlessBackfill is the filed red test: a
// capped, unfitting head with a fitting unit behind it. The tester's geometry:
// the head (7000 MB) waits for the running 7000 MB unit to finish whatever the
// backfills do — a 768 MB unit's booking coexists with the head's inside the
// 8192 MB budget, so starting it cannot delay the head's admission by one
// second and must not be blocked by the head's skip cap.
func TestTB45_CappedHeadDoesNotFreezeHarmlessBackfill(t *testing.T) {
	q := NewPreFetchQueue(5, newTestLogger())

	head := tb45Item("wu-grep-head", 7000)
	head.TimesSkipped = maxBackfillStarts
	small := tb45Item("wu-beyblade", 768)
	q.Push(head)
	q.Push(small)

	neverDelays := func(_, _ *PreFetchItem) bool { return false }
	got := q.PopFit(func(it *PreFetchItem) bool { return it.WU.ID == "wu-beyblade" }, neverDelays)
	if got == nil || got.WU.ID != "wu-beyblade" {
		t.Fatalf("PopFit = %v, want wu-beyblade — the capped head froze the queue and idled the slot (TB-45)", got)
	}
	if items := q.Items(); len(items) != 1 || items[0].WU.ID != "wu-grep-head" {
		t.Errorf("remaining queue = %v, want the head alone, in place", items)
	}
	if head.TimesSkipped != maxBackfillStarts {
		t.Errorf("head.TimesSkipped = %d, want %d — a harmless pass must not count against the head's tolerance", head.TimesSkipped, maxBackfillStarts)
	}
}

// TestTB45_CapStillBlocksDelayingBackfill: the cap's protection survives the
// fix — a candidate that COULD delay the capped head's admission is refused,
// so running work drains and the head gets the next free slot.
func TestTB45_CapStillBlocksDelayingBackfill(t *testing.T) {
	q := NewPreFetchQueue(5, newTestLogger())

	head := tb45Item("wu-grep-head", 7000)
	head.TimesSkipped = maxBackfillStarts
	mid := tb45Item("wu-mid", 4096)
	q.Push(head)
	q.Push(mid)

	got := q.PopFit(func(it *PreFetchItem) bool { return it.WU.ID == "wu-mid" }, alwaysDelays)
	if got != nil {
		t.Fatalf("PopFit = %v, want nil — a delaying backfill passed a capped head", got.WU.ID)
	}
	if q.Len() != 2 {
		t.Errorf("len = %d, want 2 (queue must be untouched)", q.Len())
	}
}

// TestTB45_OnlyDelayingJumpsCount: TimesSkipped is the cap's trigger, so a
// jump that cannot delay the waiting unit must not accrue toward it. The
// tester's counts came almost entirely from harmless 768 MB Beyblades passing
// two parked 7000 MB GREPs — jumps that cost the GREPs nothing but tripped
// the cap the moment one reached the head of the queue.
func TestTB45_OnlyDelayingJumpsCount(t *testing.T) {
	q := NewPreFetchQueue(5, newTestLogger())

	waiting := tb45Item("wu-grep-head", 7000)
	small := tb45Item("wu-beyblade", 768)
	q.Push(waiting)
	q.Push(small)

	fitsSmall := func(it *PreFetchItem) bool { return it.WU.ID != "wu-grep-head" }
	neverDelays := func(_, _ *PreFetchItem) bool { return false }
	if got := q.PopFit(fitsSmall, neverDelays); got == nil || got.WU.ID != "wu-beyblade" {
		t.Fatalf("PopFit = %v, want wu-beyblade", got)
	}
	if waiting.TimesSkipped != 0 {
		t.Errorf("TimesSkipped = %d after a harmless jump, want 0 (TB-45)", waiting.TimesSkipped)
	}

	q.Push(tb45Item("wu-mid", 4096))
	if got := q.PopFit(fitsSmall, alwaysDelays); got == nil || got.WU.ID != "wu-mid" {
		t.Fatalf("PopFit = %v, want wu-mid", got)
	}
	if waiting.TimesSkipped != 1 {
		t.Errorf("TimesSkipped = %d after a delaying jump, want 1", waiting.TimesSkipped)
	}
}

// TestTB45_MayDelayAdmission pins the delay test to the tester's real
// arithmetic: budget 8192, a 7000 MB unit waiting. A 768 MB backfill's booking
// coexists with the waiting unit's (7000+768 <= 8192) and is harmless; a
// 4096 MB backfill's does not (7000+4096 > 8192) and may delay it.
func TestTB45_MayDelayAdmission(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 2, 1.0)
	d.cfg.ResourceLimits.MaxMemoryMB = 8192

	blocked := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 7000}}
	if d.mayDelayAdmission(blocked, &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 768}}) {
		t.Error("mayDelayAdmission = true for a 768 MB unit beside a 7000 MB blocked unit at budget 8192 — the exact backfill the freeze blocked (TB-45)")
	}
	if !d.mayDelayAdmission(blocked, &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 4096}}) {
		t.Error("mayDelayAdmission = false for a 4096 MB unit that cannot coexist with the blocked unit's booking")
	}
	// Unknown shapes keep the cap's protection.
	if !d.mayDelayAdmission(nil, &runtime.WorkUnit{}) {
		t.Error("mayDelayAdmission = false for a nil blocked unit, want true (conservative)")
	}
}

// TestTB45_PickerFreezeIsVisibleToStarvationWatchdog: whenever the picker —
// for any reason — leaves a slot idle beside buffered work, idleSlotStarved
// must say so, sharing the picker's own reachability instead of running a
// parallel scan that can disagree with it. Geometry where the picker refuses
// by design even after the fix: budget 8192, a 3000 MB unit running, a capped
// 7000 MB head (3000+7000 > 8192, unfitting), and a 4096 MB candidate that
// FITS beside the running unit (3000+4096 <= 8192) but could delay the capped
// head's own admission (7000+4096 > 8192) — so the cap rightly holds it, the
// slot idles, and the TB-32 WARN must be armed, not muted.
func TestTB45_PickerFreezeIsVisibleToStarvationWatchdog(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 2, 1.0)
	d.cfg.ResourceLimits.MaxMemoryMB = 8192

	// Pin real free RAM so only the configured budget decides admission,
	// keeping the test hermetic on small CI hosts.
	origFree := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 64000, true }
	t.Cleanup(func() { freeSystemMemoryMB = origFree })

	// One 3000 MB unit running: occupy a slot with a blocking execution.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	slotID := <-d.slotManager.available
	if err := d.slotManager.StartSlot(ctx, slotID, &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:            "00000000-0000-4000-8000-0000000000aa",
			LeafID:        "leaf-running",
			RscFpopsEst:   7200,
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 3000},
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

	head := tb45Item("wu-grep-head", 7000)
	head.TimesSkipped = maxBackfillStarts
	cand := tb45Item("wu-mid", 4096)
	if err := d.prefetchQueue.Push(head); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := d.prefetchQueue.Push(cand); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Sanity: the candidate is admissible beside the running unit, the head is
	// not — the exact disagreement geometry.
	if ok, reason := d.canAccommodateWU(cand.WU); !ok {
		t.Fatalf("candidate unexpectedly inadmissible (%s) — state does not reproduce the defect", reason)
	}
	if ok, _ := d.canAccommodateWU(head.WU); ok {
		t.Fatal("head unexpectedly admissible — state does not reproduce the defect")
	}

	// THE regression: a slot is idle and the picker will not (and must not)
	// start anything, so the starvation verdict must be TRUE — it was false,
	// which muted the TB-32 WARN for the whole 43-minute freeze.
	if !d.idleSlotStarved() {
		t.Error("idleSlotStarved = false while the picker leaves a slot idle — the TB-32 WARN specified for exactly this outcome stays muted (TB-45)")
	}

	// Counterfactual: a harmless unit the picker CAN reach (768 MB passes the
	// capped head — the fixed picker's own scan) ends the starvation verdict.
	if err := d.prefetchQueue.Push(tb45Item("wu-beyblade", 768)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if d.idleSlotStarved() {
		t.Error("idleSlotStarved = true with a unit the picker can start buffered — the watchdog disagrees with the picker again")
	}
}
