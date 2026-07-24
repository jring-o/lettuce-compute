package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// PB-16 regression (dispatch-cache staging half): a volunteer that pins an UNLISTED
// leaf by id received work only if the head ALSO had a PUBLIC leaf with queued work.
//
// The selection SQL was already correct (a non-PUBLIC leaf's units are served to a
// requester that named the leaf id, and a leaf-scoped refill stages them — see
// internal/workunit/visibility_dispatch_test.go), and so was the in-memory hand-out
// (TestHandOut_NonPublicLeafOnlyToPinnedRequester). What starved was the STAGING step
// between them: gRPC RequestWorkUnit serves only from the in-process ready pool, the
// global watermark refill stages PUBLIC leafs only (the PB-38 visibility gate), and
// HandOut's on-demand leaf-scoped refill — the one path that stages a pinned
// non-PUBLIC leaf — was gated on the ready pool being NON-EMPTY. On a head whose only
// ACTIVE leafs are UNLISTED/PRIVATE the pool is permanently empty, so that gate never
// opened and the pinned volunteer polled forever against a pool nothing would ever
// fill. A co-resident PUBLIC leaf with queued work makes the pool non-empty and hides
// the defect entirely, which is why every test written for the fix wave passed: none
// of them ran a head with NO public backlog. These tests all do.

// unlistedOnlyHeadRepo fakes the work-unit repo of a head whose ONLY ACTIVE leaf is
// UNLISTED (or PRIVATE) and holds queued work — no PUBLIC leaf, no PUBLIC backlog. Its
// dispatchFn is faithful to the visibility gate of the real dispatchable query
// (`l.visibility = 'PUBLIC' OR wu.leaf_id = ANY($3)`): a GLOBAL refill (empty leaf
// scope) returns NOTHING on such a head, and the units come back only for a LEAF-SCOPED
// refill that names the leaf — the pin-by-id opt-in.
func unlistedOnlyHeadRepo(leafID types.ID, unitIDs []types.ID) *fakeWURepo {
	var mu sync.Mutex
	repo := &fakeWURepo{}
	repo.dispatchFn = func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
		mu.Lock()
		defer mu.Unlock()
		if !containsID(leafIDs, leafID) {
			// Global (watermark) refill: PUBLIC leafs only — this head has none.
			return nil, nil
		}
		excluded := make(map[types.ID]struct{}, len(excludeIDs))
		for _, id := range excludeIDs {
			excluded[id] = struct{}{}
		}
		out := make([]workunit.DispatchCandidate, 0, len(unitIDs))
		for _, uid := range unitIDs {
			if len(out) >= limit {
				break
			}
			if _, skip := excluded[uid]; skip {
				continue
			}
			out = append(out, workunit.DispatchCandidate{
				WorkUnit:          &workunit.WorkUnit{ID: uid, LeafID: leafID, State: workunit.WorkUnitStateQueued},
				LeafID:            leafID,
				RedundancyFactor:  1,
				ActiveAssignments: 0,
			})
		}
		return out, nil
	}
	return repo
}

// seedLeafRepoOnly stores the leaf in the REPO but NOT in the cache's metadata map —
// the cold state of a head whose only leaf is non-PUBLIC. Nothing has ever staged that
// leaf, so nothing has ever warmed it; the leaf-scoped refill is what warms it.
func seedLeafRepoOnly(leafRepo *fakeLeafRepo, lf *leaf.Leaf) {
	leafRepo.mu.Lock()
	defer leafRepo.mu.Unlock()
	if leafRepo.leafs == nil {
		leafRepo.leafs = map[types.ID]*leaf.Leaf{}
	}
	leafRepo.leafs[lf.ID] = lf
}

// unlistedLeaf builds an ACTIVE NATIVE leaf with the given non-PUBLIC visibility.
func unlistedLeaf(id types.ID, vis leaf.LeafVisibility) *leaf.Leaf {
	lf := nativeLeaf(id, 1, false, 0)
	lf.Visibility = vis
	return lf
}

