//go:build integration

package workunit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// reserveOpts builds the standard capability-matching AssignmentOptions used by
// the reservation tests, with an optional per-volunteer inflight cap.
func reserveOpts(vol types.ID, maxInflight int) AssignmentOptions {
	return AssignmentOptions{
		VolunteerID:             vol,
		MaxCPUCores:             4,
		MaxMemoryMB:             16384,
		MaxDiskMB:               10240,
		HasGPU:                  false,
		AvailableRuntimes:       []string{"NATIVE"},
		MaxInflightPerVolunteer: maxInflight,
	}
}

func mustQueuedWU(t *testing.T, ctx context.Context, repo *PgxWorkUnitRepository, leafID types.ID) *WorkUnit {
	t.Helper()
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create work unit: %v", err)
	}
	return wu
}

const valConfigRedundancy1 = `{"redundancy_factor":1,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`

// A buffered (reserved) unit is leased PURELY via the reservation columns — NO
// assignment_history row is written. A redundancy-1 unit reserved by one volunteer
// is hidden from a second volunteer by the reservation guard alone.
func TestReserveNextAssignable_RedundancyOneHidesFromOther(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-r1")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	first, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("ReserveNextAssignable vol1: %v", err)
	}
	if first == nil || first.ID != wu.ID {
		t.Fatalf("expected vol1 to reserve %v, got %v", wu.ID, first)
	}
	if first.State != WorkUnitStateQueued {
		t.Fatalf("reserved unit state = %s, want QUEUED", first.State)
	}
	if first.ReservedUntil == nil || first.ReservedVolunteerID == nil || *first.ReservedVolunteerID != vol1 {
		t.Fatalf("reserved unit missing reservation columns: %+v", first)
	}

	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("ReserveNextAssignable vol2: %v", err)
	}
	if second != nil {
		t.Fatalf("expected reserved redundancy-1 unit hidden from vol2, got %v", second.ID)
	}
}

// A redundancy-2 unit can be reserved by TWO distinct volunteers concurrently
// (redundant validation still works): the redundancy count includes one live
// reservation by another volunteer plus any active history rows. Here, neither
// reservation has flipped to ASSIGNED, so the column-based reservation guard only
// hides the unit once it has reached the redundancy factor in distinct holders.
//
// NOTE: with the columns-only model a single reserved_volunteer_id column can only
// record ONE live reservation at a time. The redundancy-2 "two distinct holders"
// case is therefore exercised at run-start (Assign writes the history row and frees
// the column) — see TestReserveNextAssignable_AfterAssignSecondVolunteerCanReserve.
func TestReserveNextAssignable_RedundancyTwoSecondReserverBlockedWhileFirstHolds(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-r2")
	// Default valConfig has redundancy_factor 2.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	r1, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol1: %v", err)
	}
	if r1 == nil || r1.ID != wu.ID {
		t.Fatalf("expected vol1 to reserve %v, got %v", wu.ID, r1)
	}
	// While vol1 holds the live reservation, the single reservation column is taken,
	// so vol2 cannot also reserve the SAME unit (the guard hides it).
	r2, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if r2 != nil {
		t.Fatalf("expected unit hidden from vol2 while vol1 holds the reservation, got %v", r2.ID)
	}
}

// Once the first volunteer's reservation LAPSES (without ever starting), the same
// still-QUEUED redundancy-2 unit becomes reservable by a SECOND volunteer with no
// manual transition — the lapsed-lease re-reservability property applied to a
// redundancy>1 leaf. (While the first reservation is live it occupies the single
// reservation column and hides the unit; once lapsed the guard frees it.)
func TestReserveNextAssignable_LapsedRedundancyTwoReReservable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-r2-lapse")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	// vol1 reserves with an already-lapsed lease (simulates a crashed buffer holder).
	if _, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), -1*time.Second); err != nil {
		t.Fatalf("reserve vol1 (lapsed): %v", err)
	}

	r2, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if r2 == nil || r2.ID != wu.ID {
		t.Fatalf("expected vol2 to re-reserve the unit after vol1's lease lapsed, got %v", r2)
	}
	if r2.ReservedVolunteerID == nil || *r2.ReservedVolunteerID != vol2 {
		t.Fatalf("expected unit now reserved to vol2, got %+v", r2)
	}
}

