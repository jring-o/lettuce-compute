package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-33: every slot start passes through a queue→slot handoff — the unit is
// popped from the prefetch queue (PopFit) before the slot is marked active
// (StartSlot). Before this fix the unit was counted NOWHERE during that
// window: bufferedUnitCount, bufferedSeconds, heldWorkUnitIDs, and
// idleSlotStarved all read queue+active and briefly undercounted by one. A
// fetch decision landing in the window judged the buffer one unit emptier
// than it is, requested one more unit, and the TB-32 arrival guard
// (bufferAccepts) then bounced that unit back to the head once the count
// recovered — a needless abandon/re-dispatch round-trip, and the intermittent
// CI failure of TestDaemonMultipleWorkUnits (2 of 3 units processed).
//
// These tests pin the invariant directly: a unit mid-handoff stays in every
// piece of buffer arithmetic, and once the slot turns active the brief
// starting/active overlap counts it once, not twice.

// tb33HandoffDaemon: 1 slot, no estimates (the unit-count fallback domain the
// flaky test ran in — fallback target = 2 units/slot × 1 slot), two units
// buffered, so the buffer is exactly at target before any handoff begins.
func tb33HandoffDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newBufferTestDaemon(t, 2.0, 1, 1.0)
	for _, id := range []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	} {
		if err := d.prefetchQueue.Push(bufItem(id, 0)); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	return d
}

func TestTB33_UnitInSlotHandoffStaysCounted(t *testing.T) {
	d := tb33HandoffDaemon(t)

	// Baseline: two queued units, at the fallback target — full, and an
	// arriving unit is refused.
	if got := d.bufferedUnitCount(); got != 2 {
		t.Fatalf("bufferedUnitCount = %d, want 2", got)
	}
	if !d.workBufferHoursFull() {
		t.Fatal("buffer at the fallback target should report hours-full")
	}

	// Begin the handoff exactly as fillSlots does.
	item := d.prefetchQueue.PopFit(func(*PreFetchItem) bool { return true })
	if item == nil {
		t.Fatal("PopFit returned nil")
	}

	// THE regression: mid-handoff, nothing may change from the outside view.
	if got := d.bufferedUnitCount(); got != 2 {
		t.Errorf("bufferedUnitCount mid-handoff = %d, want 2 — the popped unit vanished from the count (TB-33)", got)
	}
	if !d.workBufferHoursFull() {
		t.Error("workBufferHoursFull = false mid-handoff — a fetch decision in this window requests a unit the arrival guard will bounce (TB-33)")
	}
	if ok, _ := d.bufferAccepts(&runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000003"}); ok {
		t.Error("bufferAccepts accepted a unit mid-handoff that a stable count would refuse (TB-33)")
	}
	found := false
	for _, id := range d.heldWorkUnitIDs() {
		if id == item.WU.ID {
			found = true
		}
	}
	if !found {
		t.Error("heldWorkUnitIDs omits the mid-handoff unit — a concurrent fetch could be handed the same unit twice (TB-33)")
	}
	// The sole slot is being handed a unit: it is occupied, not starved.
	if d.idleSlotStarved() {
		t.Error("idleSlotStarved = true mid-handoff — would open a spurious starved-backfill fetch round (TB-33)")
	}

	// FinishStart without a slot activation is the abandon path: the unit
	// leaves local custody and the count drops.
	d.prefetchQueue.FinishStart(item.WU.ID)
	if got := d.bufferedUnitCount(); got != 1 {
		t.Errorf("bufferedUnitCount after FinishStart = %d, want 1", got)
	}
}

func TestTB33_HandoffOverlapNotDoubleCounted(t *testing.T) {
	d := tb33HandoffDaemon(t)

	item := d.prefetchQueue.PopFit(func(*PreFetchItem) bool { return true })
	if item == nil {
		t.Fatal("PopFit returned nil")
	}

	// Activate the slot while the unit is still in the starting set — the
	// instant inside fillSlots between StartSlot returning and FinishStart.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	item.Prep = &runtime.PrepareResult{WorkDir: t.TempDir()}
	item.Runtime = &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
		<-blockCh
		return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
	}}
	item.Conn = &ServerConnection{Name: "test-head", VolunteerID: "vol-1", Client: &mockClient{}}
	item.FetchedAt = time.Now()
	slotID := <-d.slotManager.available
	if err := d.slotManager.StartSlot(ctx, slotID, item, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}

	// Overlap instant: the unit is both starting and active — count once.
	if got := d.bufferedUnitCount(); got != 2 {
		t.Errorf("bufferedUnitCount during starting/active overlap = %d, want 2 — the handoff unit is double-counted (TB-33)", got)
	}
	seen := 0
	for _, id := range d.heldWorkUnitIDs() {
		if id == item.WU.ID {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("heldWorkUnitIDs lists the overlap unit %d times, want 1 (TB-33)", seen)
	}

	// After the handoff closes, the active slot alone carries it: no change.
	d.prefetchQueue.FinishStart(item.WU.ID)
	if got := d.bufferedUnitCount(); got != 2 {
		t.Errorf("bufferedUnitCount after handoff = %d, want 2", got)
	}
}
