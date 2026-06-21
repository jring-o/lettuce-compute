//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"
)

// FindLapsedReservations was retired with the per-unit reservation columns
// (migration 00006). A lapsed buffered hold is now a RESERVED copy (a
// work_unit_assignment_history row, started_at NULL, outcome NULL) whose
// reserved_until is in the past, and it is reclaimed by FindExpiredCopies →
// CloseCopy(ABANDONED). These tests cover that per-copy reclaim path.

// TestFindExpiredCopies_LapsedReservedCopy covers the lapsed-lease reclaim gap: a
// RESERVED copy past its reserved_until is returned by FindExpiredCopies (so the
// fault monitor can close it), while a live (future reserved_until) reserved copy
// and an unreserved QUEUED unit are NOT.
func TestFindExpiredCopies_LapsedReservedCopy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "lapsed-find")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	vol := createTestVolunteer(t, pool)

	// lapsed: a RESERVED copy whose reserved_until is already in the past.
	lapsed := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, lapsed.ID, vol, nil, time.Now().UTC().Add(-time.Minute), lapsed.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy(lapsed): %v", err)
	}

	// live: a RESERVED copy held into the future — must NOT be returned.
	live := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, live.ID, vol, nil, time.Now().UTC().Add(time.Hour), live.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy(live): %v", err)
	}

	// free: QUEUED and never reserved — must NOT be returned.
	_ = mustQueuedWU(t, ctx, repo, leafID)

	got, err := repo.FindExpiredCopies(ctx, 100)
	if err != nil {
		t.Fatalf("FindExpiredCopies: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 expired copy, got %d", len(got))
	}
	if got[0].WorkUnitID != lapsed.ID {
		t.Fatalf("expected lapsed unit %s, got %s", lapsed.ID, got[0].WorkUnitID)
	}
	if got[0].VolunteerID != vol {
		t.Fatalf("expired copy should carry its volunteer_id %s, got %s", vol, got[0].VolunteerID)
	}
	if got[0].StartedAt != nil {
		t.Fatalf("a lapsed buffered copy must be RESERVED (started_at NULL), got %v", got[0].StartedAt)
	}
}

// TestFindExpiredCopies_CloseMakesReReservable asserts the full reclaim cycle:
// after FindExpiredCopies + CloseCopy(ABANDONED) the lapsed copy is closed and the
// still-QUEUED unit (with no live copy left) is immediately re-reservable by a fresh
// volunteer — the unit never left QUEUED, so no expire/reassign is involved.
func TestFindExpiredCopies_CloseMakesReReservable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "lapsed-clear")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	deadVol := createTestVolunteer(t, pool)
	freshVol := createTestVolunteer(t, pool)

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, wu.ID, deadVol, nil, time.Now().UTC().Add(-time.Minute), wu.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy: %v", err)
	}

	lapsed, err := repo.FindExpiredCopies(ctx, 100)
	if err != nil {
		t.Fatalf("FindExpiredCopies: %v", err)
	}
	if len(lapsed) != 1 {
		t.Fatalf("expected 1 expired copy, got %d", len(lapsed))
	}

	// Close the lapsed copy (ABANDONED), freeing the unit's only redundancy slot.
	if err := repo.CloseCopy(ctx, lapsed[0].ID, "ABANDONED"); err != nil {
		t.Fatalf("CloseCopy: %v", err)
	}

	// The unit is plain QUEUED again with no live copy: a fresh volunteer can reserve it.
	reserved, err := repo.ReserveNextAssignable(ctx, reserveOpts(freshVol, 0), time.Hour)
	if err != nil {
		t.Fatalf("ReserveNextAssignable after close: %v", err)
	}
	if reserved == nil {
		t.Fatalf("expected the unit to be re-reservable after the lapsed copy was closed, got nil")
	}
	if reserved.ID != wu.ID {
		t.Fatalf("expected to re-reserve %s, got %s", wu.ID, reserved.ID)
	}
	if reserved.ReservedVolunteerID == nil || *reserved.ReservedVolunteerID != freshVol {
		t.Fatalf("re-reserved unit should now be held by the fresh volunteer")
	}

	// The fresh copy is held into the future, so no expired copies remain.
	remaining, err := repo.FindExpiredCopies(ctx, 100)
	if err != nil {
		t.Fatalf("FindExpiredCopies (after): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no expired copies after re-reserve, got %d", len(remaining))
	}
}