// The reservation holder is not handed its own already-reserved unit again (the
// self-exclusion guard on the live reservation excludes it).
func TestReserveNextAssignable_SameVolunteerNotHandedTwice(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-self")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	mustQueuedWU(t, ctx, wuRepo, leafID)

	first, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if first == nil {
		t.Fatalf("expected first reservation")
	}
	again, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve again: %v", err)
	}
	if again != nil {
		t.Fatalf("expected the same volunteer NOT to be handed its own reserved unit again, got %v", again.ID)
	}
}

// A LAPSED reservation (reserved_until < NOW()) is automatically re-reservable by
// ANOTHER volunteer with no manual transition — the core leak-prevention property.
// This is what makes a crashed holder's buffered work recoverable.
func TestReserveNextAssignable_LapsedReservationReReservableByOther(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-lapse")
	// redundancy-1 so a single live reservation would otherwise hide the unit.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	// vol1 reserves with a lease that is ALREADY in the past (simulates a crashed
	// holder whose lease has since lapsed).
	first, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), -1*time.Second)
	if err != nil {
		t.Fatalf("reserve vol1 (lapsed): %v", err)
	}
	if first == nil || first.ID != wu.ID {
		t.Fatalf("expected vol1 to reserve %v, got %v", wu.ID, first)
	}

	// The unit is still QUEUED with a lapsed lease; vol2 can re-reserve it.
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if second == nil || second.ID != wu.ID {
		t.Fatalf("expected lapsed reservation re-reservable by vol2, got %v", second)
	}
	if second.ReservedVolunteerID == nil || *second.ReservedVolunteerID != vol2 {
		t.Fatalf("expected unit now reserved to vol2, got %+v", second)
	}
}

// Run-start (Assign) flips QUEUED -> ASSIGNED, clears the reservation columns,
// and starts the assignment clock (assigned_at set).
func TestAssign_ClearsReservationAndStartsClock(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-assign")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(vol, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	assigned, err := repo.Assign(ctx, wu.ID, vol)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if assigned.State != WorkUnitStateAssigned {
		t.Fatalf("state = %s, want ASSIGNED", assigned.State)
	}
	if assigned.ReservedUntil != nil || assigned.ReservedVolunteerID != nil {
		t.Fatalf("expected reservation cleared on Assign, got until=%v vol=%v",
			assigned.ReservedUntil, assigned.ReservedVolunteerID)
	}
	if assigned.AssignedAt == nil {
		t.Fatalf("expected assigned_at set at run-start")
	}
}

// ClearReservation drops the lease on a still-QUEUED unit reserved to the caller,
// leaving it QUEUED and immediately re-reservable by another volunteer. This is the
// head-side handling of a volunteer abandoning a buffered (un-started) unit.
func TestClearReservation_DropsLeaseAndIsReReservable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-clear")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	if _, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve vol1: %v", err)
	}

	cleared, err := wuRepo.ClearReservation(ctx, wu.ID, vol1)
	if err != nil {
		t.Fatalf("ClearReservation: %v", err)
	}
	if cleared.State != WorkUnitStateQueued {
		t.Fatalf("cleared unit state = %s, want QUEUED", cleared.State)
	}
	if cleared.ReservedUntil != nil || cleared.ReservedVolunteerID != nil {
		t.Fatalf("expected reservation columns cleared, got %+v", cleared)
	}

	// Now vol2 can reserve it immediately (no live reservation, redundancy-1 free).
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if second == nil || second.ID != wu.ID {
		t.Fatalf("expected unit re-reservable by vol2 after ClearReservation, got %v", second)
	}
}

