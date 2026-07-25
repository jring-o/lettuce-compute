package server

// PB-16 / PB-38b regression (leaf-snapshot staleness): the dispatch cache's per-leaf
// snapshot used to be written once and never invalidated — InvalidateLeaf had zero
// production callers, warmLeaf refused to refresh an existing entry, and getLeaf's
// TTL refresh ran only while building an ACCEPTED hand-out, after the eligibility
// decision. eligibleLocked's visibility gate therefore trusted whatever the snapshot
// said, however old, and PR #150's unconditional pinned staging made that reachable
// on an idle head: a leaf drained while PUBLIC, flipped UNLISTED, freshly generated,
// and staged by ONE pinned poll handed its hidden units to any-leaf volunteers that
// pinned nothing — live-reproduced 95 s after the flip, unbounded by the 30 s
// leafSnapshotTTL (PB-16 re-fix closeout #2, refuter R1).
//
// The fix has two mandated halves and these tests pin both:
//
//   - EVENT-DRIVEN: every leaf mutation (update/visibility, lifecycle transition,
//     artifact version activation, delete) calls InvalidateLeaf, which marks the
//     snapshot stale — NOT deleted, so pinned dispatch keeps its metadata and never
//     re-starves (the leaf-package wiring has its own tests; here the cache-side
//     semantics are pinned).
//   - FAIL-SAFE BACKSTOP: eligibleLocked refuses to treat an over-TTL (or
//     invalidated) snapshot as PUBLIC for a requester that did not pin the leaf, and
//     the refresh runs OFF the hot path (TTL-aware warmLeaf at staging; the refiller
//     tick's stale-snapshot sweep), so a genuinely-PUBLIC leaf's any-leaf dispatch
//     resumes within ~one tick rather than degrading. The backstop covers any update
//     path a future change misses and any cross-process staleness (an update landing
//     on another replica's cache).
//
// Differential expectations (RED = `git checkout 0988e62 -- internal/server/dispatch_cache.go`
// with this file in place; this file deliberately touches only seams that exist
// there, so the package compiles and the failures are behavioral):
//
//   - TestDispatchCache_FlippedLeafStaleSnapshot_AnyLeafRequesterRefused — RED
//     fails at the leak assert: the any-leaf requester is handed the UNLISTED units
//     (closeout #2 R1, exactly).
//   - TestDispatchCache_StaleSnapshotStagedUnits_AnyLeafRefusedPinnedServed — RED
//     fails the same way with directly staged units (the pure-gate shape).
//   - TestDispatchCache_InvalidatedLeaf_AnyLeafRefusedPinnedStillServed — RED fails
//     at the PINNED assert: 0988e62's InvalidateLeaf deletes the snapshot outright,
//     which starves the pinned volunteer through rejectLeafNotCached — the reason
//     the fix marks stale instead of deleting.
//   - TestDispatchCache_LeafFlippedBackToPublic_AnyLeafServedByLiveRefiller — RED
//     times out: at 0988e62 a leaf cached hidden and then made PUBLIC stays refused
//     to any-leaf volunteers forever (nothing refreshes a snapshot whose refusal
//     prevents the very hand-outs that would refresh it) — the reverse stickiness
//     this fix must not leave behind.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// ageLeafSnapshot rewrites the cached snapshot of lf so it reads as fetched `age`
// ago — the state of a leaf that has not been dispatched (and so not refreshed) for
// that long. The entry is REPLACED, mirroring the cache's own immutable-entry rule.
func ageLeafSnapshot(c *dispatchCache, lf *leaf.Leaf, age time.Duration) {
	c.leafMu.Lock()
	c.leafCache[lf.ID] = &cachedLeaf{leaf: lf, fetchedAt: c.now().Add(-age)}
	c.leafMu.Unlock()
}

