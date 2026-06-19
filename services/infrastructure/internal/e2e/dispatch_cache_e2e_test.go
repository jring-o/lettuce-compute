//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/server"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// pollWorkUnitState waits up to timeout for the given work unit to reach wantState,
// polling the DB. The dispatch cache flushes reservations asynchronously, so a test
// that reserved a unit via the cache must wait for the flush to land before reading
// its reservation columns. Returns the observed state (== wantState on success).
func pollWorkUnitState(t *testing.T, ctx context.Context, env *headsLeafsEnv, wuID, wantState string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var state string
	for time.Now().Before(deadline) {
		if err := env.pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&state); err != nil {
			t.Fatalf("pollWorkUnitState: query %s: %v", wuID, err)
		}
		if state == wantState {
			return state
		}
		time.Sleep(50 * time.Millisecond)
	}
	return state
}

// pollReservedVolunteer waits up to timeout for a live RESERVED copy of the unit to
// land in work_unit_assignment_history (the async flush wrote it) and returns its
// volunteer_id. Per-copy model (migration 00006): the single per-unit
// reserved_volunteer_id column was retired; a hold is now a RESERVED copy row
// (outcome IS NULL, started_at IS NULL, reserved_until set). The scalar subquery
// yields NULL (not ErrNoRows) while no such row exists, so the poll loop keeps the
// original nil-keep-polling control flow.
func pollReservedVolunteer(t *testing.T, ctx context.Context, env *headsLeafsEnv, wuID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var reservedVol *string
		if err := env.pool.QueryRow(ctx,
			`SELECT (SELECT volunteer_id::text FROM work_unit_assignment_history
			         WHERE work_unit_id = $1 AND outcome IS NULL AND started_at IS NULL
			         ORDER BY assigned_at DESC LIMIT 1)`, wuID).Scan(&reservedVol); err != nil {
			t.Fatalf("pollReservedVolunteer: query %s: %v", wuID, err)
		}
		if reservedVol != nil && *reservedVol != "" {
			return *reservedVol
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// TestDispatchCache_ServeFlushStartSubmit drives the Layer-2 dispatch cache end to end
// against real Postgres: RequestWorkUnit serves distinct units from the in-memory
// cache (hot path off Postgres), the async flusher writes the reservations while the
// unit stays state='QUEUED', StartWork flips the unit to ASSIGNED + writes a history
// row, and SubmitResult completes only after StartWork. This is the integration
// coverage the cache lacked: every other gRPC integration test exercises the
// requestWorkUnitFromDB fallback because the cache was never started.
func TestDispatchCache_ServeFlushStartSubmit(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "cache-serve")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Cache Serve Leaf"))
	generateLeafWUs(t, env, lf.ID, 6)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "cache-vol")

	// Request a batch from the cache. (The refiller primes the pool on start; retry a
	// few times so a just-generated batch has time to be staged.)
	var asg *lettucev1.WorkUnitAssignment
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 4,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (cache): %v", err)
		}
		if len(resp.Assignments) > 0 {
			// No duplicate work_unit_id within a batch.
			seen := map[string]bool{}
			for _, a := range resp.Assignments {
				if seen[a.WorkUnitId] {
					t.Fatalf("duplicate work_unit_id %s in one cache-served batch", a.WorkUnitId)
				}
				seen[a.WorkUnitId] = true
			}
			asg = resp.Assignments[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if asg == nil {
		t.Fatal("cache never served a work unit (refiller did not stage the batch)")
	}
	wuID := asg.WorkUnitId

	// The async flush must land the reservation while the unit stays QUEUED.
	reserved := pollReservedVolunteer(t, ctx, env, wuID, 5*time.Second)
	if reserved != volID {
		t.Fatalf("flushed RESERVED copy volunteer = %q, want %s", reserved, volID)
	}
	if st := pollWorkUnitState(t, ctx, env, wuID, "QUEUED", time.Second); st != "QUEUED" {
		t.Fatalf("a flushed reservation must keep the unit QUEUED, got %q", st)
	}

	// StartWork run-starts THIS volunteer's copy (RESERVED -> RUNNING). Per-copy model:
	// the WORK UNIT stays QUEUED (a pure aggregate) so its other redundancy copies keep
	// dispatching in parallel; only the copy gains started_at. assigned_at on the unit
	// is still updated best-effort for observability.
	if _, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
		WorkUnitId: wuID, VolunteerId: volID,
	}); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
	var state string
	var assignedAt *time.Time
	if err := env.pool.QueryRow(ctx,
		"SELECT state, assigned_at FROM work_units WHERE id = $1", wuID).Scan(&state, &assignedAt); err != nil {
		t.Fatalf("query post-StartWork: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("post-StartWork unit state = %q, want QUEUED (per-copy: the unit stays QUEUED, only the copy runs)", state)
	}
	if assignedAt == nil {
		t.Fatal("expected assigned_at set at run start")
	}
	// The flushed RESERVED copy is now a RUNNING copy (started_at set), still the one
	// live (outcome IS NULL) copy for this volunteer.
	var historyRows int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL AND started_at IS NOT NULL",
		wuID, volID).Scan(&historyRows)
	if historyRows != 1 {
		t.Fatalf("expected 1 running history row after StartWork, got %d", historyRows)
	}

	// SubmitResult completes the unit.
	out := []byte(`{"result":"cache_ok"}`)
	if _, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volID, PublicKey: pubKey,
		OutputData: out, OutputChecksumSha256: sha256Hex(out),
		Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
	}); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if st := pollWorkUnitState(t, ctx, env, wuID, "VALIDATED", 5*time.Second); st != "VALIDATED" {
		t.Fatalf("post-Submit state = %q, want VALIDATED", st)
	}
}

