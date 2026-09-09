//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TB-81: an ABANDONED copy that never started — the volunteer could not even begin
// the unit (no runtime, an unreachable container engine, a prepare error) — benched
// its volunteer nowhere and counted against the unit's copy budget every time. One
// machine with a dead container engine re-took the same unit every ten minutes,
// spent its nine-copy budget in 80 minutes, and dead-lettered 22 healthy units in 30
// hours on infra.scios.tech, each holding a good result from another volunteer that
// the dead-letter set aside. The #59 exemption behind the no-bench rule ("a graceful
// return of un-started buffered work is not a reliability signal") predates the
// give-back flag (TB-35): a graceful return now closes RETURNED, so an un-started
// ABANDONED is a failure by the client's own account.
//
// Two rules, each pinned here. Bench: an un-started ABANDONED benches the volunteer
// on the unit for ~one deadline at every dispatch gate, exactly like EXPIRED or a
// started abandon (the pool-exhausted fallback still applies, so a small pool cannot
// strand — cooldown_fallback_integration_test.go). Budget: one volunteer's un-started
// abandons count ONCE toward the total ceiling and the error cap however many there
// are; distinct volunteers still count one each, and a started abandon still counts
// per copy.
//
// Red evidence (2026-09-09, pre-fix tree, these tests in place):
//   TestTB81_UnstartedAbandonBenches/FindNextAssignable — "re-offered the unit it
//     could not start"; /ReserveCopy — "ReserveCopy admitted the volunteer";
//     /FlushReservations — "landed"; /FindDispatchableBatch — "not in the benched
//     snapshot".
//   TestTB81_OneVolunteersUnstartedAbandonsCountOnce — "CountTotalCopies after 5
//     un-started abandons by one volunteer = 5, want 1".
//   TestTB81_OperatorReport_DeadEngineCannotDeadLetterUnitHoldingAResult —
//     "dead-lettered" and "result ... = SUPERSEDED, want PENDING".

// closeUnstartedAbandon reserves wu for vol (a buffered, never-started copy) and
// closes it ABANDONED with the client's engine-down reason — the exact row the
// volunteer client writes for a prepare-time container-engine failure.
func closeUnstartedAbandon(t *testing.T, ctx context.Context, repo *PgxWorkUnitRepository, wu *WorkUnit, vol types.ID) {
	t.Helper()
	if _, err := repo.ReserveCopy(ctx, wu.ID, vol, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("reserve copy for %v: %v", vol, err)
	}
	closed, err := repo.CloseCopyByVolunteer(ctx, wu.ID, vol, "ABANDONED", nil,
		"docker is not available: docker ping: Cannot connect to the Docker daemon at unix:///run/user/1000/podman/podman.sock")
	if err != nil {
		t.Fatalf("close copy ABANDONED for %v: %v", vol, err)
	}
	if closed.Outcome != "ABANDONED" {
		t.Fatalf("close wrote %q, want ABANDONED", closed.Outcome)
	}
}

// insertUnstartedAbandon writes a closed ABANDONED row with started_at NULL directly
// (the shape closeUnstartedAbandon produces), for histories the cooldown would refuse
// to build through the gates — a volunteer benched from the unit cannot reserve it
// again, which is the point of TB-81's bench half.
func insertUnstartedAbandon(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID) {
	t.Helper()
	insertCooldownCopy(t, pool, wuID, vol, "ABANDONED", false, 0)
}

