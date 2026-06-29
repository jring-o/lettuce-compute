//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestBrowserSubmit_TargetQuorum_ValidateAtQuorumAndSupersede proves the browser/WASM REST
// submit path (handleBrowserSubmitResult, POST /api/v1/volunteers/submit-result) routes its
// redundancy decision through the SINGLE transitioner (TODO #50/#66) — exactly like the gRPC
// SubmitResult path — instead of writing work_units.state='COMPLETED' via raw SQL using the
// old conflated redundancy_factor and calling the legacy validationEngine.TryValidate.
//
// It is the browser analogue of TestDispatchCache_TargetQuorum_OverDispatchValidateAtQuorum-
// Supersede (the gRPC/desktop path). Setup: one work unit on a target_copies=3 / min_quorum=2
// leaf, over-dispatched to THREE distinct live copies (A, B, C). Two browser volunteers (A, B)
// submit agreeing results via REST; the third copy (C) is never submitted.
//
//   - The unit VALIDATES as soon as the two agree (validate-at-quorum), without waiting for the
//     third copy.
//   - The third still-live, never-submitted copy (C) is closed SUPERSEDED (not left live to be
//     reaped EXPIRED/ABANDONED, which would charge C's host a bad reliability outcome).
//
// Before #66 the browser submit path NEVER superseded the over-dispatch extra (it had no path
// to ExpireLiveCopies at all), so C's copy lingered live — the gap this test locks closed.
func TestBrowserSubmit_TargetQuorum_ValidateAtQuorumAndSupersede(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "browser-tq")
	opts := hlDefaultLeafOpts("Browser Target>Quorum Leaf")
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

	var wuID string
	if err := env.pool.QueryRow(ctx,
		"SELECT id FROM work_units WHERE leaf_id = $1 LIMIT 1", lf.ID).Scan(&wuID); err != nil {
		t.Fatalf("load generated work unit: %v", err)
	}

	// Two browser volunteers (A, B) that submit via REST, plus a third volunteer (C) whose
	// over-dispatch copy is never submitted.
	bcA := newBrowserClient(env.httpURL)
	volA := bcA.register(t, []string{"WASM"}, false, nil)
	bcB := newBrowserClient(env.httpURL)
	volB := bcB.register(t, []string{"WASM"}, false, nil)
	cPub := genVolunteerKey(t)
	volC := registerHLVolunteer(t, env, ctx, cPub, "Browser TQ Vol C")

	// Over-dispatch to target=3: one live copy per distinct volunteer; the unit stays QUEUED.
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volA))
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volB))
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volC))

	// Identical output so the two agree under EXACT.
	output := []byte(`{"result":"ok","value":42}`)

	// First browser submit: quorum (2) not yet met -> the unit is not validated.
	bcA.submitResult(t, wuID, output)
	var afterFirst string
	if err := env.pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&afterFirst); err != nil {
		t.Fatalf("query state after first submit: %v", err)
	}
	if afterFirst == "VALIDATED" {
		t.Fatal("validated after a single browser result; min_quorum=2 requires two agreeing results")
	}

	// Second browser submit: quorum reached and the two agree -> VALIDATE AT QUORUM via the
	// transitioner, without waiting for the third copy.
	bcB.submitResult(t, wuID, output)
	if st := pollWorkUnitState(t, ctx, env, wuID, "VALIDATED", 10*time.Second); st != "VALIDATED" {
		t.Fatalf("state after 2 agreeing browser results = %q, want VALIDATED (validate-at-quorum on the browser submit path)", st)
	}

	// The third (still-live, never-submitted) over-dispatch copy is closed SUPERSEDED — not
	// left live. Before #66 the browser submit path never superseded, so this was 0.
	var superseded int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome = 'SUPERSEDED'",
		wuID).Scan(&superseded); err != nil {
		t.Fatalf("query superseded: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("expected exactly 1 SUPERSEDED copy (the over-dispatch extra), got %d "+
			"— the browser submit bypassed the transitioner's over-dispatch hygiene", superseded)
	}

	// No live copies remain after validate+supersede.
	var liveAfter int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
		wuID).Scan(&liveAfter); err != nil {
		t.Fatalf("count live after: %v", err)
	}
	if liveAfter != 0 {
		t.Fatalf("expected 0 live copies after validate+supersede, got %d", liveAfter)
	}

	// Exactly two results were credited (the quorum).
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
