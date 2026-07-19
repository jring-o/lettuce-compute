//go:build integration

package workunit

// PB-9 regression: the post-failure cooldown's pool-exhausted fallback
// (head-setup.md §Redundancy: a benched volunteer "becomes eligible again if the
// pool is otherwise exhausted (so work never strands)"). Pre-fix no such fallback
// existed — the SQL cooldown refused unconditionally for ~one deadline, so a
// one-volunteer pool stranded its failed units for the full cooldown (hours on
// long-deadline leaves) with no operator recourse. Differential: this file uses
// only pre-fix helpers, so it can be dropped onto the pre-fix tree and FAILS there.

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// flushFor builds the single-record FlushReservations input for (wu, vol).
func flushFor(wuID, volID types.ID, deadlineSeconds int) []FlushReservation {
	return []FlushReservation{{
		WorkUnitID:      wuID,
		VolunteerID:     volID,
		ReservedUntil:   time.Now().UTC().Add(time.Minute),
		DeadlineSeconds: deadlineSeconds,
	}}
}

// TestCooldownPoolExhaustedFallback pins the fallback's three regimes at the
// authoritative landing gate (FlushReservations):
//
//  1. benching outcome older than the fallback grace + unit has ZERO live copies
//     (nobody fresh took it) → the bench yields, the reservation LANDS;
//  2. same aged outcome but another volunteer holds a live copy → the pool is not
//     exhausted, the bench holds, the reservation is refused;
//  3. a fresh benching outcome (inside the grace) + zero live copies → fresh
//     volunteers keep their first-refusal window, the reservation is refused.
func TestCooldownPoolExhaustedFallback(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "cooldown-fallback")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Ages chosen against the unit deadline (3600s = the cooldown window) and the
	// fallback grace (BenchPoolExhaustedGraceSeconds = 120s). Written as literals —
	// not the constant — so this differential test compiles on the PRE-fix tree,
	// where the constant does not exist; the parity suite compile-pins the constant.
	agedPastGrace := 180 // inside the window, past the 120s grace
	freshInGrace := 30   // inside the grace

	// Case 1: exhausted pool → the bench yields and the reservation lands.
	wu1 := mustQueuedWU(t, ctx, repo, leafID)
	insertCooldownCopy(t, pool, wu1.ID, volA, "EXPIRED", true, agedPastGrace)
	landed, err := repo.FlushReservations(ctx, flushFor(wu1.ID, volA, wu1.DeadlineSeconds), types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations (exhausted): %v", err)
	}
	if !containsFlushedPair(landed, wu1.ID, volA) {
		t.Fatal("benched volunteer NOT re-admitted although the unit sat uncovered past the fallback grace: the documented pool-exhausted fallback does not exist and a small pool strands its work (PB-9)")
	}

	// Case 2: same aged outcome, but the unit is covered by another volunteer's
	// live copy → the bench must hold (fresh volunteers keep first refusal).
	wu2 := mustQueuedWU(t, ctx, repo, leafID)
	insertCooldownCopy(t, pool, wu2.ID, volA, "EXPIRED", true, agedPastGrace)
	insertLiveCopy(t, pool, wu2.ID, volB, nil)
	landed, err = repo.FlushReservations(ctx, flushFor(wu2.ID, volA, wu2.DeadlineSeconds), types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations (covered): %v", err)
	}
	if containsFlushedPair(landed, wu2.ID, volA) {
		t.Fatal("bench yielded although another volunteer holds a live copy: the fallback must fire only on an exhausted pool")
	}

	// Case 3: fresh benching outcome (inside the grace), zero live copies → still
	// benched; the fallback must not defeat the fresh-volunteer-first window.
	wu3 := mustQueuedWU(t, ctx, repo, leafID)
	insertCooldownCopy(t, pool, wu3.ID, volA, "EXPIRED", true, freshInGrace)
	landed, err = repo.FlushReservations(ctx, flushFor(wu3.ID, volA, wu3.DeadlineSeconds), types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations (fresh): %v", err)
	}
	if containsFlushedPair(landed, wu3.ID, volA) {
		t.Fatal("bench yielded inside the fallback grace: fresh volunteers must keep their first-refusal window")
	}
}
