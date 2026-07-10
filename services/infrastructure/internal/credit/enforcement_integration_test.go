//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// createTestResultAudit inserts a minimal QUEUED result_audits row (no verdict — the CHECK
// only requires a verdict once COMPLETED) so an AUDIT-stamped adjustment has a real audit_id
// FK target. It reuses the caller's existing work_unit/leaf/result parents.
func createTestResultAudit(t *testing.T, pool *pgxpool.Pool, wuID, leafID, resultID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO result_audits (
			id, work_unit_id, leaf_id, accepted_result_id, comparison_snapshot, execution_snapshot
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, wuID, leafID, resultID,
		json.RawMessage(`{}`), json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("create test result audit: %v", err)
	}
	return id
}

// seedRAC inserts a volunteer_rac row with a known rac and last_updated_at = now(), so a
// subsequent ApplyAdjustment decays over only a negligible interval before subtracting.
func seedRAC(t *testing.T, pool *pgxpool.Pool, volID, leafID types.ID, rac float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO volunteer_rac (volunteer_id, leaf_id, rac, total_credit, last_credit_at, last_updated_at)
		VALUES ($1, $2, $3, $3, now(), now())`,
		volID, leafID, rac,
	)
	if err != nil {
		t.Fatalf("seed volunteer_rac: %v", err)
	}
}

// --- ClawbackForAudit -------------------------------------------------------------------

// TestClawbackForAudit: an AUDIT clawback cancels the full remaining net, stamps
// created_by='AUDIT' and the causing audit_id, and a repeat call on the now-exhausted entry
// returns ErrAdjustmentExhausted (the F17 idempotent-no-op contract).
func TestClawbackForAudit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 5.0)
	auditID := createTestResultAudit(t, pool, entry.WorkUnitID, entry.LeafID, entry.ResultID)
	repo := NewPgxAdjustmentsRepository(pool)

	adj, err := repo.ClawbackForAudit(ctx, entry.ID, auditID, ReasonAuditMismatch)
	if err != nil {
		t.Fatalf("ClawbackForAudit: %v", err)
	}
	if adj.Amount != -5.0 {
		t.Errorf("adj.Amount = %v, want -5.0 (full remaining)", adj.Amount)
	}
	if adj.CreatedBy != AdjustmentByAudit {
		t.Errorf("adj.CreatedBy = %q, want %q", adj.CreatedBy, AdjustmentByAudit)
	}
	if adj.Reason != ReasonAuditMismatch {
		t.Errorf("adj.Reason = %q, want %q", adj.Reason, ReasonAuditMismatch)
	}
	if adj.Note != "" {
		t.Errorf("adj.Note = %q, want empty", adj.Note)
	}
	if adj.AuditID == nil || *adj.AuditID != auditID {
		t.Errorf("adj.AuditID = %v, want %v", adj.AuditID, auditID)
	}
	// volunteer_id/leaf_id come from the locked ledger row, not the caller.
	if adj.VolunteerID != entry.VolunteerID || adj.LeafID != entry.LeafID {
		t.Errorf("adj volunteer/leaf = %v/%v, want %v/%v", adj.VolunteerID, adj.LeafID, entry.VolunteerID, entry.LeafID)
	}

	// The audit_id round-trips through a read.
	list, err := repo.ListByVolunteer(ctx, entry.VolunteerID, 100, 0)
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	if list[0].AuditID == nil || *list[0].AuditID != auditID {
		t.Errorf("listed adjustment AuditID = %v, want %v", list[0].AuditID, auditID)
	}

	// Entry is now fully cancelled: a second AUDIT clawback is the idempotent no-op (F17).
	if _, err := repo.ClawbackForAudit(ctx, entry.ID, auditID, ReasonAuditMismatch); !errors.Is(err, ErrAdjustmentExhausted) {
		t.Fatalf("second ClawbackForAudit err = %v, want ErrAdjustmentExhausted", err)
	}
	if sum, _ := repo.SumForEntry(ctx, entry.ID); sum != -5.0 {
		t.Errorf("SumForEntry = %v, want -5.0 (net exactly 0)", sum)
	}
}

// --- ListUnmaturedEntryIDs --------------------------------------------------------------

// TestListUnmaturedEntryIDsBoundary: entries at now()-(days-ε) are inside the maturation
// window (returned); entries at now()-(days+ε) are matured (excluded); ordering is oldest
// first. The query does NOT pre-filter on remaining net — that is ClawbackForAudit's job.
func TestListUnmaturedEntryIDsBoundary(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const days = 7
	userID := createTestUser(t, pool, "unmat-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	ledger := NewPgxRepository(pool)

	// insert makes a fresh ledger entry for (volID, leafID) then forces its granted_at.
	insert := func(grantedInterval string) types.ID {
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID, randChecksum())
		e := &LedgerEntry{VolunteerID: volID, LeafID: leafID, WorkUnitID: wuID, ResultID: resultID, CreditAmount: 1.0}
		if err := ledger.Create(ctx, e); err != nil {
			t.Fatalf("create ledger entry: %v", err)
		}
		if _, err := pool.Exec(ctx,
			"UPDATE credit_ledger SET granted_at = now() - $2::interval WHERE id = $1", e.ID, grantedInterval); err != nil {
			t.Fatalf("age ledger entry: %v", err)
		}
		return e.ID
	}

	// Older-but-inside (6d23h): included. Just-matured (7d1h): excluded. Fresh (now): included.
	insideOld := insert("6 days 23 hours")
	insert("7 days 1 hour") // matured — must NOT appear
	insideFresh := insert("0 seconds")

	repo := NewPgxAdjustmentsRepository(pool)
	ids, err := repo.ListUnmaturedEntryIDs(ctx, volID, days)
	if err != nil {
		t.Fatalf("ListUnmaturedEntryIDs: %v", err)
	}
	// Expect exactly the two in-window entries, oldest first.
	want := []types.ID{insideOld, insideFresh}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v (matured entry must be excluded)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %v, want %v (oldest first)", i, ids[i], want[i])
		}
	}
}

// --- ApplyAdjustment (clamped RAC decrement) --------------------------------------------

// TestApplyAdjustmentDecrementsRAC: the clamped decrement subtracts the adjustment magnitude
// from the (decayed-to-now) RAC, and a second call is the exactly-once no-op leaving RAC
// untouched.
func TestApplyAdjustmentDecrementsRAC(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 3.0)
	seedRAC(t, pool, entry.VolunteerID, entry.LeafID, 10.0)

	adjRepo := NewPgxAdjustmentsRepository(pool)
	adj, err := adjRepo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("Clawback: %v", err)
	}

	racRepo := NewPgxRACRepository(pool)
	applied, err := racRepo.ApplyAdjustment(ctx, adj.ID)
	if err != nil {
		t.Fatalf("ApplyAdjustment: %v", err)
	}
	if !applied {
		t.Fatal("first ApplyAdjustment applied = false, want true")
	}

	// Seeded at now(), so decay over the few ms elapsed is negligible: rac ~= 10 - 3 = 7.
	e, err := racRepo.GetByVolunteerProject(ctx, entry.VolunteerID, entry.LeafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject: %v", err)
	}
	if e.RAC <= 6.9 || e.RAC > 7.0 {
		t.Errorf("RAC = %v, want ~7.0 (10 decayed-to-now minus 3, in (6.9, 7.0])", e.RAC)
	}
	after := e.RAC

	// Second apply is the exactly-once no-op: applied=false and RAC unchanged (step 2 skipped).
	applied, err = racRepo.ApplyAdjustment(ctx, adj.ID)
	if err != nil {
		t.Fatalf("second ApplyAdjustment: %v", err)
	}
	if applied {
		t.Error("second ApplyAdjustment applied = true, want false (already applied)")
	}
	e2, err := racRepo.GetByVolunteerProject(ctx, entry.VolunteerID, entry.LeafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject after re-apply: %v", err)
	}
	if e2.RAC != after {
		t.Errorf("RAC changed on no-op re-apply: %v -> %v", after, e2.RAC)
	}
}

// TestApplyAdjustmentClampsAtZero: when the magnitude exceeds the current RAC, the decrement
// clamps to 0 (GREATEST) rather than tripping CHECK (rac >= 0).
func TestApplyAdjustmentClampsAtZero(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 5.0)
	seedRAC(t, pool, entry.VolunteerID, entry.LeafID, 2.0)

	adjRepo := NewPgxAdjustmentsRepository(pool)
	adj, err := adjRepo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("Clawback: %v", err)
	}

	racRepo := NewPgxRACRepository(pool)
	applied, err := racRepo.ApplyAdjustment(ctx, adj.ID)
	if err != nil {
		t.Fatalf("ApplyAdjustment: %v", err)
	}
	if !applied {
		t.Fatal("ApplyAdjustment applied = false, want true")
	}

	e, err := racRepo.GetByVolunteerProject(ctx, entry.VolunteerID, entry.LeafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject: %v", err)
	}
	if e.RAC > 1e-6 {
		t.Errorf("RAC = %v, want 0 (2 - 5 clamped at 0)", e.RAC)
	}
}

// TestApplyAdjustmentMissingRACRow: with no volunteer_rac row, ApplyAdjustment is a stamp-only
// success — (true, nil), the adjustment is stamped, and no RAC row is fabricated.
func TestApplyAdjustmentMissingRACRow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 4.0)
	// No seedRAC — the (volunteer, leaf) has no RAC row.

	adjRepo := NewPgxAdjustmentsRepository(pool)
	adj, err := adjRepo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("Clawback: %v", err)
	}

	racRepo := NewPgxRACRepository(pool)
	applied, err := racRepo.ApplyAdjustment(ctx, adj.ID)
	if err != nil {
		t.Fatalf("ApplyAdjustment: %v", err)
	}
	if !applied {
		t.Fatal("ApplyAdjustment applied = false, want true (stamp-only success)")
	}

	// No RAC row was created.
	if _, err := racRepo.GetByVolunteerProject(ctx, entry.VolunteerID, entry.LeafID); err == nil {
		t.Error("a RAC row was fabricated; want NotFound (stamp-only decrement)")
	}

	// The stamp is durable: the adjustment now carries rac_applied_at and a re-apply no-ops.
	list, err := adjRepo.ListByVolunteer(ctx, entry.VolunteerID, 100, 0)
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(list) != 1 || list[0].RACAppliedAt == nil {
		t.Fatalf("expected one adjustment with RACAppliedAt set, got %+v", list)
	}
	applied, err = racRepo.ApplyAdjustment(ctx, adj.ID)
	if err != nil {
		t.Fatalf("second ApplyAdjustment: %v", err)
	}
	if applied {
		t.Error("second ApplyAdjustment applied = true, want false (already stamped)")
	}
}

// TestApplyAdjustmentNonexistent: an unknown adjustment id is a caller error, not a no-op.
func TestApplyAdjustmentNonexistent(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	racRepo := NewPgxRACRepository(pool)
	applied, err := racRepo.ApplyAdjustment(ctx, types.NewID())
	if applied {
		t.Error("applied = true for a nonexistent adjustment, want false")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 404 {
		t.Fatalf("err = %v, want a 404 NotFound", err)
	}
}