// TestDispatchCache_NonPublicOnlyHead_PinnedRequesterIsStagedAndServed is the filed
// PB-16 shape end to end through the cache's staging path: the ONLY leaf on the head is
// non-PUBLIC and has queued units, there is NO public backlog to make the ready pool
// non-empty, and a volunteer pins the leaf by id. It must be staged and served.
//
// Pre-fix this fails at the "pinned requester must now be served" assert: the first
// hand-out requested no leaf-scoped refill (the readyNonEmpty gate), so leafRefillOnce
// had nothing to service, the pool stayed empty, and every subsequent poll returned
// nothing — the ~9 minutes of QUEUED units the live repro recorded.
func TestDispatchCache_NonPublicOnlyHead_PinnedRequesterIsStagedAndServed(t *testing.T) {
	for _, vis := range []leaf.LeafVisibility{leaf.VisibilityUnlisted, leaf.VisibilityPrivate} {
		t.Run(string(vis), func(t *testing.T) {
			ctx := context.Background()
			leafID := types.NewID()
			units := []types.ID{types.NewID(), types.NewID(), types.NewID()}

			wuRepo := unlistedOnlyHeadRepo(leafID, units)
			leafRepo := &fakeLeafRepo{}
			seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, vis))
			c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

			// The head's own watermark refill: on a head with no PUBLIC leaf it stages
			// nothing, so the shared ready pool is empty. This emptiness is the state a
			// co-resident PUBLIC leaf would have hidden.
			c.refillOnce(ctx)
			if got := c.readyLen(); got != 0 {
				t.Fatalf("the global refill staged %d unit(s) on a %s-only head; the pool must be empty (PB-38)", got, vis)
			}

			// First poll from the pinned volunteer: nothing is staged yet, so it gets
			// nothing — but it must leave an on-demand leaf-scoped refill request behind.
			pinVol := types.NewID()
			pinOpts := capableOpts(pinVol, 0)
			pinOpts.LeafIDs = []types.ID{leafID}
			if got, _ := c.HandOut(pinVol, pinOpts, 2); len(got) != 0 {
				t.Fatalf("first poll cannot serve from an empty pool, got %d unit(s)", len(got))
			}

			// The refiller services that request. With no request pending this is a
			// no-op and the pool stays empty — the pre-fix behavior.
			c.leafRefillOnce(ctx)
			if c.readyLen() == 0 {
				t.Fatalf("no work was staged for the pinned %s leaf: the hand-out never requested a leaf-scoped refill, so the pinned volunteer starves forever (PB-16)", vis)
			}

			// PB-38 must still hold on the now-staged units: an any-leaf requester
			// (the volunteer fallback, no leaf filter) is refused.
			anyVol := types.NewID()
			if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 2); len(got) != 0 {
				t.Fatalf("any-leaf requester was handed %d unit(s) of a %s leaf it never pinned (PB-38)", len(got), vis)
			}

			// The pinned volunteer's next poll is served.
			got, _ := c.HandOut(pinVol, pinOpts, 2)
			if len(got) == 0 {
				t.Fatalf("pinned requester must be served the %s leaf's staged work, got nothing (PB-16)", vis)
			}
			for _, r := range got {
				if r.unit.LeafID != leafID {
					t.Fatalf("served a unit of leaf %s, want the pinned leaf %s", r.unit.LeafID, leafID)
				}
			}
		})
	}
}

