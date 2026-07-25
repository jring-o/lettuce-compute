//go:build integration

package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// PB-16 regression, end to end: a volunteer that pins an UNLISTED leaf by id must
// receive its work on a head that has NO public backlog.
//
// This is the filed shape, and it is the shape every earlier test missed. The
// selection SQL was already correct (internal/workunit/visibility_dispatch_test.go)
// and so was the in-memory hand-out (TestHandOut_NonPublicLeafOnlyToPinnedRequester),
// but gRPC RequestWorkUnit serves ONLY from the in-process dispatch cache, and nothing
// staged the pinned leaf's units into it: the global watermark refill stages PUBLIC
// leafs only (the PB-38 visibility gate), and the on-demand leaf-scoped refill that
// would stage a pinned non-PUBLIC leaf was gated on the ready pool being non-empty. On
// a head whose only ACTIVE leaf is UNLISTED the pool is permanently empty, so the units
// sat QUEUED forever. Any co-resident PUBLIC leaf with queued work fills the pool and
// hides the defect completely — so this test deliberately creates NO public leaf.
func TestPB16_UnlistedOnlyHead_PinnedVolunteerReceivesWork(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "pb16-unlisted")
	opts := hlDefaultLeafOpts("PB-16 Unlisted Only Leaf")
	opts.Visibility = leaf.VisibilityUnlisted
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 4)

	// The head's catalog does not advertise the leaf — the volunteer can only reach it
	// by pinning the id it was given out of band (`lettuce-volunteer attach <leaf-id>`).
	for _, advertised := range getHeadInfo(t, env).Leafs {
		if advertised.ID == lf.ID.String() {
			t.Fatalf("an UNLISTED leaf must not appear in the head catalog (PB-38)")
		}
	}

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "pb16-vol")

	// Control (PB-38 must keep holding): the any-leaf fallback — a request with NO leaf
	// filter, which is what an un-pinned volunteer sends — is never served this leaf, no
	// matter how long it polls or what the pinned path stages into the shared pool.
	anyKey := genVolunteerKey(t)
	anyVolID := registerHLVolunteer(t, env, ctx, anyKey, "pb16-anyleaf-vol")
	for i := 0; i < 5; i++ {
		requestWUExpectNone(t, env, ctx, anyVolID, anyKey, nil)
		time.Sleep(100 * time.Millisecond)
	}

	// The filed capability: the pinned volunteer polls, and the head must converge on
	// serving it. Pre-fix this loop expires having received nothing — the live repro
	// polled for ~9 minutes across two daemon restarts.
	var asg *lettucev1.WorkUnitAssignment
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 2,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (pinned): %v", err)
		}
		if len(resp.Assignments) > 0 {
			asg = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if asg == nil {
		t.Fatal("a volunteer pinning the head's ONLY leaf (UNLISTED, with queued work) was never served: nothing stages a pinned non-PUBLIC leaf while the ready pool is empty (PB-16)")
	}
	if asg.LeafId != lf.ID.String() {
		t.Fatalf("served leaf %s, want the pinned UNLISTED leaf %s", asg.LeafId, lf.ID)
	}

	// The reservation lands like any other: the async flush writes the RESERVED copy
	// while the unit stays QUEUED (the per-copy dispatch model).
	if reserved := pollReservedVolunteer(t, ctx, env, asg.WorkUnitId, 10*time.Second); reserved != volID {
		t.Fatalf("flushed RESERVED copy volunteer = %q, want %s", reserved, volID)
	}

	// And the any-leaf requester is STILL refused now that the pinned refill has staged
	// the leaf's units into the SHARED ready pool — the hole PB-38 closed stays closed.
	requestWUExpectNone(t, env, ctx, anyVolID, anyKey, nil)
}

