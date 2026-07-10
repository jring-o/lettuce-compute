//go:build integration

package workunit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestDemoteAndRequeue_Integration drives the §9.7 disposition against a live unit: a
// VALIDATED work unit whose accepted output was refuted, carrying assignment-history
// copies already consumed and DISAGREED results beyond the default retry margin. It
// asserts the audit-H3 fix — the refunded ceiling is CountTotalCopies + a FULL fresh
// budget (an absolute rematerialization), not a bare += of the fraud set that would
// dead-letter the requeued unit — and that a second sweep does not double-refund.
func TestDemoteAndRequeue_Integration(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, pool, "demoter-int")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)

	// Seed a VALIDATED unit with budgets defaulted (0 = derive at resolve time), so a
	// naive += would materialize an absolute ceiling below the copies already consumed.
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateValidated
	validatedAt := time.Now().UTC()
	wu.ValidatedAt = &validatedAt
	wu.Priority = WorkUnitPriorityNormal
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seven consumed (closed) assignment-history copies — CountTotalCopies = 7. The
	// partial live-copy unique index only covers outcome IS NULL, so many closed copies
	// for one volunteer are fine.
	vol := createTestVolunteer(t, pool)
	const totalCopies = 7
	for i := 0; i < totalCopies; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO work_unit_assignment_history
				(work_unit_id, volunteer_id, assigned_at, started_at, outcome, outcome_at)
			VALUES ($1, $2, NOW(), NOW(), 'EXPIRED'::assignment_outcome, NOW())`,
			wu.ID, vol,
		); err != nil {
			t.Fatalf("insert consumed copy %d: %v", i, err)
		}
	}

	// Three DISAGREED results (fraud set) — distinct volunteers for the
	// (work_unit_id, volunteer_id) uniqueness constraint.
	for i := 0; i < 3; i++ {
		rv := createTestVolunteer(t, pool)
		if _, err := pool.Exec(ctx, `
			INSERT INTO results
				(work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
			VALUES ($1, $2, '{"v":1}'::jsonb, $3, '{}'::jsonb, 'DISAGREED')`,
			wu.ID, rv, fmt.Sprintf("disagreed-checksum-%d", i),
		); err != nil {
			t.Fatalf("insert DISAGREED result %d: %v", i, err)
		}
	}

	countTotal, err := repo.CountTotalCopies(ctx, wu.ID)
	if err != nil {
		t.Fatalf("CountTotalCopies: %v", err)
	}
	if countTotal != totalCopies {
		t.Fatalf("CountTotalCopies = %d, want %d", countTotal, totalCopies)
	}

	// resolveBudgets stands in for the main.go resolution: a fresh unit of this leaf gets
	// target(2) + margin(6) = 8 total copies, unlimited (0) error ceiling.
	const freshTotal = 8
	d := NewEnforcementDemoter(repo, func(context.Context, *WorkUnit) (int, int, error) {
		return freshTotal, 0, nil
	}, discardLogger())

	if err := d.DemoteAndRequeue(ctx, wu.ID); err != nil {
		t.Fatalf("DemoteAndRequeue: %v", err)
	}

	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID after demote: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Errorf("state = %s, want QUEUED", got.State)
	}
	// The H3 regression assertion: materialized ceiling = consumed copies + fresh budget.
	wantTotal := countTotal + freshTotal
	if got.MaxTotalCopies != wantTotal {
		t.Errorf("MaxTotalCopies = %d, want %d (CountTotalCopies %d + fresh %d)",
			got.MaxTotalCopies, wantTotal, countTotal, freshTotal)
	}
	// Unlimited error ceiling (0) stays untouched (the CASE).
	if got.MaxErrorCopies != 0 {
		t.Errorf("MaxErrorCopies = %d, want 0 (unlimited, untouched)", got.MaxErrorCopies)
	}
	if got.ValidatedAt != nil {
		t.Errorf("ValidatedAt = %v, want nil (cleared by requeue)", got.ValidatedAt)
	}
	if got.Priority != WorkUnitPriorityHigh {
		t.Errorf("Priority = %s, want HIGH (raised by requeue)", got.Priority)
	}
	if got.ReassignmentCount != 1 {
		t.Errorf("ReassignmentCount = %d, want 1 (bumped by requeue)", got.ReassignmentCount)
	}

	// Second sweep (leadership failover): the WHERE state = 'REJECTED' guard + the
	// already-QUEUED early return mean no double-refund and no second requeue.
	beforeTotal := got.MaxTotalCopies
	if err := d.DemoteAndRequeue(ctx, wu.ID); err != nil {
		t.Fatalf("DemoteAndRequeue (second pass): %v", err)
	}
	again, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID after second pass: %v", err)
	}
	if again.MaxTotalCopies != beforeTotal {
		t.Errorf("MaxTotalCopies = %d after second pass, want %d unchanged (no double-refund)",
			again.MaxTotalCopies, beforeTotal)
	}
	if again.State != WorkUnitStateQueued {
		t.Errorf("state = %s after second pass, want QUEUED", again.State)
	}
	if again.ReassignmentCount != 1 {
		t.Errorf("ReassignmentCount = %d after second pass, want 1 (early return, no re-requeue)",
			again.ReassignmentCount)
	}
}
