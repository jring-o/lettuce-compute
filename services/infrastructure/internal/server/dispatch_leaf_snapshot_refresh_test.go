package server

// Seam-level tests for the PB-16/PB-38b leaf-snapshot staleness fix's NEW machinery:
// the DispatchCacheRef.InvalidateLeaf forwarding the leaf handlers call, the
// mark-stale (not delete) semantics of InvalidateLeaf, and the refiller tick's
// stale-snapshot sweep (refreshStaleLeafSnapshots). The end-to-end behavioral
// differentials live in pb16_stale_snapshot_leak_test.go, which deliberately avoids
// these new symbols so it still compiles against the pre-fix dispatch_cache.go; this
// file is the one to exclude when reproducing RED.

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestDispatchCacheRef_ForwardsInvalidateLeaf: the late-bound ref is a no-op until a
// cache is bound (the router is built before StartDispatchCache), then forwards —
// the same contract as the ref's InvalidateWorkUnit (PB-9).
func TestDispatchCacheRef_ForwardsInvalidateLeaf(t *testing.T) {
	ref := NewDispatchCacheRef()
	ref.InvalidateLeaf(types.NewID()) // unbound: must not panic

	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	if _, fresh := c.peekLeafFresh(leafID); !fresh {
		t.Fatal("setup: a just-warmed snapshot must be fresh")
	}

	ref.set(c)
	ref.InvalidateLeaf(leafID)
	if _, fresh := c.peekLeafFresh(leafID); fresh {
		t.Fatal("bound ref did not forward InvalidateLeaf to the cache (snapshot still trusted)")
	}
}

// TestInvalidateLeaf_MarksStaleKeepsMetadata pins the mark-stale contract: after an
// invalidation the snapshot's metadata is still peekable (capability checks, HR pin,
// spot-check — and the pinned volunteers PB-16 serves — keep working), it is no
// longer trusted as fresh, and the next getLeaf re-reads the database immediately
// rather than waiting out the TTL.
func TestInvalidateLeaf_MarksStaleKeepsMetadata(t *testing.T) {
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)

	c.InvalidateLeaf(leafID)

	if got := c.peekLeaf(leafID); got == nil {
		t.Fatal("InvalidateLeaf must keep the snapshot's metadata peekable (deleting it starves pinned dispatch through rejectLeafNotCached)")
	}
	if _, fresh := c.peekLeafFresh(leafID); fresh {
		t.Fatal("InvalidateLeaf must mark the snapshot stale")
	}

	// The DB truth changes (the mutation that triggered the invalidation); getLeaf
	// must re-read it at once — the original documented purpose of InvalidateLeaf.
	flipped := unlistedLeaf(leafID, leaf.VisibilityUnlisted)
	seedLeafRepoOnly(leafRepo, flipped)
	got, err := c.getLeaf(leafID)
	if err != nil {
		t.Fatalf("getLeaf after invalidation: %v", err)
	}
	if got.Visibility != leaf.VisibilityUnlisted {
		t.Fatalf("getLeaf served the invalidated snapshot (visibility %q), want an immediate re-read (UNLISTED)", got.Visibility)
	}
	if _, fresh := c.peekLeafFresh(leafID); !fresh {
		t.Fatal("the re-read must restore a fresh snapshot")
	}
}

// TestRefreshStaleLeafSnapshots_RefreshesStagedStaleOnly: the tick sweep re-reads
// exactly the leafs that (a) have staged candidates and (b) whose snapshot is
// over-TTL or invalidated — one bounded read per stale staged leaf per TTL, nothing
// for fresh snapshots or un-staged leafs.
func TestRefreshStaleLeafSnapshots_RefreshesStagedStaleOnly(t *testing.T) {
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	// leafStale: staged, snapshot stale, DB truth has flipped back to PUBLIC.
	leafStale := types.NewID()
	ageLeafSnapshot(c, unlistedLeaf(leafStale, leaf.VisibilityUnlisted), 45*time.Second)
	pubTruth := nativeLeaf(leafStale, 1, false, 0)
	pubTruth.Visibility = leaf.VisibilityPublic
	seedLeafRepoOnly(leafRepo, pubTruth)
	c.stageUnit(types.NewID(), leafStale, 1, 0)

	// leafFresh: staged, snapshot fresh, DB truth diverges — must NOT be re-read
	// (freshness is the whole point of the TTL; the sweep is not a hot loop).
	leafFresh := types.NewID()
	freshSnap := nativeLeaf(leafFresh, 1, false, 0)
	c.warm(freshSnap, leafRepo)
	seedLeafRepoOnly(leafRepo, unlistedLeaf(leafFresh, leaf.VisibilityUnlisted))
	c.stageUnit(types.NewID(), leafFresh, 1, 0)

	// leafUnstaged: stale snapshot but nothing staged — not the sweep's business.
	leafUnstaged := types.NewID()
	ageLeafSnapshot(c, unlistedLeaf(leafUnstaged, leaf.VisibilityUnlisted), 45*time.Second)
	seedLeafRepoOnly(leafRepo, nativeLeaf(leafUnstaged, 1, false, 0))

	c.refreshStaleLeafSnapshots(context.Background())

	if got, fresh := c.peekLeafFresh(leafStale); !fresh || got.Visibility != leaf.VisibilityPublic {
		t.Fatalf("staged+stale leaf not refreshed: fresh=%v visibility=%q, want fresh PUBLIC", fresh, got.Visibility)
	}
	if got, _ := c.peekLeafFresh(leafFresh); got != freshSnap {
		t.Fatal("staged+fresh leaf was re-read; the sweep must only touch stale snapshots")
	}
	if _, fresh := c.peekLeafFresh(leafUnstaged); fresh {
		t.Fatal("un-staged leaf was refreshed; the sweep must only touch leafs with staged candidates")
	}
}

// TestRefreshStaleLeafSnapshots_DeletedLeaf_DropsSnapshotAndCandidates: a staged
// leaf the database no longer has (deleted) loses both its snapshot and its staged
// candidates — its units are gone with it, and dropping them stops the sweep from
// re-probing the id every tick.
func TestRefreshStaleLeafSnapshots_DeletedLeaf_DropsSnapshotAndCandidates(t *testing.T) {
	leafRepo := &fakeLeafRepo{} // empty: GetByID resolves to no leaf
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	ageLeafSnapshot(c, nativeLeaf(leafID, 1, false, 0), 45*time.Second)
	c.stageUnit(types.NewID(), leafID, 1, 0)
	c.stageUnit(types.NewID(), leafID, 1, 0)

	c.refreshStaleLeafSnapshots(context.Background())

	if got := c.peekLeaf(leafID); got != nil {
		t.Fatal("deleted leaf's snapshot survived the sweep")
	}
	if got := c.readyLen(); got != 0 {
		t.Fatalf("deleted leaf left %d staged candidate(s) in the ready pool", got)
	}
}