// PB-16 regression, end to end, "flipped after warm": a leaf that was PUBLIC while its
// work was being dispatched, then set UNLISTED and given fresh units, must still serve a
// volunteer that pins it by id.
//
// This is the shape that broke the first re-fix. That fix skipped the on-demand
// leaf-scoped refill whenever every pinned leaf was warmed in the dispatch cache AND
// cached as PUBLIC — reasoning that the global watermark refill would stage it anyway.
// But nothing in the head ever invalidates that cached snapshot (InvalidateLeaf has no
// production caller, warmLeaf will not refresh an existing entry, and getLeaf's TTL is
// consulted only while building an ACCEPTED hand-out), so after the flip the cache keeps
// answering PUBLIC while the DB says UNLISTED: the global refill stages nothing (it reads
// the DB) and the leaf-scoped refill is skipped (it read the snapshot). On a live head
// that was 50 polls over 147 s with zero hand-outs, curable only by a restart.
//
// The test walks that exact sequence, and deliberately proves the ready pool is EMPTY
// before the pinned volunteer polls — a co-resident backlog masks this defect completely.
func TestPB16_LeafFlippedPublicToUnlistedAfterWarm_PinnedVolunteerReceivesWork(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "pb16-flip")

	// Phase 1 — the leaf is PUBLIC and its work is dispatched normally. This is what
	// warms the dispatch cache's leaf snapshot as PUBLIC. One unit, redundancy 1, and one
	// volunteer, so the ready pool is empty again the moment it is taken.
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("PB-16 Flipped Leaf"))
	generateLeafWUs(t, env, lf.ID, 1)

	warmKey := genVolunteerKey(t)
	warmVolID := registerHLVolunteer(t, env, ctx, warmKey, "pb16-flip-warm-vol")
	var warmAsg *lettucev1.WorkUnitAssignment
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, warmKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    warmVolID,
			PublicKey:      warmKey,
			MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (warm phase): %v", err)
		}
		if len(resp.Assignments) > 0 {
			warmAsg = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if warmAsg == nil {
		t.Fatal("setup: the PUBLIC leaf's unit was never dispatched, so the cache was never warmed with a PUBLIC snapshot")
	}
	// Wait for the async flush to write the RESERVED copy, so the unit is no longer
	// dispatchable and the refiller will not put it back in the pool.
	if reserved := pollReservedVolunteer(t, ctx, env, warmAsg.WorkUnitId, 10*time.Second); reserved != warmVolID {
		t.Fatalf("setup: flushed RESERVED copy volunteer = %q, want %s", reserved, warmVolID)
	}

	// Phase 2 — the ordinary operator flow: publish, then unlist. The head's leaf-update
	// path does not touch the dispatch cache, so its snapshot still reads PUBLIC.
	unlisted := leaf.VisibilityUnlisted
	resp := httpReq(t, "PUT", env.httpURL+"/api/v1/leafs/"+lf.ID.String(),
		leaf.UpdateLeafRequest{Visibility: &unlisted})
	requireStatus(t, resp, http.StatusOK, "flip visibility to UNLISTED")
	var flipped leaf.Leaf
	decodeJSON(t, resp, &flipped)
	if flipped.Visibility != leaf.VisibilityUnlisted {
		t.Fatalf("leaf visibility = %q after the flip, want UNLISTED", flipped.Visibility)
	}
	generateLeafWUs(t, env, lf.ID, 4)

	// The catalog stops advertising it, as UNLISTED requires.
	for _, advertised := range getHeadInfo(t, env).Leafs {
		if advertised.ID == lf.ID.String() {
			t.Fatalf("an UNLISTED leaf must not appear in the head catalog (PB-38)")
		}
	}

	// The ready pool must be EMPTY before the pinned poll, or this test proves nothing:
	// a non-empty pool opens the refill gate on its own and masks the defect. An any-leaf
	// probe is the sharp instrument here — the cache's stale snapshot still says PUBLIC,
	// so it WOULD be handed these units if any were staged (the accepted PB-38b window).
	// Getting nothing therefore means nothing is staged: the global watermark refill,
	// which reads the DB rather than the snapshot, correctly stages no hidden leaf.
	probeKey := genVolunteerKey(t)
	probeVolID := registerHLVolunteer(t, env, ctx, probeKey, "pb16-flip-probe-vol")
	for i := 0; i < 5; i++ {
		requestWUExpectNone(t, env, ctx, probeVolID, probeKey, nil)
		time.Sleep(200 * time.Millisecond)
	}

	// Phase 3 — the filed capability, on the flipped leaf: a volunteer pins it by id and
	// must be served. Against the coverage gate this loop expires having received nothing,
	// because the stale PUBLIC snapshot suppresses the only refill that can stage it.
	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "pb16-flip-pin-vol")
	var asg *lettucev1.WorkUnitAssignment
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 2,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (pinned): %v", err)
		}
		if len(resp.Assignments) > 0 {
			asg = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if asg == nil {
		t.Fatal("a volunteer pinning a leaf that was flipped PUBLIC->UNLISTED after the cache warmed it was never served: the cached PUBLIC snapshot suppresses the leaf-scoped refill, and nothing else stages a hidden leaf, so the starvation lasts until the head restarts (PB-16)")
	}
	if asg.LeafId != lf.ID.String() {
		t.Fatalf("served leaf %s, want the pinned flipped leaf %s", asg.LeafId, lf.ID)
	}
	if reserved := pollReservedVolunteer(t, ctx, env, asg.WorkUnitId, 10*time.Second); reserved != volID {
		t.Fatalf("flushed RESERVED copy volunteer = %q, want %s", reserved, volID)
	}
}
