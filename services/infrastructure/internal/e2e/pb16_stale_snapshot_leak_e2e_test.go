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

// PB-38b / PB-16 closeout-#2 R1 regression, end to end: after a leaf is flipped
// PUBLIC -> UNLISTED and a PINNED volunteer's empty-handed poll stages its fresh
// units into the SHARED ready pool, an ANY-LEAF volunteer — one that pinned
// nothing — must receive NOTHING from that leaf.
//
// On the pre-fix head this leaked live: the dispatch cache's leaf snapshot was
// warmed PUBLIC while the leaf's earlier work was dispatched, nothing invalidated
// it on the flip (InvalidateLeaf had zero production callers), and the in-memory
// visibility gate trusted it — so the hidden units staged by the pinned refill were
// eligible for everyone. Two any-leaf identities were each handed 2 UNLISTED units,
// and repeating with a 95 s idle gap showed the exposure is NOT bounded by the 30 s
// snapshot TTL (peekLeaf ignored it).
//
// The fix closes this from two sides, and this test exercises both over real gRPC +
// Postgres: the visibility flip (PUT /api/v1/leafs/{id}) now invalidates this
// replica's snapshot through the same late-bound ref production wires, and the
// eligibility gate refuses to treat a stale snapshot as PUBLIC for an un-pinned
// requester. (The well-past-TTL variant needs a controllable clock and lives at the
// cache level: pb16_stale_snapshot_leak_test.go in internal/server.)
//
// RED evidence: this file uses only helpers that exist at 0988e62 (PR #150's merge),
// so copied into a worktree at that commit it compiles and FAILS at the leak assert —
// the any-leaf volunteer is served the UNLISTED units.
func TestPB16_LeafFlippedPublicToUnlisted_AnyLeafVolunteerReceivesNothing(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "pb16-leak")

	// Phase 1 — warm: the leaf is PUBLIC and its one unit is dispatched normally,
	// which caches the leaf's snapshot as PUBLIC. Redundancy 1 and one unit, so the
	// pool is empty again the moment it is taken.
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("PB-16 Leak Leaf"))
	generateLeafWUs(t, env, lf.ID, 1)

	warmKey := genVolunteerKey(t)
	warmVolID := registerHLVolunteer(t, env, ctx, warmKey, "pb16-leak-warm-vol")
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
	if reserved := pollReservedVolunteer(t, ctx, env, warmAsg.WorkUnitId, 10*time.Second); reserved != warmVolID {
		t.Fatalf("setup: flushed RESERVED copy volunteer = %q, want %s", reserved, warmVolID)
	}

	// Phase 2 — flip UNLISTED and generate fresh units. The snapshot warmed in
	// phase 1 said PUBLIC; nothing in phase 2 dispatches the leaf, so only the
	// update path's own invalidation (or the staleness fail-safe) protects what
	// phase 3 stages.
	unlisted := leaf.VisibilityUnlisted
	resp := httpReq(t, "PUT", env.httpURL+"/api/v1/leafs/"+lf.ID.String(),
		leaf.UpdateLeafRequest{Visibility: &unlisted})
	requireStatus(t, resp, http.StatusOK, "flip visibility to UNLISTED")
	generateLeafWUs(t, env, lf.ID, 4)

	// Phase 3 — a pinned volunteer's poll stages the hidden units (the PR #150
	// unconditional leaf-scoped refill) and it is served 2 of the 4, leaving hidden
	// units sitting in the SHARED ready pool — the exact state the R1 refuter
	// attacked.
	pinKey := genVolunteerKey(t)
	pinVolID := registerHLVolunteer(t, env, ctx, pinKey, "pb16-leak-pin-vol")
	var pinAsg *lettucev1.WorkUnitAssignment
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pinKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    pinVolID,
			PublicKey:      pinKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 2,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (pinned): %v", err)
		}
		if len(resp.Assignments) > 0 {
			pinAsg = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pinAsg == nil {
		t.Fatal("a volunteer pinning the flipped leaf was never served — the PB-16 starvation fix regressed (the invalidation on the flip must not suppress pinned staging)")
	}
	if pinAsg.LeafId != lf.ID.String() {
		t.Fatalf("pinned volunteer served leaf %s, want %s", pinAsg.LeafId, lf.ID)
	}

	// The leak assert: two any-leaf identities poll, repeatedly, with hidden units
	// verifiably staged (the pinned volunteer took 2 of 4). Each must get ZERO.
	// Pre-fix, each was handed units of the UNLISTED leaf off the stale PUBLIC
	// snapshot — this is the exact live sequence of closeout #2's refuter R1.
	for i := 0; i < 2; i++ {
		anyKey := genVolunteerKey(t)
		anyVolID := registerHLVolunteer(t, env, ctx, anyKey, "pb16-leak-any-vol")
		for poll := 0; poll < 5; poll++ {
			reqResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, anyKey), &lettucev1.RequestWorkUnitRequest{
				VolunteerId:    anyVolID,
				PublicKey:      anyKey,
				MaxAssignments: 2,
			})
			if err != nil {
				t.Fatalf("RequestWorkUnit (any-leaf %d): %v", i+1, err)
			}
			if len(reqResp.Assignments) != 0 {
				t.Fatalf("any-leaf volunteer %d was handed %d unit(s) of leaf %s it never pinned — UNLISTED units leaked out of the shared ready pool off a stale PUBLIC snapshot (PB-38b / PB-16 closeout #2 R1)",
					i+1, len(reqResp.Assignments), reqResp.Assignments[0].LeafId)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}

	// And the pinned volunteer keeps draining the leaf — hidden-ness applies to the
	// un-pinned, never to the pin.
	reqResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pinKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    pinVolID,
		PublicKey:      pinKey,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 2,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit (pinned, drain): %v", err)
	}
	if len(reqResp.Assignments) == 0 {
		t.Fatal("pinned volunteer got nothing on its follow-up poll while its leaf's units sit staged (PB-16)")
	}
}