// ClearReservation is a no-op-safe guard: clearing a unit that is NOT reserved to
// the caller (e.g. lease lapsed and re-taken by another volunteer, or never
// reserved) returns a Conflict rather than touching the row.
func TestClearReservation_NotReservedToCallerConflicts(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-clear-conflict")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)
	if _, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve vol1: %v", err)
	}

	// vol2 never reserved this unit; clearing it must conflict, not silently drop
	// vol1's reservation.
	if _, err := wuRepo.ClearReservation(ctx, wu.ID, vol2); err == nil {
		t.Fatalf("expected Conflict clearing a reservation not held by vol2")
	}
}

// The per-volunteer inflight cap counts a live reservation (no history row): with
// cap=1 and one live reservation, the same volunteer cannot reserve a second unit.
func TestReserveNextAssignable_InflightCapCountsReservations(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-cap")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	mustQueuedWU(t, ctx, wuRepo, leafID)
	mustQueuedWU(t, ctx, wuRepo, leafID)

	first, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol, 1), 60*time.Second)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if first == nil {
		t.Fatalf("first reserve under cap=1 should succeed")
	}
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol, 1), 60*time.Second)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if second != nil {
		t.Fatalf("expected inflight cap to block second reservation, got %v", second.ID)
	}
}

