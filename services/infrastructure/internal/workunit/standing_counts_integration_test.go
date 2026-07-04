//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setStanding updates a volunteer's account-standing columns directly, so the standing
// resolver (standingExprSQL) sees the CURRENT effective standing a live-copy holder has.
func setStanding(t *testing.T, pool *pgxpool.Pool, volID types.ID, standing string, benchedUntil *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE volunteers SET standing = $2, benched_until = $3 WHERE id = $1`,
		volID, standing, benchedUntil); err != nil {
		t.Fatalf("set standing for %s: %v", volID, err)
	}
}

// TestCountProbationLiveCopies stages four live copies of one unit, each held by a volunteer in a
// different standing, and asserts CountProbationLiveCopies counts exactly the non-OK holders:
//   - OK holder                       -> NOT counted;
//   - PROBATION holder                -> counted;
//   - BENCHED with an EXPIRED bench    -> counted (an expired bench resolves to PROBATION);
//   - BENCHED with a FUTURE bench      -> counted (still BENCHED).
//
// The count resolves the holder's CURRENT effective standing (standingExprSQL), not a stamp:
// clearing a probation holder back to OK immediately drops it from the count.
func TestCountProbationLiveCopies(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, pool, "standing-live-counts")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)

	unit := mustQueuedWU(t, ctx, repo, leafID)
	until := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)

	okVol := createTestVolunteer(t, pool)      // OK -> not counted
	probVol := createTestVolunteer(t, pool)    // PROBATION -> counted
	expiredVol := createTestVolunteer(t, pool) // BENCHED, expired -> PROBATION -> counted
	futureVol := createTestVolunteer(t, pool)  // BENCHED, future -> BENCHED -> counted

	// Reserve while every holder is still OK, THEN move their standings: ReserveCopy refuses
	// a currently-BENCHED reserver outright, so a benched account can only ever HOLD a live
	// copy it acquired before the bench — exactly the production shape this count exists for
	// (the standing change lands mid-flight and the copy stops covering redundancy).
	for _, v := range []types.ID{okVol, probVol, expiredVol, futureVol} {
		if _, err := repo.ReserveCopy(ctx, unit.ID, v, nil, until, 3600); err != nil {
			t.Fatalf("ReserveCopy for %s: %v", v, err)
		}
	}

	setStanding(t, pool, probVol, "PROBATION", nil)
	setStanding(t, pool, expiredVol, "BENCHED", &past)
	setStanding(t, pool, futureVol, "BENCHED", &future)

	if n, err := repo.CountLiveCopies(ctx, unit.ID); err != nil || n != 4 {
		t.Fatalf("CountLiveCopies = %d (err %v), want 4 (all four copies live)", n, err)
	}
	got, err := repo.CountProbationLiveCopies(ctx, unit.ID)
	if err != nil {
		t.Fatalf("CountProbationLiveCopies: %v", err)
	}
	if got != 3 {
		t.Fatalf("CountProbationLiveCopies = %d, want 3 (probation + expired-bench + future-bench; OK excluded)", got)
	}

	// CURRENT standing, not a stamp: clearing the probation holder back to OK drops the count.
	setStanding(t, pool, probVol, "OK", nil)
	if got, err := repo.CountProbationLiveCopies(ctx, unit.ID); err != nil || got != 2 {
		t.Fatalf("after clearing one holder to OK: CountProbationLiveCopies = %d (err %v), want 2", got, err)
	}
}
