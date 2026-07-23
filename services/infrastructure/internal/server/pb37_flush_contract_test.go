package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// New-seam contract test for PB-37 (green-only — the bool return does not exist in
// pre-fix trees): flushPendingFor must report FALSE when the caller's ctx expires
// while a snapshotted flush batch is still in flight, and TRUE once everything
// pending has landed. This is the seam StartWork / AbandonWorkUnit key their
// wait-or-fail-atomically behavior on.
func TestFlushPendingFor_ReportsIncompleteUnderSaturation(t *testing.T) {
	leafID := types.NewID()
	unitID := types.NewID()
	vol := types.NewID()

	fake := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(fake, leafRepo, &fakeAssignRepo{})
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	c.stageUnit(unitID, leafID, 2, 0)
	if got, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(got) != 1 {
		t.Fatalf("hand-out failed: got %d results", len(got))
	}

	// Saturate the maintenance slot and let the ticker snapshot the batch out of the
	// queue (the in-flight window).
	c.maintenanceAdmission <- struct{}{}
	go c.flushOnce(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		inFlight := c.flushInFlight
		queued := len(c.pendingWrites)
		c.mu.Unlock()
		if inFlight == 1 && queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ticker batch never entered the in-flight window")
		}
		time.Sleep(5 * time.Millisecond)
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c.flushPendingFor(shortCtx) {
		t.Fatal("flushPendingFor must report INCOMPLETE while the in-flight batch is stuck beyond the ctx budget")
	}

	// Free the slot: the batch lands, and a fresh drain reports complete.
	<-c.maintenanceAdmission
	done, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !c.flushPendingFor(done) {
		t.Fatal("flushPendingFor must report COMPLETE once the in-flight batch has landed")
	}
}
