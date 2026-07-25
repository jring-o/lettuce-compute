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
// non-PUBLIC leaf — was suppressed by a PREDICTIVE GATE that tried to decide when the
// global refill made it redundant. A co-resident PUBLIC leaf with queued work fills the
// pool and hides the whole defect, which is why every test written for the fix wave
// passed: none of them ran a head with NO public backlog. These tests all do.
//
// Two such gates shipped and both starved a pinned volunteer indefinitely:
//
//   - `readyNonEmpty` — skip the leaf-scoped refill while the ready pool is empty. On a
//     head whose only ACTIVE leafs are non-PUBLIC the pool is empty forever, so the
//     refill never fired. This is the filed shape, covered below by the "born hidden"
//     tests (a leaf that is non-PUBLIC from the start, never warmed in the cache).
//   - a coverage check — skip when every named leaf is WARMED in the leaf cache and
//     PUBLIC. It reads `peekLeaf`, and nothing in the head ever invalidates that
//     snapshot: `InvalidateLeaf` has no production caller, `warmLeaf` will not refresh
//     an entry that already exists, and the only refresher (`getLeaf`'s
//     leafSnapshotTTL) runs solely while building an ACCEPTED hand-out — the one thing
//     the starvation prevents. A leaf warmed while PUBLIC and later set UNLISTED read
//     as "covered" permanently. This is the "flipped after warm" shape, covered below.
//
// So the predicate is now unconditional: an empty-handed pinned poll ALWAYS queues the
// leaf-scoped refill. The economy is carried by machinery that predicts nothing —
// per-leaf coalescing in requestLeafRefill, serialization by the single refiller, and
// the maintenance admission budget — which live measurement showed is nowhere near
// binding (~2 000 hammered polls pinning nonexistent leaf ids produced 10 leaf-scoped
// refills, no admission saturation, no control-dispatch latency regression).

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
// PB-16 shape ("born hidden") end to end through the cache's staging path: the ONLY leaf
// on the head is non-PUBLIC and has queued units, there is NO public backlog to make the
// ready pool non-empty, the cache has never warmed the leaf (nothing ever staged it), and
// a volunteer pins it by id. It must be staged and served.
//
// Against the original `readyNonEmpty` gate this fails at the "no work was staged"
// assert: the first hand-out requested no leaf-scoped refill, so leafRefillOnce had
// nothing to service, the pool stayed empty, and every subsequent poll returned nothing —
// the ~9 minutes of QUEUED units the live repro recorded.
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

// TestDispatchCache_LeafFlippedPublicToHidden_PinnedRequesterIsStagedAndServed is the
// "flipped after warm" shape — an ordinary operator flow (publish a leaf, then decide it
// should be unlisted) and the one that broke the first re-fix.
//
// The leaf was PUBLIC when its earlier units were dispatched, so the dispatch cache holds
// a snapshot saying PUBLIC. The operator then flips it to UNLISTED/PRIVATE and generates
// fresh units. Nothing in the head drops or refreshes that snapshot — InvalidateLeaf has
// no production caller, warmLeaf will not refresh an existing entry, and getLeaf's TTL is
// consulted only while building an ACCEPTED hand-out, which cannot happen while the leaf
// is starved. So the cache keeps answering PUBLIC indefinitely while the DB says hidden.
//
// Against the coverage gate this was permanent starvation: the leaf read as "covered by
// the watermark refill", the leaf-scoped refill was never requested, and the global refill
// stages PUBLIC leafs only (per the DB, which says hidden) — 50 polls over 147 s produced
// 0 hand-outs and 0 refills on a live head, curable only by a restart. With the gate gone
// the pinned poll stages the leaf on the spot.
//
// Note what is deliberately NOT asserted here: that an any-leaf requester is refused. The
// in-memory visibility gate also reads the stale PUBLIC snapshot, so for up to
// leafSnapshotTTL after a flip it can still hand out the leaf's units — the already-filed
// and accepted PB-38b window. That direction self-corrects (a hand-out is happening, so
// getLeaf refreshes); the starvation direction never did, which is the asymmetry this
// test exists for.
func TestDispatchCache_LeafFlippedPublicToHidden_PinnedRequesterIsStagedAndServed(t *testing.T) {
	for _, vis := range []leaf.LeafVisibility{leaf.VisibilityUnlisted, leaf.VisibilityPrivate} {
		t.Run(string(vis), func(t *testing.T) {
			ctx := context.Background()
			leafID := types.NewID()
			units := []types.ID{types.NewID(), types.NewID(), types.NewID()}

			// The DB truth AFTER the flip: the leaf is hidden, so the dispatchable query
			// returns its units only for a LEAF-SCOPED refill that names it.
			wuRepo := unlistedOnlyHeadRepo(leafID, units)
			leafRepo := &fakeLeafRepo{}
			c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

			// The cache's snapshot is the one warmed BEFORE the flip, while the leaf was
			// still PUBLIC and its earlier units were being dispatched.
			pub := nativeLeaf(leafID, 1, false, 0)
			pub.Visibility = leaf.VisibilityPublic
			c.warm(pub, leafRepo)
			// ...and the repo (DB truth) now diverges from it: hidden. Nothing reconciles
			// the two, which is exactly the live head's state.
			seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, vis))
			if cached := c.peekLeaf(leafID); cached == nil || cached.Visibility != leaf.VisibilityPublic {
				t.Fatalf("test setup: the cached snapshot must still read PUBLIC (the stale state under test), got %v", cached)
			}

			// The head's own watermark refill honours the DB, not the snapshot: it stages
			// nothing for a hidden leaf, so the shared ready pool is empty.
			c.refillOnce(ctx)
			if got := c.readyLen(); got != 0 {
				t.Fatalf("the global refill staged %d unit(s) for a leaf the DB says is %s; the pool must be empty (PB-38)", got, vis)
			}

			// First poll from the pinned volunteer: nothing is staged yet, so it gets
			// nothing — but it must leave an on-demand leaf-scoped refill request behind,
			// stale PUBLIC snapshot notwithstanding.
			pinVol := types.NewID()
			pinOpts := capableOpts(pinVol, 0)
			pinOpts.LeafIDs = []types.ID{leafID}
			if got, _ := c.HandOut(pinVol, pinOpts, 2); len(got) != 0 {
				t.Fatalf("first poll cannot serve from an empty pool, got %d unit(s)", len(got))
			}

			c.leafRefillOnce(ctx)
			if c.readyLen() == 0 {
				t.Fatalf("no work was staged for a leaf flipped PUBLIC->%s after it was warmed: the hand-out never requested a leaf-scoped refill, so the pinned volunteer starves until the head is restarted (PB-16)", vis)
			}

			got, _ := c.HandOut(pinVol, pinOpts, 2)
			if len(got) == 0 {
				t.Fatalf("pinned requester must be served the flipped leaf's staged work, got nothing (PB-16)")
			}
			for _, r := range got {
				if r.unit.LeafID != leafID {
					t.Fatalf("served a unit of leaf %s, want the pinned leaf %s", r.unit.LeafID, leafID)
				}
			}
		})
	}
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
// leaf the cache has never warmed. That is the state a non-PUBLIC-only head is in at
// boot — nothing has staged the leaf, so nothing has warmed it — and it is also every
// pinned poll for a leaf id that does not exist at all.
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

