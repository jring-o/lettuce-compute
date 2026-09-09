//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestDispatchCache_UnstartedAbandon_BenchesAndBillsOnce verifies TB-81 end to end
// against the REAL Layer-2 dispatch cache + real Postgres + real gRPC.
//
// Scenario (the infra.scios.tech operator's report of 2026-09-07): a volunteer whose
// container engine is dead reserves a unit into its prefetch buffer, fails to prepare
// it, and abandons it UN-STARTED with the client's engine-down reason and no give-back
// flag. The head closes the copy ABANDONED with started_at NULL.
//
// BEFORE TB-81 that copy fed neither the post-failure cooldown nor the reliability
// signal (the #59 exemption meant for graceful buffer returns — which are RETURNED
// give-backs now, TB-35) but DID spend one of the unit's budget copies, so the same
// machine was re-offered the same unit on its next poll, ten minutes later, eight
// times, until the budget was gone and the unit dead-lettered.
//
// AFTER TB-81 the abandoner is benched from that unit for ~one deadline: its next
// polls are served the leaf's OTHER units and never the one it could not start, the
// unit's billed count is 1 however often it repeats, and a fresh volunteer is offered
// it at once. This file replaces the #59 e2e test whose assertion was the defect.
func TestDispatchCache_UnstartedAbandon_BenchesAndBillsOnce(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "tb81-unstarted-abandon")
	opts := hlDefaultLeafOpts("TB-81 Un-started Abandon Leaf")
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	const units = 4
	generateLeafWUs(t, env, lf.ID, units) // unit #1 (earliest) is the one A fails to start

	pubA := genVolunteerKey(t)
	volA := registerHLVolunteer(t, env, ctx, pubA, "tb81-dead-engine-A")
	pubB := genVolunteerKey(t)
	volB := registerHLVolunteer(t, env, ctx, pubB, "tb81-fresh-B")

	reqOne := func(pub []byte, volID string) string {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pub), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pub, LeafIds: []string{lf.ID.String()}, MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit(%s): %v", volID, err)
		}
		if len(resp.Assignments) == 1 {
			return resp.Assignments[0].WorkUnitId
		}
		return ""
	}

	liveCopies := func(wuID, volID string) int {
		var n int
		if err := env.pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2 AND outcome IS NULL",
			wuID, volID).Scan(&n); err != nil {
			t.Fatalf("count live copies: %v", err)
		}
		return n
	}

	// 1) A reserves the front (earliest) unit U into its buffer.
	var failedUnit string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if failedUnit = reqOne(pubA, volA); failedUnit != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if failedUnit == "" {
		t.Fatal("A never got a unit from the cache")
	}
	for deadline := time.Now().Add(10 * time.Second); liveCopies(failedUnit, volA) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's reserved copy never landed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 2) A cannot prepare U (its container engine is dead) and abandons it un-started —
	//    a plain abandon, NOT a give-back.
	if _, err := env.grpc.AbandonWorkUnit(signFor(t, ctx, pubA), &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId: failedUnit, VolunteerId: volA, PublicKey: pubA,
		Reason: "docker is not available: docker ping: Cannot connect to the Docker daemon at unix:///run/user/1000/podman/podman.sock",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); liveCopies(failedUnit, volA) != 0; {
		if time.Now().After(deadline) {
			t.Fatal("A's abandoned copy never closed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var outcome string
	var started *time.Time
	if err := env.pool.QueryRow(ctx,
		"SELECT outcome::text, started_at FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2",
		failedUnit, volA).Scan(&outcome, &started); err != nil {
		t.Fatalf("read A's closed copy: %v", err)
	}
	if outcome != "ABANDONED" || started != nil {
		t.Fatalf("A's copy closed %s (started_at %v), want ABANDONED with started_at NULL", outcome, started)
	}

	// 3) TB-81: A is benched from U. Its next polls — the client retries ten minutes
	//    after a prepare failure; here at once — are served the leaf's other units and
	//    never U. Pre-fix U, at the front of the dispatch order and unbenched, came
	//    straight back.
	handed := map[string]bool{}
	for i := 0; i < 3*units; i++ {
		got := reqOne(pubA, volA)
		if got == "" {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if got == failedUnit {
			t.Fatal("A was re-offered the unit it could not start: an un-started ABANDONED must bench the volunteer on the unit (TB-81)")
		}
		handed[got] = true
		if len(handed) == units-1 {
			break
		}
	}
	if len(handed) != units-1 {
		t.Fatalf("A was handed %d other units, want %d: the bench must be per-unit, not a refusal of the whole leaf", len(handed), units-1)
	}
	if n := liveCopies(failedUnit, volA); n != 0 {
		t.Fatalf("A holds %d live copies of the unit it could not start, want 0", n)
	}

	// 4) The abandon spent exactly one budget copy — and would spend no more however
	//    often A repeated it (the workunit integration tests pin the repeat).
	var billed int
	if err := env.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_unit_assignment_history
		WHERE work_unit_id = $1 AND outcome IS DISTINCT FROM 'RETURNED'`, failedUnit).Scan(&billed); err != nil {
		t.Fatalf("count billed rows: %v", err)
	}
	if billed != 1 {
		t.Fatalf("billed history rows on the failed unit = %d, want 1", billed)
	}

	// 5) A fresh volunteer is offered U at once (it is the earliest unit and A's copy is
	//    closed), so A's failure costs the unit nothing but one budget copy.
	gotU := false
	for i := 0; i < 3*units && !gotU; i++ {
		got := reqOne(pubB, volB)
		if got == "" {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		gotU = got == failedUnit
	}
	if !gotU {
		t.Fatal("fresh volunteer B was never offered the unit A could not start: the bench must apply to the abandoner only")
	}
}
