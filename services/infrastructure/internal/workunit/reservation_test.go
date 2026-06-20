//go:build integration

package workunit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// liveCopyVolunteers returns the volunteer ids of a unit's LIVE (outcome IS NULL)
// copies, ordered, for per-copy assertions. With per-copy dispatch a unit can hold
// up to redundancy live copies, each by a DISTINCT volunteer.
func liveCopyVolunteers(t *testing.T, pool *pgxpool.Pool, wuID types.ID) []types.ID {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT volunteer_id FROM work_unit_assignment_history
		 WHERE work_unit_id = $1 AND outcome IS NULL
		 ORDER BY volunteer_id`, wuID)
	if err != nil {
		t.Fatalf("query live copies: %v", err)
	}
	defer rows.Close()
	var out []types.ID
	for rows.Next() {
		var v types.ID
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan live copy volunteer: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func containsID(ids []types.ID, want types.ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// A redundancy-1 unit reserved by one volunteer (a single live RESERVED copy) is
// hidden from a second volunteer: the live-copy count has reached the redundancy.
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
		t.Fatalf("reserved unit missing transient reservation echo: %+v", first)
	}
	// Exactly one live copy, held by vol1.
	if vols := liveCopyVolunteers(t, pool, wu.ID); len(vols) != 1 || vols[0] != vol1 {
		t.Fatalf("expected one live copy held by vol1, got %v", vols)
	}

	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("ReserveNextAssignable vol2: %v", err)
	}
	if second != nil {
		t.Fatalf("expected reserved redundancy-1 unit hidden from vol2, got %v", second.ID)
	}
}

// A redundancy-2 unit can be reserved by TWO distinct volunteers CONCURRENTLY: each
// gets its own live RESERVED copy (the parallel-copy case, property 7). A third
// volunteer is then blocked because the live-copy count has reached redundancy.
func TestReserveNextAssignable_RedundancyTwoParallelReservers(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-r2")
	// Default valConfig has redundancy_factor 2.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	vol3 := createTestVolunteer(t, pool)
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
	// A SECOND distinct volunteer reserves the SAME unit in parallel (redundancy 2).
	r2, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if r2 == nil || r2.ID != wu.ID {
		t.Fatalf("expected vol2 to reserve a parallel copy of %v, got %v", wu.ID, r2)
	}

	vols := liveCopyVolunteers(t, pool, wu.ID)
	if len(vols) != 2 || !containsID(vols, vol1) || !containsID(vols, vol2) {
		t.Fatalf("expected two distinct live copies (vol1+vol2), got %v", vols)
	}

	// Redundancy is now met: a THIRD volunteer is blocked.
	r3, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol3, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol3: %v", err)
	}
	if r3 != nil {
		t.Fatalf("expected unit hidden from vol3 once redundancy 2 is met, got %v", r3.ID)
	}
}

// A lapsed RESERVED copy (reserved_until in the past) is NOT automatically
// re-reservable in the per-copy model: while its outcome is still NULL it counts as
// a live copy and keeps the redundancy slot taken. The slot frees only once the
// expiry sweep CLOSES the copy (FindExpiredCopies → CloseCopy).
func TestReserveNextAssignable_LapsedCopyStillBlocksUntilClosed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-lapse")
	// redundancy-1 so a single live copy hides the unit.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	wuRepo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, wuRepo, leafID)

	if _, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol1, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve vol1: %v", err)
	}
	// Lapse vol1's copy: reserved_until in the past, but outcome still NULL (live).
	if _, err := pool.Exec(ctx,
		`UPDATE work_unit_assignment_history SET reserved_until = NOW() - INTERVAL '1 second'
		 WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`, wu.ID, vol1); err != nil {
		t.Fatalf("lapse vol1 copy: %v", err)
	}

	// vol2 is STILL blocked: the lapsed-but-open copy keeps the redundancy slot.
	blocked, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2 (pre-close): %v", err)
	}
	if blocked != nil {
		t.Fatalf("a lapsed-but-open copy must still block re-reservation, got %v", blocked.ID)
	}

	// The expiry sweep finds and closes the lapsed copy.
	expired, err := wuRepo.FindExpiredCopies(ctx, 100)
	if err != nil {
		t.Fatalf("FindExpiredCopies: %v", err)
	}
	if len(expired) != 1 || expired[0].WorkUnitID != wu.ID {
		t.Fatalf("expected the lapsed copy of %v, got %v", wu.ID, expired)
	}
	if err := wuRepo.CloseCopy(ctx, expired[0].ID, "ABANDONED"); err != nil {
		t.Fatalf("CloseCopy: %v", err)
	}

	// Now the slot is free: vol2 can reserve.
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2 (post-close): %v", err)
	}
	if second == nil || second.ID != wu.ID {
		t.Fatalf("expected unit re-reservable by vol2 after the lapsed copy was closed, got %v", second)
	}
	if second.ReservedVolunteerID == nil || *second.ReservedVolunteerID != vol2 {
		t.Fatalf("expected unit now reserved to vol2, got %+v", second)
	}
}

// The reservation holder is not handed its own already-reserved unit again (the
// self-exclusion guard on the volunteer's live copy excludes it).
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

// Run-start (Assign) flips the volunteer's RESERVED copy to RUNNING (started_at set)
// and starts the per-copy deadline clock. The WORK UNIT stays QUEUED — it is a pure
// aggregate over its copies, so its other redundancy copies keep dispatching.
func TestAssign_ReservedCopyRunStartsAndStaysQueued(t *testing.T) {
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
	if assigned.State != WorkUnitStateQueued {
		t.Fatalf("state = %s, want QUEUED (unit stays QUEUED through run-start)", assigned.State)
	}
	if assigned.AssignedVolunteerID == nil || *assigned.AssignedVolunteerID != vol {
		t.Fatalf("expected denormalized assigned_volunteer_id = vol, got %v", assigned.AssignedVolunteerID)
	}
	if assigned.AssignedAt == nil {
		t.Fatalf("expected assigned_at set at run-start")
	}

	// The volunteer's copy is now RUNNING (started_at set).
	var startedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT started_at FROM work_unit_assignment_history
		 WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`, wu.ID, vol).Scan(&startedAt); err != nil {
		t.Fatalf("read copy started_at: %v", err)
	}
	if startedAt == nil {
		t.Fatalf("expected the copy's started_at set at run-start")
	}
}

