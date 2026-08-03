//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"
)

// TB-27: the abandon reason — since v0.10.3 the failing process's own output tail,
// collected so the head alone could diagnose a broken leaf — was logged once and
// persisted nowhere. After log rotation (or the container recreate every upgrade
// performs) head-side failure forensics had nothing. These tests pin that the reason
// now survives on the assignment history row.

// CloseCopyByVolunteer must persist the client's reason text on the closed row.
func TestCloseCopyByVolunteer_PersistsAbandonReason(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "abandon-reason")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}

	reason := "non-zero exit code 137; output: worker: signal: killed"
	if _, err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil, reason); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	var stored *string
	if err := pool.QueryRow(ctx,
		`SELECT outcome_reason FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2`,
		wu.ID, volA,
	).Scan(&stored); err != nil {
		t.Fatalf("read outcome_reason: %v", err)
	}
	if stored == nil || *stored != reason {
		got := "<NULL>"
		if stored != nil {
			got = *stored
		}
		t.Fatalf("outcome_reason = %q, want %q — the abandon reason must survive on the row (TB-27)", got, reason)
	}
}

// An empty reason (a submit close, or a client that sent none) stores NULL, not "".
func TestCloseCopyByVolunteer_EmptyReasonStoresNull(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "abandon-reason-null")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if _, err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil, ""); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	var stored *string
	if err := pool.QueryRow(ctx,
		`SELECT outcome_reason FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2`,
		wu.ID, volA,
	).Scan(&stored); err != nil {
		t.Fatalf("read outcome_reason: %v", err)
	}
	if stored != nil {
		t.Fatalf("outcome_reason = %q, want NULL for an empty reason", *stored)
	}
}