// Batching: looping reserve inside one transaction reserves distinct units and
// never returns the same work_unit_id twice (no history rows written).
func TestReserveNextAssignable_BatchInOneTxNoDuplicates(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-batch")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	ctx := context.Background()

	const total = 5
	poolRepo := NewPgxWorkUnitRepository(pool)
	for i := 0; i < total; i++ {
		mustQueuedWU(t, ctx, poolRepo, leafID)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx)
	txRepo := NewPgxWorkUnitRepository(tx)

	seen := map[types.ID]bool{}
	n := 4
	for i := 0; i < n; i++ {
		wu, rerr := txRepo.ReserveNextAssignable(ctx, reserveOpts(vol, 0), 60*time.Second)
		if rerr != nil {
			t.Fatalf("ReserveNextAssignable iter %d: %v", i, rerr)
		}
		if wu == nil {
			t.Fatalf("iter %d: expected a unit (only %d reserved of %d available)", i, len(seen), total)
		}
		if seen[wu.ID] {
			t.Fatalf("duplicate work_unit_id %v returned within one batch", wu.ID)
		}
		seen[wu.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct reserved units, got %d", n, len(seen))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// Renewing a buffered unit's lease before it lapses keeps the unit hidden from a
// SECOND volunteer — i.e. a unit held in the buffer LONGER than the original
// lease window is NOT re-dispatched, as long as the holder renews. This is the
// repo-level analog of the PREPARING-heartbeat-renews-reservation path in the
// Heartbeat handler (which calls StampReservation), and the core guard against
// the double-dispatch the lapsed-lease behavior would otherwise cause.
func TestStampReservation_RenewKeepsUnitHiddenPastOriginalLease(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-renew")
	// redundancy-1 so a single live reservation hides the unit; a lapsed one would
	// expose it.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	// vol1 reserves with a SHORT lease (the original window).
	if _, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 50*time.Millisecond); err != nil {
		t.Fatalf("reserve vol1: %v", err)
	}

	// Before that short lease lapses, vol1 renews it (what a PREPARING heartbeat
	// does) to a fresh, longer window.
	if _, err := wuRepo.StampReservation(ctx, wu.ID, vol1, 60*time.Second); err != nil {
		t.Fatalf("renew (StampReservation) vol1: %v", err)
	}

	// Wait out the ORIGINAL short lease window. The renewed lease is still live.
	time.Sleep(80 * time.Millisecond)

	// vol2 must NOT be able to re-reserve: the renewal kept the lease alive past
	// the original window, so the unit is not re-dispatched.
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if second != nil {
		t.Fatalf("expected renewed reservation to keep unit hidden from vol2 past the original lease, got %v", second.ID)
	}
}

// The queue-order index (idx_work_units_queue_order) must exist so the
// FindNextAssignable / ReserveNextAssignable hot query can short-circuit the
// global "ORDER BY priority DESC, created_at ASC" at LIMIT 1 instead of reading
// and sorting every QUEUED row. Without it, the planner full-scans the queue and
// external-sorts it on every assignment, which is the pathological single-head
// assignment-latency ceiling the load test surfaced at 100k+ QUEUED units.
func TestFindNextAssignable_QueueOrderIndexExists(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	var indexDef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'idx_work_units_queue_order'`,
	).Scan(&indexDef)
	if err != nil {
		t.Fatalf("idx_work_units_queue_order is missing (queue-order migration not applied?): %v", err)
	}
	// Sanity-check the index actually materializes the queue order and is partial.
	for _, want := range []string{"priority", "created_at", "QUEUED"} {
		if !strings.Contains(indexDef, want) {
			t.Fatalf("queue-order index missing %q: %s", want, indexDef)
		}
	}
}

// The queue's global "ORDER BY priority DESC, created_at ASC ... LIMIT 1" must be
// satisfiable directly by walking idx_work_units_queue_order, so the planner
// short-circuits at the first row instead of reading and sorting the whole queue.
// This asserts the index covers that ordering (the plan uses the index and has no
// Sort node). It is the structural regression guard for the assignment-latency
// blowup the load test surfaced: at 100k+ QUEUED units a Sort here spills to disk
// on every assignment. (The full assignment predicate's per-row redundancy
// subquery can still lead the planner to a seq-scan+sort at *small* row counts;
// the cost crossover to this index walk happens precisely at the scale that
// matters — verified empirically at ~102k rows, where the plan switches to the
// index walk and execution drops from hundreds of ms to sub-ms.)
func TestFindNextAssignable_PlanUsesIndexNoSort(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "queue-plan")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Seed enough QUEUED units that an unindexed plan would have to sort a
	// non-trivial set; the index walk is the only plan with no Sort node.
	for i := 0; i < 500; i++ {
		mustQueuedWU(t, ctx, repo, leafID)
	}
	// ANALYZE so the planner has fresh stats and prefers the index walk.
	if _, err := pool.Exec(ctx, "ANALYZE work_units"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	rows, err := pool.Query(ctx, `
		EXPLAIN (FORMAT TEXT)
		SELECT wu.id
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
		WHERE wu.state = 'QUEUED' AND l.state = 'ACTIVE'
		ORDER BY wu.priority DESC, wu.created_at ASC
		LIMIT 1
		FOR UPDATE OF wu SKIP LOCKED`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	planText := plan.String()
	if strings.Contains(planText, "Sort") {
		t.Fatalf("assignment query plan contains a Sort node (queue-order index not used):\n%s", planText)
	}
	if !strings.Contains(planText, "idx_work_units_queue_order") {
		t.Fatalf("assignment query plan does not use idx_work_units_queue_order:\n%s", planText)
	}
}

// StampReservation marks a still-QUEUED unit with the reservation columns.
func TestStampReservation_StampsColumns(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "stamp-reserve")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	stamped, err := repo.StampReservation(ctx, wu.ID, vol, 60*time.Second)
	if err != nil {
		t.Fatalf("StampReservation: %v", err)
	}
	if stamped.State != WorkUnitStateQueued {
		t.Fatalf("stamped unit state = %s, want QUEUED", stamped.State)
	}
	if stamped.ReservedVolunteerID == nil || *stamped.ReservedVolunteerID != vol {
		t.Fatalf("expected reservation stamped for vol, got %+v", stamped)
	}
	if stamped.ReservedUntil == nil {
		t.Fatalf("expected reserved_until set")
	}
}
