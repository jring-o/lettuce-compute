//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestFindLapsedReservations covers the #22 lapsed-lease reclaim gap: a still-QUEUED
// unit whose reservation lease has lapsed (reserved_until < NOW()) is returned (so
// the fault monitor can clear it), while a live reservation and an unreserved QUEUED
// unit are NOT returned.
func TestFindLapsedReservations(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "lapsed-find")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	vol := types.NewID()

	// lapsed: reserved with an already-past reserved_until.
	lapsed := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.StampReservation(ctx, lapsed.ID, vol, -time.Minute); err != nil {
		t.Fatalf("StampReservation(lapsed): %v", err)
	}

	// live: reserved with a future reserved_until — must NOT be returned.
	live := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.StampReservation(ctx, live.ID, vol, time.Hour); err != nil {
		t.Fatalf("StampReservation(live): %v", err)
	}

	// free: QUEUED and never reserved — must NOT be returned.
	_ = mustQueuedWU(t, ctx, repo, leafID)

	got, err := repo.FindLapsedReservations(ctx, 100)
	if err != nil {
		t.Fatalf("FindLapsedReservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 lapsed reservation, got %d", len(got))
	}
	if got[0].ID != lapsed.ID {
		t.Fatalf("expected lapsed unit %s, got %s", lapsed.ID, got[0].ID)
	}
	if got[0].ReservedVolunteerID == nil || *got[0].ReservedVolunteerID != vol {
		t.Fatalf("lapsed unit should still carry its reserved_volunteer_id")
	}
}

// TestFindLapsedReservations_ClearMakesReReservable asserts the full reclaim cycle:
// after FindLapsedReservations + ClearReservation the unit is plain QUEUED again and
// immediately re-reservable by any volunteer (it never left QUEUED, so no
// expire/reassign is involved).
func TestFindLapsedReservations_ClearMakesReReservable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "lapsed-clear")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	deadVol := types.NewID()
	freshVol := types.NewID()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.StampReservation(ctx, wu.ID, deadVol, -time.Minute); err != nil {
		t.Fatalf("StampReservation: %v", err)
	}

	lapsed, err := repo.FindLapsedReservations(ctx, 100)
	if err != nil {
		t.Fatalf("FindLapsedReservations: %v", err)
	}
	if len(lapsed) != 1 {
		t.Fatalf("expected 1 lapsed reservation, got %d", len(lapsed))
	}

	if _, err := repo.ClearReservation(ctx, lapsed[0].ID, *lapsed[0].ReservedVolunteerID); err != nil {
		t.Fatalf("ClearReservation: %v", err)
	}

	// The unit is plain QUEUED again: a fresh volunteer can reserve it.
	reserved, err := repo.ReserveNextAssignable(ctx, reserveOpts(freshVol, 0), time.Hour)
	if err != nil {
		t.Fatalf("ReserveNextAssignable after clear: %v", err)
	}
	if reserved == nil {
		t.Fatalf("expected the cleared unit to be re-reservable, got nil")
	}
	if reserved.ID != wu.ID {
		t.Fatalf("expected to re-reserve %s, got %s", wu.ID, reserved.ID)
	}
	if reserved.ReservedVolunteerID == nil || *reserved.ReservedVolunteerID != freshVol {
		t.Fatalf("re-reserved unit should now be held by the fresh volunteer")
	}

	// And no lapsed reservations remain.
	remaining, err := repo.FindLapsedReservations(ctx, 100)
	if err != nil {
		t.Fatalf("FindLapsedReservations (after): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no lapsed reservations after re-reserve, got %d", len(remaining))
	}
}