// publicBacklogRepo fakes the work-unit repo of a head whose leaf is PUBLIC in the
// DATABASE: the dispatchable query returns its queued units for the GLOBAL
// (watermark) refill and for any leaf scope — `l.visibility = 'PUBLIC'` satisfies
// the visibility disjunct unconditionally.
func publicBacklogRepo(leafID types.ID, unitIDs []types.ID) *fakeWURepo {
	var mu sync.Mutex
	repo := &fakeWURepo{}
	repo.dispatchFn = func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
		mu.Lock()
		defer mu.Unlock()
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

// TestDispatchCache_FlippedLeafStaleSnapshot_AnyLeafRequesterRefused is closeout
// #2's refuter R1 as a regression test — the full leak sequence through the cache's
// own staging path, well past the snapshot TTL:
//
//	leaf drained while PUBLIC (snapshot warmed PUBLIC) → 95 s of idleness → flipped
//	UNLISTED in the DB → fresh units generated → ONE empty-handed pinned poll queues
//	the (now unconditional, PR #150) leaf-scoped refill, staging the hidden units
//	into the SHARED ready pool → any-leaf volunteers that pinned nothing poll.
//
// They must be handed NOTHING — on 0988e62 each was handed the UNLISTED units,
// because eligibleLocked trusted the 95 s-old PUBLIC snapshot and nothing between
// the flip and the poll ever refreshed it.
func TestDispatchCache_FlippedLeafStaleSnapshot_AnyLeafRequesterRefused(t *testing.T) {
	for _, vis := range []leaf.LeafVisibility{leaf.VisibilityUnlisted, leaf.VisibilityPrivate} {
		t.Run(string(vis), func(t *testing.T) {
			ctx := context.Background()
			leafID := types.NewID()
			units := []types.ID{types.NewID(), types.NewID(), types.NewID(), types.NewID(), types.NewID()}

			// DB truth after the flip: hidden — units come back only for a
			// leaf-scoped refill that names the leaf.
			wuRepo := unlistedOnlyHeadRepo(leafID, units)
			leafRepo := &fakeLeafRepo{}
			c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

			// The snapshot the warm-phase dispatch left behind: PUBLIC, and 95 s old —
			// deliberately far past leafSnapshotTTL, the distance at which the live
			// leak was reproduced (the exposure was NOT bounded by the TTL).
			pub := nativeLeaf(leafID, 1, false, 0)
			pub.Visibility = leaf.VisibilityPublic
			ageLeafSnapshot(c, pub, 95*time.Second)
			// ...and the repo (DB truth) diverges: hidden.
			seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, vis))

			// The watermark refill honours the DB: nothing staged, pool empty.
			c.refillOnce(ctx)
			if got := c.readyLen(); got != 0 {
				t.Fatalf("the global refill staged %d unit(s) for a leaf the DB says is %s; the pool must be empty (PB-38)", got, vis)
			}

			// One empty-handed pinned poll queues the unconditional leaf-scoped
			// refill (PR #150), and the refiller stages the hidden units.
			pinVol := types.NewID()
			pinOpts := capableOpts(pinVol, 0)
			pinOpts.LeafIDs = []types.ID{leafID}
			if got, _ := c.HandOut(pinVol, pinOpts, 2); len(got) != 0 {
				t.Fatalf("first pinned poll cannot serve from an empty pool, got %d unit(s)", len(got))
			}
			c.leafRefillOnce(ctx)
			if c.readyLen() == 0 {
				t.Fatalf("no work was staged for the pinned %s leaf — the PB-16 starvation fix regressed", vis)
			}

			// The leak assert: two any-leaf identities that pinned nothing poll,
			// exactly as the refuter's probes did. Each must get ZERO units.
			for i := 0; i < 2; i++ {
				anyVol := types.NewID()
				if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 2); len(got) != 0 {
					t.Fatalf("any-leaf requester %d was handed %d unit(s) of a %s leaf it never pinned, off a stale PUBLIC snapshot (PB-38b / PB-16 closeout #2 R1)", i+1, len(got), vis)
				}
			}

			// The pinned volunteer is served — the starvation half stays fixed.
			got, _ := c.HandOut(pinVol, pinOpts, len(units))
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

// TestDispatchCache_StaleSnapshotStagedUnits_AnyLeafRefusedPinnedServed is the
// pure-gate statement of the same rule, with the units already staged (no refill
// machinery in the path): hidden units sit in the SHARED ready pool while the
// cached snapshot still reads PUBLIC and is over-TTL. The visibility gate itself —
// not a refresh that happens to have run first — must refuse the un-pinned
// requester, because under DB pressure (or any future path that stages without
// warming) the gate is the only thing standing.
func TestDispatchCache_StaleSnapshotStagedUnits_AnyLeafRefusedPinnedServed(t *testing.T) {
	leafID := types.NewID()
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	// Stale PUBLIC snapshot; DB truth UNLISTED (the build path and any refresh that
	// runs must see the hidden truth).
	pub := nativeLeaf(leafID, 1, false, 0)
	pub.Visibility = leaf.VisibilityPublic
	ageLeafSnapshot(c, pub, 95*time.Second)
	seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, leaf.VisibilityUnlisted))

	c.stageUnit(types.NewID(), leafID, 1, 0)
	c.stageUnit(types.NewID(), leafID, 1, 0)

	// An any-leaf requester must get nothing off the stale-PUBLIC snapshot.
	anyVol := types.NewID()
	if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 2); len(got) != 0 {
		t.Fatalf("any-leaf requester was handed %d staged unit(s) of a hidden leaf whose snapshot is stale-PUBLIC (PB-38b)", len(got))
	}

	// A pinned requester is served regardless — the pin is the opt-in, and the
	// staleness gate must not apply to it.
	pinVol := types.NewID()
	pinOpts := capableOpts(pinVol, 0)
	pinOpts.LeafIDs = []types.ID{leafID}
	if got, _ := c.HandOut(pinVol, pinOpts, 1); len(got) != 1 {
		t.Fatalf("pinned requester = %d unit(s), want 1: the staleness fail-safe must never refuse a pin-by-id (that would re-create PB-16)", len(got))
	}

	// The pinned hand-out's build path refreshed the snapshot (getLeaf saw it
	// over-TTL); the now-FRESH hidden snapshot must refuse the any-leaf requester
	// through the ordinary PB-38 gate.
	if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 1); len(got) != 0 {
		t.Fatalf("any-leaf requester was handed %d unit(s) of a hidden leaf after its snapshot refreshed (PB-38)", len(got))
	}
}