// TestDispatchCache_RedundancyTwoDispatchesParallelCopiesToDistinctVolunteers
// asserts the per-copy parallel-dispatch invariant (migration 00006): a NORMAL
// redundancy-2 unit is dispatched as TWO copies IN PARALLEL to two DISTINCT
// volunteers, both held concurrently while the unit STAYS QUEUED.
//
// What this test proves:
//   - The cache stages the redundancy-2 unit to vol A AND, concurrently, hands a
//     SECOND distinct copy of the SAME unit to vol B (the parallel-copy case). Each
//     lands as its own RESERVED copy row (distinct volunteer_id, outcome IS NULL); the
//     unit never leaves QUEUED while the copies run.
//   - Both copies run-start and submit agreeing results, and the unit validates with
//     no phantom hold leaked and no copy stranded.
//
// INVERTED from the previous per-unit model: that earlier test asserted a concurrent
// second RequestWorkUnit got ZERO assignments (redundancy was a single live
// reserved_volunteer_id column, served to one holder; the second corroborator had to
// be injected via a direct history-row INSERT because StartWork flipped the unit
// QUEUED->ASSIGNED, removing it from the dispatchable universe). Under the per-copy
// model the redundancy guard counts LIVE COPIES (outcome IS NULL) against the leaf's
// redundancy_factor and the unit stays QUEUED, so the second distinct volunteer is
// served a parallel copy directly by dispatch.
func TestDispatchCache_RedundancyTwoDispatchesParallelCopiesToDistinctVolunteers(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "cache-redundancy")
	opts := hlDefaultLeafOpts("Cache Redundancy Leaf")
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 1)

	pubA := genVolunteerKey(t)
	pubB := genVolunteerKey(t)
	volA := registerHLVolunteer(t, env, ctx, pubA, "cache-red-A")
	volB := registerHLVolunteer(t, env, ctx, pubB, "cache-red-B")

	// reserveOne requests one unit from the cache for the given volunteer, retrying so
	// the refiller has time to stage the unit.
	reserveOne := func(pub []byte, vol string) string {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pub), &lettucev1.RequestWorkUnitRequest{
				VolunteerId: vol, PublicKey: pub, LeafIds: []string{lf.ID.String()}, MaxAssignments: 1,
			})
			if err != nil {
				t.Fatalf("RequestWorkUnit(%s): %v", vol, err)
			}
			if len(resp.Assignments) == 1 {
				return resp.Assignments[0].WorkUnitId
			}
			time.Sleep(150 * time.Millisecond)
		}
		return ""
	}

	submit := func(pub []byte, vol, wuID string, out []byte) {
		if _, err := env.grpc.SubmitResult(signFor(t, ctx, pub), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuID, VolunteerId: vol, PublicKey: pub,
			OutputData: out, OutputChecksumSha256: sha256Hex(out),
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
		}); err != nil {
			t.Fatalf("SubmitResult(%s): %v", vol, err)
		}
	}

	// Vol A reserves a copy of the redundancy-2 unit from the cache.
	wuA := reserveOne(pubA, volA)
	if wuA == "" {
		t.Fatal("vol A never got the redundancy-2 unit from the cache")
	}

	// Per-copy parallel dispatch: a concurrent RequestWorkUnit by the DISTINCT vol B
	// must get a SECOND copy of the SAME unit — the unit stays QUEUED with one live copy,
	// still below redundancy_factor=2, so the redundancy guard keeps it dispatchable to a
	// second distinct volunteer. reserveOne retries so vol A's in-memory hold has time to
	// flush its RESERVED copy row before the guard re-evaluates.
	wuB := reserveOne(pubB, volB)
	if wuB == "" {
		t.Fatal("vol B was not served a parallel copy of the redundancy-2 unit (per-copy parallel dispatch broken)")
	}
	if wuB != wuA {
		t.Fatalf("vol B got a different unit %s, want the same unit %s dispatched in parallel", wuB, wuA)
	}

	// Two distinct live copies of the one unit now exist concurrently (the parallel-copy
	// case), and the unit is still QUEUED.
	if got := pollReservedVolunteer(t, ctx, env, wuA, 5*time.Second); got == "" {
		t.Fatal("no flushed RESERVED copy landed for the redundancy-2 unit")
	}
	var liveCopies int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(DISTINCT volunteer_id) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
		wuA).Scan(&liveCopies); err != nil {
		t.Fatalf("count live copies: %v", err)
	}
	if liveCopies != 2 {
		t.Fatalf("expected 2 distinct live copies of the unit (parallel dispatch), got %d", liveCopies)
	}
	if st := pollWorkUnitState(t, ctx, env, wuA, "QUEUED", time.Second); st != "QUEUED" {
		t.Fatalf("unit must stay QUEUED while its two copies run, got %q", st)
	}

	// Both volunteers run-start their copy (RESERVED -> RUNNING; the unit stays QUEUED)
	// and submit agreeing results.
	agreed := []byte(`{"result":"agree"}`)
	for _, h := range []struct {
		pub []byte
		vol string
	}{{pubA, volA}, {pubB, volB}} {
		if _, err := env.grpc.StartWork(signFor(t, ctx, h.pub), &lettucev1.StartWorkRequest{
			WorkUnitId: wuA, VolunteerId: h.vol,
		}); err != nil {
			t.Fatalf("StartWork(%s): %v", h.vol, err)
		}
		submit(h.pub, h.vol, wuA, agreed)
	}

	// Two agreeing results -> the unit validates. (If the cache had leaked a phantom
	// hold or stranded the unit, the second result could not have completed it.)
	if st := pollWorkUnitState(t, ctx, env, wuA, "VALIDATED", 10*time.Second); st != "VALIDATED" {
		var results int
		env.pool.QueryRow(ctx, "SELECT COUNT(*) FROM results WHERE work_unit_id = $1", wuA).Scan(&results)
		t.Fatalf("redundancy-2 unit state = %q, want VALIDATED (results landed=%d)", st, results)
	}
}

