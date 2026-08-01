//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TB-35: un-run give-backs (a batch tail returned by a full client buffer) used to be
// closed ABANDONED, and the dead-letter ceiling counted every history row, so a healthy
// unit whose copies kept bouncing between full buffers burned max_total_copies and was
// parked FAILED with zero compute ever attempted — 33 units in one day on the live heads,
// 8 of them carrying an orphaned honest PENDING result. Between exhaustion and the sweep,
// dispatch kept serving the unit even though no copy row could ever be minted again (the
// claimless "zombie" hand-outs behind the all-day "no live copy" WARN storms).
//
// The fix: give-backs close RETURNED (verified un-started at the write), RETURNED counts
// toward neither the total ceiling nor the error cap, every dispatch read/landing refuses
// a budget-exhausted unit, and a RETURNED copy carries only a short same-volunteer
// re-offer cooldown. These tests pin each half.

// copyOutcome reads the (single) closed outcome of volID's copy of wuID.
func copyOutcome(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID) string {
	t.Helper()
	var outcome string
	if err := pool.QueryRow(context.Background(), `
		SELECT outcome::text FROM work_unit_assignment_history
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NOT NULL`,
		wuID, volID).Scan(&outcome); err != nil {
		t.Fatalf("read copy outcome: %v", err)
	}
	return outcome
}

