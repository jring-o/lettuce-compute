//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestDispatchCache_TargetQuorum_OverDispatchValidateAtQuorumSupersede drives the
// Phase 2 target>quorum feature (TODO #50) through the LAYER-2 in-process dispatch
// cache — the path PRODUCTION serves work from (requestWorkUnitFromCache ->
// dispatchCache.HandOut -> eligibleLocked), whose over-dispatch headroom uses the
// candidate's effectiveRedundancy == the SQL effective_redundancy column ==
// effTargetSQL == target_copies.
//
// The existing full-gRPC target>quorum e2e (TestE2E_TargetQuorum_ValidateAtQuorum-
// AndSupersede, internal package) uses setupF05Server, which does NOT start the cache,
// so it only exercises the Layer-1 DIRECT reserve path (FindNextAssignable /
// ReserveNextAssignable). This test closes that gap by proving the same semantics over
// the cache hand-out path:
//   - THREE distinct copies of one target=3/quorum=2 unit are handed out from the cache
//     (over-dispatch to target), all live concurrently while the unit stays QUEUED;
//   - the unit VALIDATES as soon as TWO agree (validate-at-quorum), without waiting for
//     the third copy;
//   - the third still-running, never-submitted copy is closed SUPERSEDED (not
//     EXPIRED/ABANDONED), so its host is not charged a bad reliability outcome.
func TestDispatchCache_TargetQuorum_OverDispatchValidateAtQuorumSupersede(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "cache-tq")
	opts := hlDefaultLeafOpts("Cache Target>Quorum Leaf")
	// target_copies=3 over-dispatches; min_quorum=2 validates at the 2nd agreeing result.
	// redundancy_factor is retained as the back-compat alias; target/quorum override it.
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:   2,
		TargetCopies:       3,
		MinQuorum:          2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 1)

	// Three distinct volunteers (distinctness keys on the account/Ed25519 key).
	type vol struct {
		pub []byte
		id  string
	}
	vols := make([]vol, 3)
	for i, name := range []string{"cache-tq-A", "cache-tq-B", "cache-tq-C"} {
		pub := genVolunteerKey(t)
		vols[i] = vol{pub: pub, id: registerHLVolunteer(t, env, ctx, pub, name)}
	}

	// reserveOne requests one copy from the cache for the given volunteer, retrying so the
	// refiller has time to (re-)stage the unit with fresh over-dispatch headroom after the
	// prior holder's in-memory reservation flushes its RESERVED copy row.
	reserveOne := func(v vol) string {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, v.pub), &lettucev1.RequestWorkUnitRequest{
				VolunteerId: v.id, PublicKey: v.pub, LeafIds: []string{lf.ID.String()}, MaxAssignments: 1,
			})
			if err != nil {
				t.Fatalf("RequestWorkUnit(%s): %v", v.id, err)
			}
			if len(resp.Assignments) == 1 {
				return resp.Assignments[0].WorkUnitId
			}
			time.Sleep(150 * time.Millisecond)
		}
		return ""
	}

	// Over-dispatch to target=3: each distinct volunteer is served a copy of the SAME unit
	// from the cache.
	wuID := reserveOne(vols[0])
	if wuID == "" {
		t.Fatal("vol A never got the target>quorum unit from the cache (refiller did not stage it)")
	}
	for i := 1; i < 3; i++ {
		got := reserveOne(vols[i])
		if got == "" {
			t.Fatalf("vol %d was not served a parallel copy (over-dispatch to target=3 via the cache broken)", i)
		}
		if got != wuID {
			t.Fatalf("vol %d got a different unit %s, want the same unit %s over-dispatched to target", i, got, wuID)
		}
	}

	// All three reservations flush -> three distinct live copies, unit still QUEUED.
	var live int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := env.pool.QueryRow(ctx,
			"SELECT COUNT(DISTINCT volunteer_id) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
			wuID).Scan(&live); err != nil {
			t.Fatalf("count live copies: %v", err)
		}
		if live == 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if live != 3 {
		t.Fatalf("expected 3 distinct live copies (over-dispatch to target=3 via the cache), got %d", live)
	}
	if st := pollWorkUnitState(t, ctx, env, wuID, "QUEUED", time.Second); st != "QUEUED" {
		t.Fatalf("unit must stay QUEUED while its three copies run, got %q", st)
	}

	// All three run-start their copy (RESERVED -> RUNNING).
	for _, v := range vols {
		if _, err := env.grpc.StartWork(signFor(t, ctx, v.pub), &lettucev1.StartWorkRequest{
			WorkUnitId: wuID, VolunteerId: v.id,
		}); err != nil {
			t.Fatalf("StartWork(%s): %v", v.id, err)
		}
	}

	// Identical output so all agree under EXACT.
	output := []byte(`{"result":"ok","value":42}`)
	submit := func(v vol) {
		if _, err := env.grpc.SubmitResult(signFor(t, ctx, v.pub), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuID, VolunteerId: v.id, PublicKey: v.pub,
			OutputData: output, OutputChecksumSha256: sha256Hex(output),
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
		}); err != nil {
			t.Fatalf("SubmitResult(%s): %v", v.id, err)
		}
	}

	// First agreeing result: quorum (2) not yet met -> the unit is not validated.
	submit(vols[0])
	var afterFirst string
	if err := env.pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&afterFirst); err != nil {
		t.Fatalf("query state after first submit: %v", err)
	}
	if afterFirst == "VALIDATED" {
		t.Fatal("validated after a single result; min_quorum=2 requires two agreeing results")
	}

	// Second agreeing result: quorum reached and the two agree -> VALIDATE AT QUORUM,
	// without waiting for the third copy.
	submit(vols[1])
	if st := pollWorkUnitState(t, ctx, env, wuID, "VALIDATED", 10*time.Second); st != "VALIDATED" {
		t.Fatalf("state after 2 agreeing results = %q, want VALIDATED (validate-at-quorum via the cache path)", st)
	}

	// The third (still-running, never-submitted) copy is closed SUPERSEDED — not
	// EXPIRED/ABANDONED — so vol C is not charged a bad reliability outcome for the
	// over-dispatch extra.
	var superseded int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome = 'SUPERSEDED'",
		wuID).Scan(&superseded); err != nil {
		t.Fatalf("query superseded: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("expected exactly 1 SUPERSEDED copy (the over-dispatch extra), got %d", superseded)
	}

	// No live copies remain and exactly two results were credited (the quorum).
	var liveAfter int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
		wuID).Scan(&liveAfter); err != nil {
		t.Fatalf("count live after: %v", err)
	}
	if liveAfter != 0 {
		t.Fatalf("expected 0 live copies after validate+supersede, got %d", liveAfter)
	}
	var agreed int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'AGREED'",
		wuID).Scan(&agreed); err != nil {
		t.Fatalf("query agreed: %v", err)
	}
	if agreed != 2 {
		t.Fatalf("expected 2 AGREED results (quorum), got %d", agreed)
	}
}