// TestHandOut_EmptyPool_PinnedWarmedPublicLeaf_StillRequestsLeafRefill is the direct,
// predicate-level statement of the rule that replaced the coverage gate, and it is the
// smallest expression of the "flipped after warm" break: a leaf the cache has warmed as
// PUBLIC gets the leaf-scoped refill anyway.
//
// It REPLACES TestHandOut_EmptyPool_PinnedPublicLeaf_SkipsRedundantLeafRefill, which
// asserted the opposite — that a warmed PUBLIC leaf on an empty pool is covered by the
// watermark refill and needs no leaf-scoped query. That is true only if the snapshot is
// accurate, and the cache has no invalidation path that would make it so: "warmed as
// PUBLIC" carries no information about the leaf's CURRENT visibility, so the skip it
// pinned is precisely the permanent starvation R2 reproduced on a live head.
func TestHandOut_EmptyPool_PinnedWarmedPublicLeaf_StillRequestsLeafRefill(t *testing.T) {
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
	pending := c.drainLeafRefills()
	if len(pending) != 1 || pending[0] != leafID {
		t.Fatalf("an empty-handed pinned poll must always queue a leaf-scoped refill — a cached PUBLIC snapshot proves nothing about the leaf's current visibility (PB-16) — got %v", pending)
	}
}

// TestHandOut_RepeatedEmptyPinnedPolls_CoalesceIntoOneLeafRefill pins the economy that
// actually carries the unconditional refill: requestLeafRefill is a SET keyed by leaf id,
// so a starved volunteer polling in a tight loop — or many volunteers pinning the same
// leaf — costs the refiller ONE query per drain, not one per poll. That, plus the single
// refiller goroutine and the maintenance admission budget, is the whole cost model; no
// prediction about the watermark refill is needed or wanted.
func TestHandOut_RepeatedEmptyPinnedPolls_CoalesceIntoOneLeafRefill(t *testing.T) {
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(unlistedLeaf(leafID, leaf.VisibilityUnlisted), leafRepo)

	// Twenty polls from twenty distinct volunteers, all pinning the same leaf, all
	// handed nothing (the pool is empty and nothing stages a hidden leaf but this).
	for i := 0; i < 20; i++ {
		vol := types.NewID()
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{leafID}
		if got, _ := c.HandOut(vol, opts, 1); len(got) != 0 {
			t.Fatalf("empty pool cannot serve, got %d unit(s)", len(got))
		}
	}
	pending := c.drainLeafRefills()
	if len(pending) != 1 || pending[0] != leafID {
		t.Fatalf("20 empty-handed pinned polls for one leaf must coalesce into a single pending refill, got %v", pending)
	}
	if pending := c.drainLeafRefills(); len(pending) != 0 {
		t.Fatalf("the drain must clear the pending set, got %v", pending)
	}
}