// setMaxTotalCopies stamps a per-unit dead-letter ceiling directly.
func setMaxTotalCopies(t *testing.T, pool *pgxpool.Pool, wuID types.ID, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE work_units SET max_total_copies = $2 WHERE id = $1`, wuID, n); err != nil {
		t.Fatalf("set max_total_copies: %v", err)
	}
}

// TestCloseCopyByVolunteer_ReturnedVerifiedUnstarted pins the write-point verification:
// RETURNED lands only on a copy that never run-started; a STARTED copy closes ABANDONED
// no matter what the caller (i.e. the client's wire flag) asked for.
func TestCloseCopyByVolunteer_ReturnedVerifiedUnstarted(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb35-returned-verify")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Un-started copy closed RETURNED stays RETURNED.
	unrun := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, unrun.ID, volA, nil, time.Now().UTC().Add(time.Hour), unrun.DeadlineSeconds); err != nil {
		t.Fatalf("reserve un-run copy: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, unrun.ID, volA, "RETURNED", nil, "work buffer full (over the hours target)"); err != nil {
		t.Fatalf("close un-run copy RETURNED: %v", err)
	}
	if got := copyOutcome(t, pool, unrun.ID, volA); got != "RETURNED" {
		t.Fatalf("un-started give-back closed %q, want RETURNED", got)
	}

	// A STARTED copy closed with the give-back outcome downgrades to ABANDONED —
	// the flag can never whitewash work the volunteer actually began and dropped.
	started := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, started.ID, volA, nil, time.Now().UTC().Add(time.Hour), started.DeadlineSeconds); err != nil {
		t.Fatalf("reserve started copy: %v", err)
	}
	if _, err := repo.Assign(ctx, started.ID, volA); err != nil {
		t.Fatalf("run-start copy: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, started.ID, volA, "RETURNED", nil, ""); err != nil {
		t.Fatalf("close started copy: %v", err)
	}
	if got := copyOutcome(t, pool, started.ID, volA); got != "ABANDONED" {
		t.Fatalf("started copy closed %q under the give-back flag, want ABANDONED", got)
	}
}

// TestTB35_ReturnedGivebacksDoNotBurnCopyBudget is the accounting half: a unit whose
// entire history is RETURNED give-backs has spent NONE of its budget — it neither
// dead-letters nor stops being dispatchable — while the same volume of real abandons
// still kills it (the poison-unit valve is intact).
func TestTB35_ReturnedGivebacksDoNotBurnCopyBudget(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb35-budget")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Give-back churn: ceiling 2, two RETURNED rows — the exact volume that used to
	// dead-letter the unit (each give-back was an ABANDONED row and 2 >= 2).
	churned := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, churned.ID, 2)
	for _, vol := range []types.ID{volA, volB} {
		if _, err := repo.ReserveCopy(ctx, churned.ID, vol, nil, time.Now().UTC().Add(time.Hour), churned.DeadlineSeconds); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := repo.CloseCopyByVolunteer(ctx, churned.ID, vol, "RETURNED", nil, "work buffer full (over the hours target)"); err != nil {
			t.Fatalf("close RETURNED: %v", err)
		}
	}

	if n, err := repo.CountTotalCopies(ctx, churned.ID); err != nil || n != 0 {
		t.Fatalf("CountTotalCopies after 2 give-backs = %d (err %v), want 0 (budget-neutral)", n, err)
	}
	failed, err := repo.DeadLetterIfExhausted(ctx, churned.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if failed {
		t.Fatal("a unit whose history is only give-backs must NOT dead-letter")
	}
	got, err := repo.GetByID(ctx, churned.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Fatalf("give-back-churned unit state = %s, want QUEUED", got.State)
	}

	// The valve is intact: the same ceiling with two REAL closed copies dead-letters.
	poisoned := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, poisoned.ID, 2)
	insertClosedCopy(t, pool, poisoned.ID, volA, "EXPIRED")
	insertClosedCopy(t, pool, poisoned.ID, volB, "ABANDONED")
	failed, err = repo.DeadLetterIfExhausted(ctx, poisoned.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted(poisoned): %v", err)
	}
	if !failed {
		t.Fatal("real EXPIRED/ABANDONED copies at the ceiling must still dead-letter")
	}
}

// TestTB35_BudgetExhaustedUnitNotDispatchable is the zombie half: once a unit's budget
// is spent, EVERY dispatch read and landing refuses it — it is never staged, claimed,
// assigned, or landed again while it waits for the dead-letter sweep. Before the fix all
// five sites kept serving it (the refill predicates never looked at the budget), which
// produced hours of claimless hand-outs whose every return failed "no live copy".
func TestTB35_BudgetExhaustedUnitNotDispatchable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb35-zombie")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	volC := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 2)
	insertClosedCopy(t, pool, wu.ID, volA, "EXPIRED")
	insertClosedCopy(t, pool, wu.ID, volB, "ABANDONED")

	// Refill (single-replica): not staged.
	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	for _, c := range cands {
		if c.WorkUnit.ID == wu.ID {
			t.Fatal("budget-exhausted unit staged by FindDispatchableBatch")
		}
	}

	// Refill (scale-out claim): not claimed.
	headID := types.NewID()
	claimed, err := repo.ClaimDispatchableBatch(ctx, headID, time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch: %v", err)
	}
	for _, c := range claimed {
		if c.WorkUnit.ID == wu.ID {
			t.Fatal("budget-exhausted unit claimed by ClaimDispatchableBatch")
		}
	}

	// Browser immediate-assign read: not offered.
	next, err := repo.FindNextAssignable(ctx, reserveOpts(volC, 0))
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if next != nil && next.ID == wu.ID {
		t.Fatal("budget-exhausted unit offered by FindNextAssignable")
	}

	// Landing write: a stale in-memory hold must not land one more row past the ceiling.
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: wu.ID, VolunteerID: volC, ReservedUntil: time.Now().UTC().Add(time.Hour), DeadlineSeconds: wu.DeadlineSeconds},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if containsFlushedPair(landed, wu.ID, volC) {
		t.Fatal("budget-exhausted unit landed a reservation past its ceiling")
	}

	// Spot-check landing: refused too.
	if _, err := repo.ReserveCopy(ctx, wu.ID, volC, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
		t.Fatal("budget-exhausted unit minted a copy via ReserveCopy")
	}
}

// TestTB35_ReturnedReofferCooldown pins the give-back's only teeth: the returning
// volunteer is not re-offered the same unit inside the short cooldown, a fresh
// volunteer is served immediately, and the pool-exhausted fallback still re-admits the
// returner once the grace passes with nobody else covering the unit (a one-volunteer
// pool never strands). This is what breaks the same-machine ping-pong (29 laps on one
// traced unit) even before the client's fetch hysteresis ships.
func TestTB35_ReturnedReofferCooldown(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb35-cooldown")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "RETURNED", nil, "work buffer full (over the hours target)"); err != nil {
		t.Fatalf("close volA RETURNED: %v", err)
	}

	// Inside the cooldown: the returner is refused on the read side...
	if got, err := repo.FindNextAssignable(ctx, reserveOpts(volA, 0)); err != nil {
		t.Fatalf("FindNextAssignable(volA): %v", err)
	} else if got != nil && got.ID == wu.ID {
		t.Fatal("unit re-offered to its returner inside the give-back cooldown")
	}
	// ...and on the landing side.
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: wu.ID, VolunteerID: volA, ReservedUntil: time.Now().UTC().Add(time.Hour), DeadlineSeconds: wu.DeadlineSeconds},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations(volA): %v", err)
	}
	if containsFlushedPair(landed, wu.ID, volA) {
		t.Fatal("returner's reservation landed inside the give-back cooldown")
	}

	// The benched snapshot carries the give-back entry (marked Returned) so the
	// in-memory hand-out gives the returner last refusal too.
	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	var found bool
	for _, c := range cands {
		if c.WorkUnit.ID != wu.ID {
			continue
		}
		for _, b := range c.Benched {
			if b.VolunteerID == volA && b.Returned {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("give-back entry missing from the benched snapshot (in-memory hand-out would re-offer instantly)")
	}

	// A FRESH volunteer is served immediately — the cooldown is per (unit, volunteer).
	if _, err := repo.ReserveCopy(ctx, wu.ID, volB, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("fresh volunteer must be admitted during the returner's cooldown: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volB, "RETURNED", nil, ""); err != nil {
		t.Fatalf("close volB RETURNED: %v", err)
	}

	// Pool-exhausted fallback: age both give-backs past the grace with zero live
	// copies — the returner is re-admitted rather than the pool stranding the unit.
	if _, err := pool.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome_at = NOW() - make_interval(secs => $2)
		WHERE work_unit_id = $1`, wu.ID, BenchPoolExhaustedGraceSeconds+60); err != nil {
		t.Fatalf("age give-backs: %v", err)
	}
	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("pool-exhausted fallback must re-admit the returner: %v", err)
	}
}