// TestDispatchCache_UnlistedOnlyHead_LiveRefillerServesPinnedVolunteer is the same
// shape driven by the REAL refiller goroutine rather than direct refill calls, so the
// signal path a live head uses (HandOut -> requestLeafRefill -> leafRefillSignal ->
// leafRefillOnce -> fetchAndStage) is exercised exactly as it runs in production: a
// volunteer polls, and the head must converge on serving it.
func TestDispatchCache_UnlistedOnlyHead_LiveRefillerServesPinnedVolunteer(t *testing.T) {
	leafID := types.NewID()
	units := []types.ID{types.NewID(), types.NewID()}

	wuRepo := unlistedOnlyHeadRepo(leafID, units)
	leafRepo := &fakeLeafRepo{}
	seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, leaf.VisibilityUnlisted))
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.runRefiller(ctx, 10*time.Millisecond)

	pinVol := types.NewID()
	pinOpts := capableOpts(pinVol, 0)
	pinOpts.LeafIDs = []types.ID{leafID}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := c.HandOut(pinVol, pinOpts, 1); len(got) > 0 {
			return // served
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a volunteer pinning the head's only (UNLISTED) leaf polled for 5s and was never served: nothing stages a pinned non-PUBLIC leaf while the ready pool is empty (PB-16)")
}

// TestHandOut_EmptyPool_PinnedNonPublicLeaf_RequestsLeafRefill pins the changed
// predicate directly: an empty ready pool no longer suppresses the on-demand
// leaf-scoped refill for a leaf the watermark refill cannot stage.
func TestHandOut_EmptyPool_PinnedNonPublicLeaf_RequestsLeafRefill(t *testing.T) {
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(unlistedLeaf(leafID, leaf.VisibilityUnlisted), leafRepo)

	vol := types.NewID()
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	if got, _ := c.HandOut(vol, opts, 1); len(got) != 0 {
		t.Fatalf("empty pool cannot serve, got %d unit(s)", len(got))
	}
	pending := c.drainLeafRefills()
	if len(pending) != 1 || pending[0] != leafID {
		t.Fatalf("expected a pending leaf-scoped refill for the pinned UNLISTED leaf, got %v (PB-16)", pending)
	}
}

// TestHandOut_EmptyPool_PinnedUnknownLeaf_RequestsLeafRefill covers the cold cache: a
// leaf the cache has never warmed has unknown visibility, and the fix treats unknown as
// NOT covered by the watermark refill. That is the state a non-PUBLIC-only head is in
// at boot — nothing has staged the leaf, so nothing has warmed it — and assuming
// coverage there is the starvation itself.
func TestHandOut_EmptyPool_PinnedUnknownLeaf_RequestsLeafRefill(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})

	leafID := types.NewID() // never warmed
	vol := types.NewID()
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	if got, _ := c.HandOut(vol, opts, 1); len(got) != 0 {
		t.Fatalf("empty pool cannot serve, got %d unit(s)", len(got))
	}
	pending := c.drainLeafRefills()
	if len(pending) != 1 || pending[0] != leafID {
		t.Fatalf("expected a pending leaf-scoped refill for the pinned un-warmed leaf, got %v", pending)
	}
}

// TestHandOut_EmptyPool_PinnedPublicLeaf_SkipsRedundantLeafRefill pins the refill
// economy the old gate was there for, in the one case where it is provably valid: the
// pool is empty and the pinned leaf is a warmed PUBLIC one, so the watermark refill
// this very hand-out signals (drained) will stage it. No second, leaf-scoped query is
// issued for it.
func TestHandOut_EmptyPool_PinnedPublicLeaf_SkipsRedundantLeafRefill(t *testing.T) {
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	pub := nativeLeaf(leafID, 1, false, 0)
	pub.Visibility = leaf.VisibilityPublic
	c.warm(pub, leafRepo)

	vol := types.NewID()
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	if got, _ := c.HandOut(vol, opts, 1); len(got) != 0 {
		t.Fatalf("empty pool cannot serve, got %d unit(s)", len(got))
	}
	if pending := c.drainLeafRefills(); len(pending) != 0 {
		t.Fatalf("a warmed PUBLIC leaf on an empty pool is covered by the watermark refill; no leaf-scoped refill should be queued, got %v", pending)
	}
}