// Abandoning a volunteer's buffered (un-started) copy via CloseCopyByVolunteer
// frees the unit's redundancy slot, leaving it QUEUED and immediately re-reservable
// by another volunteer. This is the head-side handling of a volunteer dropping a
// buffered unit (the per-copy replacement for the old ClearReservation path).
func TestCloseCopyByVolunteer_AbandonBufferedCopyReReservable(t *testing.T) {
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

	if err := wuRepo.CloseCopyByVolunteer(ctx, wu.ID, vol1, "ABANDONED", nil); err != nil {
		t.Fatalf("CloseCopyByVolunteer: %v", err)
	}
	// No live copy remains.
	if vols := liveCopyVolunteers(t, pool, wu.ID); len(vols) != 0 {
		t.Fatalf("expected no live copies after abandon, got %v", vols)
	}

	// Now vol2 can reserve it immediately (redundancy-1 free again).
	second, err := wuRepo.ReserveNextAssignable(ctx, reserveOpts(vol2, 0), 60*time.Second)
	if err != nil {
		t.Fatalf("reserve vol2: %v", err)
	}
	if second == nil || second.ID != wu.ID {
		t.Fatalf("expected unit re-reservable by vol2 after abandon, got %v", second)
	}
}

// CloseCopyByVolunteer is a no-op-safe guard: closing a copy for a volunteer that
// holds NO live copy of the unit returns a Conflict rather than touching another
// volunteer's copy.
func TestCloseCopyByVolunteer_NoLiveCopyConflicts(t *testing.T) {
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

	// vol2 never reserved this unit; closing its (nonexistent) copy must conflict,
	// not silently drop vol1's copy.
	if err := wuRepo.CloseCopyByVolunteer(ctx, wu.ID, vol2, "ABANDONED", nil); err == nil {
		t.Fatalf("expected Conflict closing a copy not held by vol2")
	}
}

// ReserveCopy inserts a RESERVED copy (a buffered work_unit_assignment_history row,
// outcome NULL / started_at NULL) on a still-QUEUED unit.
func TestReserveCopy_InsertsReservedRow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-copy")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	vol := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	cp, err := repo.ReserveCopy(ctx, wu.ID, vol, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds)
	if err != nil {
		t.Fatalf("ReserveCopy: %v", err)
	}
	if cp.WorkUnitID != wu.ID || cp.VolunteerID != vol {
		t.Fatalf("copy identity mismatch: %+v", cp)
	}
	if cp.StartedAt != nil || cp.Outcome != nil {
		t.Fatalf("a fresh reserved copy must be RESERVED (started_at/outcome NULL), got %+v", cp)
	}
	if cp.State() != CopyStateReserved {
		t.Fatalf("copy state = %s, want RESERVED", cp.State())
	}
	if cp.DeadlineSeconds != wu.DeadlineSeconds {
		t.Fatalf("deadline_seconds = %d, want %d", cp.DeadlineSeconds, wu.DeadlineSeconds)
	}

	// The unit row itself stays QUEUED.
	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Fatalf("unit state = %s, want QUEUED", got.State)
	}

	// A second ReserveCopy for the SAME volunteer conflicts (one live copy per
	// volunteer per unit — the partial unique).
	if _, err := repo.ReserveCopy(ctx, wu.ID, vol, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
		t.Fatalf("expected Conflict reserving a second live copy for the same volunteer")
	}
}