// TestTB81_UnstartedAbandonBenches pins the bench half at the four requester-aware
// gates: the abandoner is refused its own unit for the deadline window while a fresh
// volunteer is admitted at once.
func TestTB81_UnstartedAbandonBenches(t *testing.T) {
	t.Run("FindNextAssignable", func(t *testing.T) {
		pool, cleanup := setupTestDB(t)
		defer cleanup()
		userID := createTestUser(t, pool, "tb81-find")
		leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
		volA, volB := createTestVolunteer(t, pool), createTestVolunteer(t, pool)
		repo := NewPgxWorkUnitRepository(pool)
		ctx := context.Background()

		wu := mustQueuedWU(t, ctx, repo, leafID)
		closeUnstartedAbandon(t, ctx, repo, wu, volA)

		got, err := repo.FindNextAssignable(ctx, reserveOpts(volA, 0))
		if err != nil {
			t.Fatalf("FindNextAssignable(volA): %v", err)
		}
		if got != nil && got.ID == wu.ID {
			t.Fatal("FindNextAssignable re-offered the unit it could not start to the same volunteer: an un-started ABANDONED must bench (TB-81)")
		}
		got, err = repo.FindNextAssignable(ctx, reserveOpts(volB, 0))
		if err != nil {
			t.Fatalf("FindNextAssignable(volB): %v", err)
		}
		if got == nil || got.ID != wu.ID {
			t.Fatalf("a fresh volunteer must be offered the unit during the abandoner's bench, got %v", got)
		}
	})

	t.Run("ReserveCopy", func(t *testing.T) {
		pool, cleanup := setupTestDB(t)
		defer cleanup()
		userID := createTestUser(t, pool, "tb81-reserve")
		leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
		volA, volB := createTestVolunteer(t, pool), createTestVolunteer(t, pool)
		repo := NewPgxWorkUnitRepository(pool)
		ctx := context.Background()

		wu := mustQueuedWU(t, ctx, repo, leafID)
		closeUnstartedAbandon(t, ctx, repo, wu, volA)

		if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
			t.Fatal("ReserveCopy admitted the volunteer to the unit it could not start: an un-started ABANDONED must bench (TB-81)")
		}
		if _, err := repo.ReserveCopy(ctx, wu.ID, volB, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
			t.Fatalf("a fresh volunteer must be admitted during the abandoner's bench: %v", err)
		}
	})

	t.Run("FlushReservations", func(t *testing.T) {
		pool, cleanup := setupTestDB(t)
		defer cleanup()
		userID := createTestUser(t, pool, "tb81-flush")
		leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
		volA, volB := createTestVolunteer(t, pool), createTestVolunteer(t, pool)
		repo := NewPgxWorkUnitRepository(pool)
		ctx := context.Background()

		wu := mustQueuedWU(t, ctx, repo, leafID)
		closeUnstartedAbandon(t, ctx, repo, wu, volA)

		until := time.Now().UTC().Add(time.Hour)
		landed, err := repo.FlushReservations(ctx, []FlushReservation{
			{WorkUnitID: wu.ID, VolunteerID: volA, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
			{WorkUnitID: wu.ID, VolunteerID: volB, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
		}, types.ID{}, 0)
		if err != nil {
			t.Fatalf("FlushReservations: %v", err)
		}
		if containsFlushedPair(landed, wu.ID, volA) {
			t.Fatal("the abandoner's reservation landed: the flush gate must refuse a volunteer inside its un-started-abandon bench (TB-81)")
		}
		if !containsFlushedPair(landed, wu.ID, volB) {
			t.Fatal("a fresh volunteer's reservation must land during the abandoner's bench")
		}
	})

	t.Run("FindDispatchableBatch", func(t *testing.T) {
		pool, cleanup := setupTestDB(t)
		defer cleanup()
		userID := createTestUser(t, pool, "tb81-batch")
		leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
		volA := createTestVolunteer(t, pool)
		repo := NewPgxWorkUnitRepository(pool)
		ctx := context.Background()

		wu := mustQueuedWU(t, ctx, repo, leafID)
		closeUnstartedAbandon(t, ctx, repo, wu, volA)

		cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
		if err != nil {
			t.Fatalf("FindDispatchableBatch: %v", err)
		}
		var cand *DispatchCandidate
		for i := range cands {
			if cands[i].WorkUnit.ID == wu.ID {
				cand = &cands[i]
				break
			}
		}
		if cand == nil {
			t.Fatal("unit not staged after an un-started abandon (it must stay dispatchable to fresh volunteers)")
		}
		found := false
		for _, b := range cand.Benched {
			if b.VolunteerID == volA {
				found = true
				if b.Returned {
					t.Fatal("the abandoner's bench entry is marked as a RETURNED give-back: it must carry the deadline window, not the re-offer throttle")
				}
			}
		}
		if !found {
			t.Fatalf("the abandoner is not in the benched snapshot the dispatch cache reads, got %v (TB-81)", cand.Benched)
		}
	})
}

// TestTB81_OneVolunteersUnstartedAbandonsCountOnce pins the budget half: five
// un-started abandons by ONE volunteer are one billed copy and one error copy, the
// unit neither dead-letters nor stops being dispatchable — while the same failures
// from DISTINCT volunteers still count one each, so a unit nobody can start still
// exhausts its budget.
func TestTB81_OneVolunteersUnstartedAbandonsCountOnce(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "tb81-budget")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA, volB, volC := createTestVolunteer(t, pool), createTestVolunteer(t, pool), createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 3)
	for i := 0; i < 5; i++ {
		insertUnstartedAbandon(t, pool, wu.ID, volA)
	}

	if n, err := repo.CountTotalCopies(ctx, wu.ID); err != nil || n != 1 {
		t.Fatalf("CountTotalCopies after 5 un-started abandons by one volunteer = %d (err %v), want 1 (TB-81)", n, err)
	}
	if n, err := repo.CountErrorCopies(ctx, wu.ID); err != nil || n != 1 {
		t.Fatalf("CountErrorCopies after 5 un-started abandons by one volunteer = %d (err %v), want 1 (TB-81)", n, err)
	}
	failed, err := repo.DeadLetterIfExhausted(ctx, wu.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if failed {
		t.Fatal("one volunteer's repeated un-started abandons dead-lettered the unit (TB-81)")
	}
	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	staged := false
	for i := range cands {
		if cands[i].WorkUnit.ID == wu.ID {
			staged = true
		}
	}
	if !staged {
		t.Fatal("unit no longer dispatchable after one volunteer's repeated un-started abandons: the dispatch-side budget gate must count them once too")
	}

	// Distinct volunteers still count one each: with the ceiling at 3, two more
	// machines that cannot start the unit spend it.
	insertUnstartedAbandon(t, pool, wu.ID, volB)
	insertUnstartedAbandon(t, pool, wu.ID, volC)
	if n, err := repo.CountTotalCopies(ctx, wu.ID); err != nil || n != 3 {
		t.Fatalf("CountTotalCopies with three distinct un-started abandoners = %d (err %v), want 3", n, err)
	}
	failed, err = repo.DeadLetterIfExhausted(ctx, wu.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if !failed {
		t.Fatal("three distinct volunteers failing to start the unit must still exhaust a ceiling of 3 (the poison-unit valve is intact)")
	}
}

// TestTB81_StartedAbandonsStillCountPerCopy guards the boundary: a copy the volunteer
// STARTED and then abandoned is evidence about the unit and still spends one budget
// copy per attempt, even from a single volunteer.
func TestTB81_StartedAbandonsStillCountPerCopy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "tb81-started")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 3)
	for i := 0; i < 3; i++ {
		insertCooldownCopy(t, pool, wu.ID, volA, "ABANDONED", true, 0)
	}
	if n, err := repo.CountTotalCopies(ctx, wu.ID); err != nil || n != 3 {
		t.Fatalf("CountTotalCopies after 3 started abandons by one volunteer = %d (err %v), want 3", n, err)
	}
	if n, err := repo.CountErrorCopies(ctx, wu.ID); err != nil || n != 3 {
		t.Fatalf("CountErrorCopies after 3 started abandons by one volunteer = %d (err %v), want 3", n, err)
	}
	failed, err := repo.DeadLetterIfExhausted(ctx, wu.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if !failed {
		t.Fatal("three started abandons must still exhaust a ceiling of 3")
	}
}