// TestReleaseStaleHeldCopies_UnstartedClosesReturned pins the reaper-side twin: a
// buffered (never-started) copy a machine no longer reports holding is a give-back —
// closed RETURNED, budget-neutral — while a RUN-STARTED stale copy still closes
// ABANDONED (lost compute is a real signal).
func TestReleaseStaleHeldCopies_UnstartedClosesReturned(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb35-stale-release")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	unrun := mustQueuedWU(t, ctx, repo, leafID)
	started := mustQueuedWU(t, ctx, repo, leafID)
	for _, wu := range []*WorkUnit{unrun, started} {
		if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}
	if _, err := repo.Assign(ctx, started.ID, volA); err != nil {
		t.Fatalf("run-start: %v", err)
	}
	// Age both copies past the reconcile grace, then release with an empty held set.
	if _, err := pool.Exec(ctx, `
		UPDATE work_unit_assignment_history SET created_at = NOW() - INTERVAL '10 minutes'
		WHERE volunteer_id = $1`, volA); err != nil {
		t.Fatalf("age copies: %v", err)
	}
	released, err := repo.ReleaseStaleHeldCopies(ctx, volA, nil, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ReleaseStaleHeldCopies: %v", err)
	}
	if len(released) != 2 {
		t.Fatalf("released %d copies, want 2", len(released))
	}
	if got := copyOutcome(t, pool, unrun.ID, volA); got != "RETURNED" {
		t.Fatalf("stale un-started copy closed %q, want RETURNED (budget-neutral give-back)", got)
	}
	if got := copyOutcome(t, pool, started.ID, volA); got != "ABANDONED" {
		t.Fatalf("stale run-started copy closed %q, want ABANDONED (lost compute)", got)
	}
}