// The per-volunteer inflight cap counts a live copy: with cap=1 and one live copy,
// the same volunteer cannot reserve a second unit.
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
// never returns the same work_unit_id twice (each prior live copy excludes its unit
// from the same volunteer).
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

// --- per-volunteer distinctness + cooldown (authoritative copy-creation gates) ---

// insertPendingResult inserts a minimal PENDING result row for (wuID, vol) — the
// durable marker that this volunteer has already contributed a result to the unit.
func insertPendingResult(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO results
			(work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
		VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, 'PENDING')`,
		wuID, vol, strings.Repeat("a", 64),
	)
	if err != nil {
		t.Fatalf("insert pending result: %v", err)
	}
}

func containsFlushedPair(landed []FlushedCopy, wuID, vol types.ID) bool {
	for _, fc := range landed {
		if fc.WorkUnitID == wuID && fc.VolunteerID == vol {
			return true
		}
	}
	return false
}

// ReserveCopy is the authoritative copy-creation gate: it must refuse a volunteer that
// already authored a PENDING result for the unit (so each of the N redundant results
// comes from a DISTINCT volunteer), while still letting a fresh volunteer reserve the
// corroborating copy. This is the durable backstop behind the in-memory hand-out filter.
func TestReserveCopy_RefusesPriorResultAuthor(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-distinct")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	insertPendingResult(t, pool, wu.ID, volA)

	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
		t.Fatalf("ReserveCopy must refuse a volunteer that already submitted a result for this unit")
	}
	if _, err := repo.ReserveCopy(ctx, wu.ID, volB, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy must allow a distinct corroborator: %v", err)
	}
}

// ReserveCopy must bench a volunteer whose recent copy of the unit EXPIRED/was abandoned
// (post-failure cooldown ~one deadline) so a fresh volunteer gets first crack, while
// still allowing that fresh volunteer.
func TestReserveCopy_RefusesBenchedVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-cooldown")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	// volA reserves, then its copy expires — benching it for ~one deadline window.
	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "EXPIRED", nil); err != nil {
		t.Fatalf("close volA copy EXPIRED: %v", err)
	}

	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
		t.Fatalf("ReserveCopy must bench a volunteer whose copy just expired")
	}
	if _, err := repo.ReserveCopy(ctx, wu.ID, volB, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy must allow a fresh volunteer during another's cooldown: %v", err)
	}
}

// FlushReservations is the production hand-out landing path. It must SKIP (not land) a
// reservation for a volunteer that already authored a PENDING result for the unit while
// landing a DISTINCT corroborator — so a unit re-queued for corroboration is never
// re-dispatched to its own prior submitter (the live regression). A skipped record is
// absent from the returned landed set, so the cache voids that hold before any run-start.
func TestFlushReservations_SkipsPriorResultAuthor(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "flush-distinct")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	insertPendingResult(t, pool, wu.ID, volA)

	until := time.Now().UTC().Add(time.Hour)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: wu.ID, VolunteerID: volA, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
		{WorkUnitID: wu.ID, VolunteerID: volB, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if containsFlushedPair(landed, wu.ID, volA) {
		t.Fatalf("prior result author volA must NOT land a copy")
	}
	if !containsFlushedPair(landed, wu.ID, volB) {
		t.Fatalf("distinct corroborator volB must land a copy")
	}
}

// FlushReservations must also SKIP a volunteer benched by a recent EXPIRED/abandoned copy
// of the unit, while landing a fresh volunteer.
func TestFlushReservations_SkipsBenchedVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "flush-cooldown")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "EXPIRED", nil); err != nil {
		t.Fatalf("close volA copy EXPIRED: %v", err)
	}

	until := time.Now().UTC().Add(time.Hour)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: wu.ID, VolunteerID: volA, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
		{WorkUnitID: wu.ID, VolunteerID: volB, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if containsFlushedPair(landed, wu.ID, volA) {
		t.Fatalf("benched volA must NOT land a copy during cooldown")
	}
	if !containsFlushedPair(landed, wu.ID, volB) {
		t.Fatalf("fresh volB must land a copy")
	}
}
