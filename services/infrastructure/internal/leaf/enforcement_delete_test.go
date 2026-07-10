//go:build integration

package leaf

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// seedAuditFixture creates the minimal fabric a result_audits row references — a leaf, a
// volunteer, a COMPLETED work unit, and an AGREED result (NO credit_ledger row, so the
// existing credit probe stays clean and the audit-enforcement probe is what's under test).
func seedAuditFixture(t *testing.T, pool *pgxpool.Pool, userID types.ID) (leafID, wuID, resultID types.ID) {
	t.Helper()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create leaf: %v", err)
	}

	volID := types.NewID()
	pubKey := []byte(fmt.Sprintf("audit-del-pubkey-%s!!!!", volID.String()[:14]))
	if _, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, display_name) VALUES ($1, $2, $3)`,
		volID, pubKey, "Audit Del Vol"); err != nil {
		t.Fatalf("Create volunteer: %v", err)
	}

	wuID = types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_units (id, leaf_id, state, priority, code_artifact_ref, deadline_seconds)
		VALUES ($1, $2, 'COMPLETED', 'NORMAL', 'ref://test', 3600)`, wuID, p.ID); err != nil {
		t.Fatalf("Create work unit: %v", err)
	}

	resultID = types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO results (id, work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status, submitted_at)
		VALUES ($1, $2, $3, '{"r":1}', 'sha256:abc', '{}', 'AGREED', NOW())`,
		resultID, wuID, volID); err != nil {
		t.Fatalf("Create result: %v", err)
	}
	return p.ID, wuID, resultID
}

// insertAudit writes a COMPLETED/MISMATCH result_audits row for the fixture with the given
// enforcement_state and returns its id.
func insertAudit(t *testing.T, pool *pgxpool.Pool, leafID, wuID, resultID types.ID, enforcementState string) types.ID {
	t.Helper()
	auditID := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO result_audits (
			id, work_unit_id, leaf_id, accepted_result_id,
			comparison_snapshot, execution_snapshot,
			status, verdict, completed_at,
			enforcement_eligible, enforcement_state
		) VALUES (
			$1, $2, $3, $4,
			'{}'::jsonb, '{}'::jsonb,
			'COMPLETED', 'MISMATCH', NOW(),
			true, $5
		)`,
		auditID, wuID, leafID, resultID, enforcementState); err != nil {
		t.Fatalf("insert result_audits: %v", err)
	}
	return auditID
}

func assertCanDeleteConflict(t *testing.T, err error) *apierror.APIError {
	t.Helper()
	if err == nil {
		t.Fatal("expected 409 Conflict from CanDelete, got nil")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
	return apiErr
}

func TestCanDelete_CleanLeaf(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "candel-clean")
	leafID, _, _ := seedAuditFixture(t, pool, userID)

	// No credit, no audit-enforcement evidence — deletable.
	if err := CanDelete(context.Background(), pool, leafID, StateDraft); err != nil {
		t.Errorf("CanDelete on a clean leaf = %v, want nil", err)
	}
}

func TestCanDelete_EnforcedAuditBlocks(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "candel-enforced")
	leafID, wuID, resultID := seedAuditFixture(t, pool, userID)
	insertAudit(t, pool, leafID, wuID, resultID, "ENFORCED")

	apiErr := assertCanDeleteConflict(t, CanDelete(context.Background(), pool, leafID, StateDraft))
	if apiErr.Message != "cannot delete leaf with audit-enforcement history; archive instead" {
		t.Errorf("unexpected message: %s", apiErr.Message)
	}
}

func TestCanDelete_AuditRepairBlocks(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "candel-repair")
	leafID, wuID, resultID := seedAuditFixture(t, pool, userID)

	// Enforcement state NONE, but a repair row exists — the probe's OR branch must still
	// refuse deletion.
	auditID := insertAudit(t, pool, leafID, wuID, resultID, "NONE")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO audit_repairs (id, audit_id, result_id) VALUES ($1, $2, $3)`,
		types.NewID(), auditID, resultID); err != nil {
		t.Fatalf("insert audit_repairs: %v", err)
	}

	assertCanDeleteConflict(t, CanDelete(context.Background(), pool, leafID, StateDraft))
}