// TestDispatchCache_StartWorkBeforeFlush is the Major-3 (flush-race) regression guard.
// With the flusher's interval set very long, a hand-out's reservation is held only
// in memory (the DB reserved_volunteer_id stays NULL) when the volunteer immediately
// run-starts. Before the fix StartWork found the unit plain QUEUED with a NULL
// reserved_volunteer_id and returned ok=false, so the volunteer dropped the unit
// without submitting (100% drop in the buffered/instant-drain profile). After the fix
// StartWork consults the cache's in-memory reservation, forces the pending flush, and
// completes the run-start.
func TestDispatchCache_StartWorkBeforeFlush(t *testing.T) {
	// Flush interval 10 minutes: the periodic flush will NOT fire during the test, so a
	// freshly handed-out reservation is in-memory-only until StartWork forces it.
	env, cleanup := setupHeadsLeafsServerWithCacheCfg(t, server.HeadDispatchConfig{FlushIntervalMs: 600000})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "cache-flushrace")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Cache FlushRace Leaf"))
	generateLeafWUs(t, env, lf.ID, 4)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "cache-flushrace-vol")

	var wuID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (cache): %v", err)
		}
		if len(resp.Assignments) > 0 {
			wuID = resp.Assignments[0].WorkUnitId
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wuID == "" {
		t.Fatal("cache never served a work unit")
	}

	// The reservation is in-memory-only: the long flush interval means NO RESERVED copy
	// row has landed yet. (Sanity check; not strictly required, but documents the
	// window.) Per-copy model: the hold would land as a work_unit_assignment_history row,
	// not a work_units column.
	var reservedVol *string
	if err := env.pool.QueryRow(ctx,
		`SELECT (SELECT volunteer_id::text FROM work_unit_assignment_history
		         WHERE work_unit_id = $1 AND outcome IS NULL AND started_at IS NULL
		         ORDER BY assigned_at DESC LIMIT 1)`, wuID).Scan(&reservedVol); err != nil {
		t.Fatalf("query reserved copy: %v", err)
	}
	if reservedVol != nil && *reservedVol != "" {
		t.Logf("note: flush landed early (reserved copy volunteer=%q); race window not exercised, but StartWork must still succeed", *reservedVol)
	}

	// StartWork immediately after hand-out, inside the flush window, must succeed: it
	// consults the in-memory reservation and forces the flush.
	resp, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
		WorkUnitId: wuID, VolunteerId: volID,
	})
	if err != nil {
		t.Fatalf("StartWork in flush window: %v", err)
	}
	if !resp.Ok {
		t.Fatalf("StartWork in flush window returned ok=false (%q): the flush-race drop bug is not fixed", resp.Message)
	}

	// Per-copy model: run-start flips the copy to RUNNING but the unit stays QUEUED.
	if st := pollWorkUnitState(t, ctx, env, wuID, "QUEUED", 5*time.Second); st != "QUEUED" {
		t.Fatalf("post-StartWork unit state = %q, want QUEUED (only the copy runs)", st)
	}
	var historyRows int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL AND started_at IS NOT NULL",
		wuID, volID).Scan(&historyRows)
	if historyRows != 1 {
		t.Fatalf("expected 1 running history row after StartWork in flush window, got %d", historyRows)
	}

	// And the unit can be submitted (the volunteer no longer drops it).
	out := []byte(`{"result":"flushrace_ok"}`)
	if _, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volID, PublicKey: pubKey,
		OutputData: out, OutputChecksumSha256: sha256Hex(out),
		Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
	}); err != nil {
		t.Fatalf("SubmitResult after flush-window StartWork: %v", err)
	}
	if st := pollWorkUnitState(t, ctx, env, wuID, "VALIDATED", 5*time.Second); st != "VALIDATED" {
		t.Fatalf("post-Submit state = %q, want VALIDATED", st)
	}
}
