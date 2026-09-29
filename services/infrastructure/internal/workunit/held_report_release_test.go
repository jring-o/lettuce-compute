//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"
)

// TestReleaseStaleHeldCopies_RecordsWhyTheHeadClosedTheCopy: a copy the head releases because
// the machine no longer reports holding it carries a reason, so the copy history tells it apart
// from a give-back the client asked for (which records the client's own reason) — started or not.
func TestReleaseStaleHeldCopies_RecordsWhyTheHeadClosedTheCopy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := createTestUser(t, pool, "held-report-reason")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)

	unrun := mustQueuedWU(t, ctx, repo, leafID)
	started := mustQueuedWU(t, ctx, repo, leafID)
	for _, wu := range []*WorkUnit{unrun, started} {
		if _, err := repo.ReserveCopy(ctx, wu.ID, vol, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}
	if _, err := repo.Assign(ctx, started.ID, vol); err != nil {
		t.Fatalf("run-start: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE work_unit_assignment_history SET created_at = NOW() - INTERVAL '10 minutes'
		WHERE volunteer_id = $1`, vol); err != nil {
		t.Fatalf("age copies: %v", err)
	}
	released, err := repo.ReleaseStaleHeldCopies(ctx, vol, nil, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ReleaseStaleHeldCopies: %v", err)
	}
	if len(released) != 2 {
		t.Fatalf("released %d copies, want 2", len(released))
	}
	rows, err := pool.Query(ctx, `
		SELECT outcome::text, outcome_reason FROM work_unit_assignment_history
		WHERE volunteer_id = $1 ORDER BY started_at NULLS FIRST`, vol)
	if err != nil {
		t.Fatalf("read copies: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var outcome string
		var reason *string
		if err := rows.Scan(&outcome, &reason); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if reason == nil || *reason != ReleasedNotHeldReason {
			t.Fatalf("%s copy released by the head carries reason %v, want %q", outcome, reason, ReleasedNotHeldReason)
		}
		got = append(got, outcome)
	}
	if len(got) != 2 || got[0] != "RETURNED" || got[1] != "ABANDONED" {
		t.Fatalf("outcomes = %v, want [RETURNED ABANDONED]", got)
	}
}