// TestDispatchCache_InvalidatedLeaf_AnyLeafRefusedPinnedStillServed pins the
// event-driven half's cache-side semantics: InvalidateLeaf (now called by every
// leaf-mutation handler) must make the gate stop trusting the snapshot IMMEDIATELY —
// even though it is well inside its TTL — while a pinned volunteer keeps being
// served. The second half is why InvalidateLeaf marks the snapshot stale instead of
// deleting it: at 0988e62 (where this call deletes the entry) the pinned volunteer
// is refused through rejectLeafNotCached with nothing left to re-warm the leaf — a
// fresh starvation in place of the leak.
func TestDispatchCache_InvalidatedLeaf_AnyLeafRefusedPinnedStillServed(t *testing.T) {
	leafID := types.NewID()
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	// Fresh PUBLIC snapshot (well inside the TTL) — without the invalidation event
	// this would be trusted for up to leafSnapshotTTL after the flip.
	pub := nativeLeaf(leafID, 1, false, 0)
	pub.Visibility = leaf.VisibilityPublic
	c.warm(pub, leafRepo)
	// The operator flips the leaf UNLISTED: DB truth changes...
	seedLeafRepoOnly(leafRepo, unlistedLeaf(leafID, leaf.VisibilityUnlisted))
	c.stageUnit(types.NewID(), leafID, 1, 0)
	c.stageUnit(types.NewID(), leafID, 1, 0)
	// ...and the update handler invalidates this replica's snapshot (the wiring
	// added by this fix; here the cache-side effect is under test).
	c.InvalidateLeaf(leafID)

	// Any-leaf requester: refused at once — no waiting out the TTL.
	anyVol := types.NewID()
	if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 2); len(got) != 0 {
		t.Fatalf("any-leaf requester was handed %d unit(s) of a just-flipped leaf after InvalidateLeaf (PB-38b)", len(got))
	}

	// Pinned requester: still served. Invalidation withdraws visibility trust, not
	// the pinned volunteer's dispatch.
	pinVol := types.NewID()
	pinOpts := capableOpts(pinVol, 0)
	pinOpts.LeafIDs = []types.ID{leafID}
	if got, _ := c.HandOut(pinVol, pinOpts, 1); len(got) != 1 {
		t.Fatalf("pinned requester = %d unit(s), want 1: InvalidateLeaf must not starve a pinned volunteer (mark-stale, not delete)", len(got))
	}
}

// TestDispatchCache_LeafFlippedBackToPublic_AnyLeafServedByLiveRefiller pins the
// flip-BACK direction against the live refiller loop: a leaf whose snapshot was
// warmed UNLISTED (and has gone stale) is made PUBLIC in the DB. Any-leaf
// volunteers must promptly be served — hidden-ness must not be sticky. At 0988e62
// this starves them permanently: the watermark refill stages the units (DB says
// PUBLIC) but the visibility gate keeps reading the old UNLISTED snapshot, the
// refusal prevents the hand-outs whose getLeaf would refresh it, warmLeaf refuses
// to touch an existing entry, and nothing else runs. The refiller tick's
// stale-snapshot sweep is what breaks that loop.
func TestDispatchCache_LeafFlippedBackToPublic_AnyLeafServedByLiveRefiller(t *testing.T) {
	leafID := types.NewID()
	units := []types.ID{types.NewID(), types.NewID()}

	wuRepo := publicBacklogRepo(leafID, units)
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	// Snapshot: UNLISTED, stale (the leaf was pin-dispatched a while ago). DB
	// truth: PUBLIC (the operator flipped it back).
	ageLeafSnapshot(c, unlistedLeaf(leafID, leaf.VisibilityUnlisted), 45*time.Second)
	pub := nativeLeaf(leafID, 1, false, 0)
	pub.Visibility = leaf.VisibilityPublic
	seedLeafRepoOnly(leafRepo, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.runRefiller(ctx, 10*time.Millisecond)

	anyVol := types.NewID()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 1); len(got) > 0 {
			return // served — the flip-back propagated
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("an any-leaf volunteer polled a leaf flipped back to PUBLIC for 5s and was never served: the hidden snapshot is sticky (nothing refreshes a snapshot whose refusal prevents the hand-outs that would refresh it)")
}