// TestTB81_OperatorReport_DeadEngineCannotDeadLetterUnitHoldingAResult replays one of
// the report's 22 units against the fix: a redundancy-3 leaf (ceiling 9), eight
// un-started abandons by the dead-engine machine, one COMPLETED copy from the fleet's
// best host with its result PENDING, and two give-backs. Pre-fix that was 9 billed of
// 9 — dead-lettered, the good result set aside as SUPERSEDED. After: 2 billed of 9,
// the unit stays QUEUED with its result PENDING, and a fresh volunteer can still take
// a copy.
func TestTB81_OperatorReport_DeadEngineCannotDeadLetterUnitHoldingAResult(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "tb81-report")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "",
		`{"redundancy_factor":3,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	deadEngine := createTestVolunteer(t, pool)
	bestHost := createTestVolunteer(t, pool)
	giverA, giverB := createTestVolunteer(t, pool), createTestVolunteer(t, pool)
	fresh := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	for i := 0; i < 8; i++ {
		insertUnstartedAbandon(t, pool, wu.ID, deadEngine)
	}
	insertClosedCopy(t, pool, wu.ID, bestHost, "COMPLETED")
	insertPendingResult(t, pool, wu.ID, bestHost)
	insertClosedCopy(t, pool, wu.ID, giverA, "RETURNED")
	insertClosedCopy(t, pool, wu.ID, giverB, "RETURNED")

	if n, err := repo.CountTotalCopies(ctx, wu.ID); err != nil || n != 2 {
		t.Fatalf("CountTotalCopies = %d (err %v), want 2: one dead-engine abandoner + one completed copy; give-backs free", n, err)
	}
	failed, err := repo.DeadLetterIfExhausted(ctx, wu.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if failed {
		t.Fatal("dead-lettered: one machine that cannot start the unit spent a nine-copy budget by itself (TB-81)")
	}
	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Fatalf("unit state = %s, want QUEUED", got.State)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT validation_status FROM results WHERE work_unit_id = $1 AND volunteer_id = $2`,
		wu.ID, bestHost).Scan(&status); err != nil {
		t.Fatalf("read result status: %v", err)
	}
	if status != "PENDING" {
		t.Fatalf("the good result's status = %s, want PENDING (the dead-letter must not have set it aside)", status)
	}
	if _, err := repo.ReserveCopy(ctx, wu.ID, fresh, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("a fresh volunteer must still be able to take a copy of the unit: %v", err)
	}
}
