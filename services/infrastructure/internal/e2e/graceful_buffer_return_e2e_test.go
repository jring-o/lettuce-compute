//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestDispatchCache_GracefulBufferReturn_ReReservable verifies ROADMAP #59 end-to-end
// against the REAL Layer-2 dispatch cache + real Postgres + real gRPC.
//
// Scenario (QuaXeros's "spoiled peaches" pool): a single volunteer on a redundancy-2 leaf
// reserves a unit into its prefetch buffer, then GRACEFULLY hands it back UN-STARTED — the
// restart / shutdown path, AbandonWorkUnit reason "volunteer shutdown". The head closes
// that copy ABANDONED with started_at NULL.
//
// BEFORE #59 the post-failure cooldown — meant to give a *different* volunteer first crack
// after a genuine TIMEOUT — benched this volunteer on the unit for ~one deadline even though
// it never ran the work. With no other volunteer in the pool, the unit's only candidate was
// refused its re-reservation (FlushReservations cooldown) and StartWork was denied, so the
// work sat idle until the cooldown elapsed (the dispatch-cache hygiene fix, covered by the
// unit test TestFlush_NonLanded_BenchesVolunteer_BreaksRehandoutLivelock, stopped the
// resulting re-handout livelock but could not un-strand the work).
//
// AFTER #59 a never-started ABANDONED copy is not a reliability signal and does not feed the
// cooldown, so the volunteer can re-reserve and RUN the very unit it returned. The unit is
// generated FIRST (earliest created_at) so it sits at the front of the dispatch order and is
// the one the volunteer is re-offered.
func TestDispatchCache_GracefulBufferReturn_ReReservable(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "graceful-rereserve")
	opts := hlDefaultLeafOpts("Graceful Buffer-Return Leaf")
	// lbry's container-leaf shape: plain redundancy 2 (target == quorum == 2 by the back-compat alias).
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 6) // unit #1 (earliest) is the one we abandon & re-acquire

	pub := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pub, "graceful-rereserve-A")

	reqOne := func() string {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pub), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pub, LeafIds: []string{lf.ID.String()}, MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit: %v", err)
		}
		if len(resp.Assignments) == 1 {
			return resp.Assignments[0].WorkUnitId
		}
		return ""
	}

	myLiveCopies := func(wuID string) int {
		var n int
		if err := env.pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2 AND outcome IS NULL",
			wuID, volID).Scan(&n); err != nil {
			t.Fatalf("count live copies: %v", err)
		}
		return n
	}

	// 1) A reserves the front (earliest) unit U into its buffer.
	var returned string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if returned = reqOne(); returned != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if returned == "" {
		t.Fatal("A never got a unit from the cache")
	}
	// Wait for A's RESERVED copy to flush so the graceful abandon has a live copy to close.
	for deadline := time.Now().Add(10 * time.Second); myLiveCopies(returned) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's reserved copy never landed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 2) A GRACEFULLY abandons U un-started (restart / buffer-return). Pre-#59 this benched A
	//    on U; post-#59 it does not (started_at NULL).
	if _, err := env.grpc.AbandonWorkUnit(signFor(t, ctx, pub), &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId: returned, VolunteerId: volID, PublicKey: pub, Reason: "volunteer shutdown",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); myLiveCopies(returned) != 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's abandoned copy never closed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3) #59: A is NOT benched on U. When the cache re-offers it, A's re-reservation LANDS
	//    (the cooldown no longer refuses it) and StartWork succeeds — A runs the unit it
	//    returned, instead of being stranded for a full deadline.
	ranReturned := false
	for i := 0; i < 20 && !ranReturned; i++ {
		got := reqOne()
		if got == "" {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		// Let the async flush process this hand-out before we read the DB / run-start.
		time.Sleep(250 * time.Millisecond)
		if got != returned {
			continue // a fresh unit; keep going until A is re-offered the returned one
		}
		// The core #59 signal: A's re-reservation of the gracefully-returned unit must LAND
		// (pre-fix the cooldown refused it forever, so this stayed 0).
		landed := false
		for d := time.Now().Add(5 * time.Second); time.Now().Before(d); {
			if myLiveCopies(returned) == 1 {
				landed = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !landed {
			t.Fatal("A's re-reservation of the gracefully-returned unit never landed — A is still benched (#59 not effective)")
		}
		sw, err := env.grpc.StartWork(signFor(t, ctx, pub), &lettucev1.StartWorkRequest{
			WorkUnitId: returned, VolunteerId: volID,
		})
		if err != nil {
			t.Fatalf("StartWork(returned): %v", err)
		}
		if !sw.GetOk() {
			t.Fatal("StartWork on the gracefully-returned unit was DENIED (Ok=false) — A is still benched; #59 not effective")
		}
		ranReturned = true
	}

	if !ranReturned {
		t.Fatal("A was never re-offered + able to run the unit it gracefully returned (still benched / stranded?)")
	}
	// A holds exactly its own live (now RUNNING) copy of the re-acquired unit.
	if n := myLiveCopies(returned); n != 1 {
		t.Fatalf("expected A to hold exactly 1 live copy of the re-acquired unit, got %d", n)
	}
}
