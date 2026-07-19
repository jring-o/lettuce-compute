package server

// Coverage for the operator-requeue invalidation hook (PB-9). These tests exercise
// the NEW seam (InvalidateWorkUnit / DispatchCacheRef), so unlike the differential
// regression file they do not compile on the pre-fix tree.

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestInvalidateWorkUnit_DropsCandidateHoldsAndPendingWrites: invalidating a unit
// must remove its staged candidate (with its stale bench/contributor snapshots),
// release every in-memory hold (decrementing inflight), purge its queued
// reservation writes, and request a refill — so the next stage happens from a
// fresh DB snapshot.
func TestInvalidateWorkUnit_DropsCandidateHoldsAndPendingWrites(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatalf("hand-out = %d results, want 1", len(res))
	}
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatal("hold missing after hand-out")
	}
	if n := c.pendingWriteCount(); n != 1 {
		t.Fatalf("pending writes = %d, want 1", n)
	}

	c.InvalidateWorkUnit(unitID)

	if c.hasInMemReservation(unitID, vol) {
		t.Fatal("hold survived InvalidateWorkUnit")
	}
	if n := c.pendingWriteCount(); n != 0 {
		t.Fatalf("pending writes after invalidate = %d, want 0 (a late flush must not resurrect the reservation)", n)
	}
	c.mu.Lock()
	stillStaged := c.readyContainsLocked(unitID)
	inflight := c.inflight[vol]
	c.mu.Unlock()
	if stillStaged {
		t.Fatal("candidate survived InvalidateWorkUnit (stale bench/contributor snapshots would keep serving)")
	}
	if inflight != 0 {
		t.Fatalf("inflight after invalidate = %d, want 0", inflight)
	}
	select {
	case <-c.refillSignal:
	default:
		t.Fatal("InvalidateWorkUnit did not request a refill (the unit would only re-stage on the next watermark tick)")
	}
}

// TestDispatchCacheRef_NilSafeAndForwarding: the late-bound ref is a no-op until a
// cache is bound (the HTTP router is built before StartDispatchCache runs), then
// forwards to the live cache.
func TestDispatchCacheRef_NilSafeAndForwarding(t *testing.T) {
	ref := NewDispatchCacheRef()
	// Unbound: must not panic.
	ref.InvalidateWorkUnit(types.NewID())

	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	ref.set(c)
	ref.InvalidateWorkUnit(unitID)
	c.mu.Lock()
	stillStaged := c.readyContainsLocked(unitID)
	c.mu.Unlock()
	if stillStaged {
		t.Fatal("bound ref did not forward InvalidateWorkUnit to the cache")
	}
}
