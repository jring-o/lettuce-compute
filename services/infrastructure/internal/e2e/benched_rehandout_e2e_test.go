//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestDispatchCache_BenchedRehandout_Livelock reproduces the "spoiled peaches" production churn
// against the REAL Layer-2 dispatch cache + real Postgres + real gRPC.
//
// Scenario (exactly QuaXeros's): a single volunteer on a redundancy-2 leaf reserves a unit, then
// GRACEFULLY abandons it (the restart / buffer-return path, reason "volunteer shutdown"). The head
// closes that copy ABANDONED, which BENCHES the volunteer on that unit for ~one deadline (the
// post-failure cooldown that is meant to give a *different* volunteer first crack). With no other
// volunteer in the pool, the unit still needs a copy — but the dispatch cache keeps RE-OFFERING it
// to the benched volunteer (its in-memory `benched` snapshot was taken when the unit was staged,
// BEFORE the abandon, and never refreshes). The head then refuses the copy (FlushReservations
// cooldown), so the volunteer is handed a unit it can NEVER run (StartWork denied) over and over,
// while fresh units sit untouched.
//
// The unit is generated FIRST (earliest created_at) so it sits at the front of the dispatch order
// (priority DESC, created_at ASC) and is the one the volunteer keeps being re-handed; the rest are
// fresh work the volunteer should be getting instead.
func TestDispatchCache_BenchedRehandout_Livelock(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "bench-livelock")
	opts := hlDefaultLeafOpts("Benched Re-handout Leaf")
	// lbry's container-leaf shape: plain redundancy 2 (target == quorum == 2 by the back-compat alias).
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 6) // unit #1 (earliest) becomes the stuck one; 5 fresh remain

	pub := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pub, "bench-livelock-A")

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

	// 1) A reserves the front (earliest) unit U.
	var stuck string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if stuck = reqOne(); stuck != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if stuck == "" {
		t.Fatal("A never got a unit from the cache")
	}
	// Wait for A's RESERVED copy to flush so the graceful abandon has a live copy to close.
	for deadline := time.Now().Add(10 * time.Second); myLiveCopies(stuck) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's reserved copy never landed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 2) A GRACEFULLY abandons U (restart / buffer-return). This benches A on U for ~one deadline.
	if _, err := env.grpc.AbandonWorkUnit(signFor(t, ctx, pub), &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId: stuck, VolunteerId: volID, PublicKey: pub, Reason: "volunteer shutdown",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); myLiveCopies(stuck) != 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's abandoned copy never closed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3) A keeps requesting. Count how often the cache RE-HANDS the un-reservable benched unit
	//    (and StartWork is denied) vs serves A a different, runnable unit.
	rehandedStuck, servedOther := 0, 0
	for i := 0; i < 10; i++ {
		got := reqOne()
		if got == "" {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		// Let the async flush process: land the copy (fresh unit) or refuse it & release the
		// in-memory hold (benched unit), so StartWork sees the authoritative DB state.
		time.Sleep(250 * time.Millisecond)
		if got == stuck {
			sw, err := env.grpc.StartWork(signFor(t, ctx, pub), &lettucev1.StartWorkRequest{
				WorkUnitId: stuck, VolunteerId: volID,
			})
			if err != nil {
				t.Fatalf("StartWork(stuck): %v", err)
			}
			if sw.GetOk() {
				t.Fatal("StartWork on the benched unit returned Ok=true — A is NOT benched (test premise broken)")
			}
			rehandedStuck++
		} else {
			servedOther++
		}
	}

	t.Logf("after abandon, over 10 requests: re-handed the un-reservable unit %d times (StartWork denied), served a runnable unit %d times", rehandedStuck, servedOther)

	// A must NEVER have landed a copy of the benched unit — every flush refused it (cooldown).
	if myLiveCopies(stuck) != 0 {
		t.Fatalf("A landed a live copy of the benched unit (cooldown not enforced?) — test premise broken")
	}

	// FIXED behavior: the cache stops re-offering an un-reservable unit (it benches A on the staged
	// candidate after the first refused flush), so A is re-handed it at most once and then reaches
	// the fresh work. PRE-FIX this is a tight livelock: re-handed every cycle, fresh work starved.
	if rehandedStuck > 1 {
		t.Fatalf("LIVELOCK reproduced: cache re-handed the benched/un-reservable unit %d times (want <=1); fresh work served only %d times",
			rehandedStuck, servedOther)
	}
	if servedOther == 0 {
		t.Fatalf("starvation: A never reached a runnable unit (re-handed stuck=%d, served other=0)", rehandedStuck)
	}
}
