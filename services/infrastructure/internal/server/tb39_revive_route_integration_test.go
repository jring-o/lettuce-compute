//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TB-39, end to end through the REAL router: the dead-letter parking is "for a human to
// review", and this is the review verb. This test deliberately references no new Go API —
// it seeds the filed kill shape (a healthy unit dead-lettered by un-run give-backs billed
// ABANDONED, its honest result disposed SUPERSEDED) with SQL and pre-existing repo calls,
// then drives the operator's HTTP surface. On pre-fix code it fails with 404: the product
// had no route, which is the filed defect (the requeue endpoint refuses FAILED, the state
// chart gave FAILED no exit, and remediation was raw SQL on production).
func TestTB39_ReviveRouteRestoresDeadLetteredUnit(t *testing.T) {
	env := setupAuthzMatrix(t)
	ctx := context.Background()

	wuRepo := workunit.NewPgxWorkUnitRepository(env.pool)
	wu := &workunit.WorkUnit{
		LeafID:           env.leafA.ID,
		State:            workunit.WorkUnitStateQueued,
		Priority:         workunit.WorkUnitPriorityNormal,
		InputData:        json.RawMessage(`{"x": 42}`),
		CodeArtifactRef:  "ref://tb39-revive",
		Parameters:       json.RawMessage(`{}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
	}
	if err := wuRepo.Create(ctx, wu); err != nil {
		t.Fatalf("create work unit: %v", err)
	}
	// Per-unit redundancy stamps: the authz harness's leaf carries a zero-value
	// validation_config (raw repo.Create, no defaults pass), which resolves quorum to 0
	// and would block the dead-letter guard (pending 1 < 0 never holds).
	if _, err := env.pool.Exec(ctx,
		`UPDATE work_units SET max_total_copies = 2, target_copies = 2, min_quorum = 2 WHERE id = $1`, wu.ID); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}

	// Two un-run give-backs billed ABANDONED (a pre-v0.10.8 client), one honest PENDING
	// result — the exact signature of the 42 dead units.
	volA, volB, volC := tb39Volunteer(t, env), tb39Volunteer(t, env), tb39Volunteer(t, env)
	for _, vol := range []types.ID{volA, volB} {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at, outcome, outcome_at, outcome_reason)
			VALUES ($1, $2, NOW(), 'ABANDONED', NOW(), 'work buffer full (over the hours target)')`,
			wu.ID, vol); err != nil {
			t.Fatalf("insert give-back row: %v", err)
		}
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO results (work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
		VALUES ($1, $2, '{"y":1}'::jsonb, repeat('a', 64), '{}'::jsonb, 'PENDING')`,
		wu.ID, volC); err != nil {
		t.Fatalf("insert honest result: %v", err)
	}

	// Park it through the real executor: FAILED + flagged, the result SUPERSEDED.
	failed, err := wuRepo.DeadLetterIfExhausted(ctx, wu.ID)
	if err != nil || !failed {
		t.Fatalf("DeadLetterIfExhausted = (%v, %v), want dead-lettered", failed, err)
	}

	w := env.do(http.MethodPost,
		"/api/v1/leafs/"+env.leafA.ID.String()+"/work-units/"+wu.ID.String()+"/revive",
		env.ownerKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST revive = %d (body %s), want 200 — the dead-letter review has no verb (TB-39)",
			w.Code, w.Body.String())
	}

	var state string
	var flagged bool
	if err := env.pool.QueryRow(ctx,
		`SELECT state, flagged_for_review FROM work_units WHERE id = $1`, wu.ID).Scan(&state, &flagged); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if state != "QUEUED" || flagged {
		t.Errorf("after revive state=%s flagged=%v, want QUEUED/unflagged", state, flagged)
	}
	var resultStatus string
	if err := env.pool.QueryRow(ctx,
		`SELECT validation_status FROM results WHERE work_unit_id = $1`, wu.ID).Scan(&resultStatus); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if resultStatus != "PENDING" {
		t.Errorf("result = %s, want PENDING (the orphaned honest compute restored)", resultStatus)
	}
	var returned int
	if err := env.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome = 'RETURNED'`,
		wu.ID).Scan(&returned); err != nil {
		t.Fatalf("count reclassified: %v", err)
	}
	if returned != 2 {
		t.Errorf("reclassified give-backs = %d, want 2", returned)
	}
}

// tb39Volunteer seeds one volunteer row (the workunit package's createTestVolunteer
// shape; that helper lives behind its own package's test files).
func tb39Volunteer(t *testing.T, env *authzMatrixEnv) types.ID {
	t.Helper()
	id := types.NewID()
	id1, id2 := uuid.New(), uuid.New()
	pubKey := make([]byte, 32)
	copy(pubKey, id1[:])
	copy(pubKey[16:], id2[:])
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes, scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{}', '{NATIVE}', 'ALWAYS', true, NOW())`,
		id, pubKey); err != nil {
		t.Fatalf("insert volunteer: %v", err)
	}
	return id
}
