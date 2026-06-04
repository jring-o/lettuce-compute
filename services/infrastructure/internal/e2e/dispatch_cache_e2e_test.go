//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/types"
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

// pollReservedVolunteer waits up to timeout for the unit's reserved_volunteer_id to be
// non-null (the async flush landed) and returns it.
func pollReservedVolunteer(t *testing.T, ctx context.Context, env *headsLeafsEnv, wuID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var reservedVol *string
		if err := env.pool.QueryRow(ctx,
			"SELECT reserved_volunteer_id::text FROM work_units WHERE id = $1", wuID).Scan(&reservedVol); err != nil {
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
		t.Fatalf("flushed reserved_volunteer_id = %q, want %s", reserved, volID)
	}
	if st := pollWorkUnitState(t, ctx, env, wuID, "QUEUED", time.Second); st != "QUEUED" {
		t.Fatalf("a flushed reservation must keep the unit QUEUED, got %q", st)
	}

	// StartWork flips QUEUED -> ASSIGNED and writes the active history row.
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
	if state != "ASSIGNED" {
		t.Fatalf("post-StartWork state = %q, want ASSIGNED", state)
	}
	if assignedAt == nil {
		t.Fatal("expected assigned_at set at run start")
	}
	var historyRows int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL",
		wuID, volID).Scan(&historyRows)
	if historyRows != 1 {
		t.Fatalf("expected 1 active history row after StartWork, got %d", historyRows)
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

// TestDispatchCache_RedundancyTwoFirstHandedSingleHolderSecondViaRedundantAssignment
// is the regression guard for the redundancy single-hold invariant. It does NOT
// assert that the dispatch cache drives concurrent redundant validation — it cannot,
// and that is a documented, pre-existing limitation (see the NOTE below).
//
// What this test DOES prove:
//   - The cache stages a NORMAL redundancy-2 unit to exactly ONE in-memory holder at
//     a time (mirroring the single live reserved_volunteer_id column the DB enforces)
//     and routes its flush as a single reservation, so the flush-conflict void-check
//     is unambiguous and no phantom in-memory holder leaks. A concurrent second
//     RequestWorkUnit for the same NORMAL unit gets nothing (asserted on respB).
//   - The cache does not hide, double-hand, or strand the unit: once the first holder
//     run-starts and submits, a second corroborating result still lands and validates.
//
// NOTE — redundancy>1 is NOT served concurrently through dispatch (pre-existing):
// StartWork's Assign flips the unit QUEUED->ASSIGNED, and BOTH the cache refill
// (FindDispatchableBatch) and the Layer-1 DB path (FindNextAssignable) gate on
// state='QUEUED'. So once the FIRST holder run-starts, the unit leaves the
// dispatchable universe and the SECOND distinct holder can never be reached by the
// cache OR the DB find concurrently. Concurrent dispatch of one unit to two
// volunteers is therefore unsupported until a per-volunteer dispatch table (out of
// scope, Layer 3). To exercise the second corroborating result here we inject it via
// createRedundantAssignment (a direct history-row INSERT that bypasses dispatch),
// exactly as the alpha_e2e redundancy tests do. This test asserts the cache does not
// break that existing non-dispatch redundancy flow; it is NOT proof of cache-driven
// redundant dispatch. See guides/head-setup.md "Redundancy and the dispatch cache".
//
// Before the fix the cache double-staged the same unit to two in-memory holders from
// one snapshot, both flushed into the single column, and the WorkUnitID-keyed
// void-check voided NEITHER — leaking a phantom holder that reconcile never cleared
// and (per the second holder's column miss) blocking the second result.
func TestDispatchCache_RedundancyTwoFirstHandedSingleHolderSecondViaRedundantAssignment(t *testing.T) {
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
	volBParsed := types.MustParseID(volB)

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

	// Vol A reserves the unit from the cache. The cache must serve it to exactly ONE
	// in-memory holder (volA): a single concurrent RequestWorkUnit by volB must NOT get
	// a second concurrent in-memory hold of the same NORMAL unit (that is the
	// double-stage the blocker described).
	wuA := reserveOne(pubA, volA)
	if wuA == "" {
		t.Fatal("vol A never got the redundancy-2 unit from the cache")
	}
	respB, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubB), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volB, PublicKey: pubB, LeafIds: []string{lf.ID.String()}, MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("vol B concurrent request: %v", err)
	}
	if len(respB.Assignments) != 0 {
		t.Fatalf("a NORMAL redundancy-2 unit must not be double-staged to a 2nd concurrent in-memory holder, vol B got %d", len(respB.Assignments))
	}

	// Vol A run-starts + submits its result (the flush must have landed its reservation).
	if got := pollReservedVolunteer(t, ctx, env, wuA, 5*time.Second); got != volA {
		t.Fatalf("flushed reserved_volunteer_id = %q, want %s", got, volA)
	}
	if _, err := env.grpc.StartWork(signFor(t, ctx, pubA), &lettucev1.StartWorkRequest{
		WorkUnitId: wuA, VolunteerId: volA,
	}); err != nil {
		t.Fatalf("StartWork(A): %v", err)
	}
	agreed := []byte(`{"result":"agree"}`)
	submit(pubA, volA, wuA, agreed)

	// The second corroborating volunteer is NOT reachable through dispatch: wuA is now
	// ASSIGNED (vol A run-started), and both the cache refill and the DB find gate on
	// QUEUED, so neither can hand it to vol B concurrently. We inject vol B's distinct
	// active history row directly, bypassing dispatch — the only way redundancy>1 lands
	// today (see the NOTE on this test and head-setup.md). This asserts the cache does
	// not break that existing non-dispatch redundancy flow.
	createRedundantAssignment(t, env.pool, ctx, wuA, volBParsed)
	submit(pubB, volB, wuA, agreed)

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

	// The reservation is in-memory-only: the long flush interval means the DB column is
	// still NULL. (Sanity check; not strictly required, but documents the window.)
	var reservedVol *string
	if err := env.pool.QueryRow(ctx,
		"SELECT reserved_volunteer_id::text FROM work_units WHERE id = $1", wuID).Scan(&reservedVol); err != nil {
		t.Fatalf("query reserved_volunteer_id: %v", err)
	}
	if reservedVol != nil && *reservedVol != "" {
		t.Logf("note: flush landed early (reserved_volunteer_id=%q); race window not exercised, but StartWork must still succeed", *reservedVol)
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

	if st := pollWorkUnitState(t, ctx, env, wuID, "ASSIGNED", 5*time.Second); st != "ASSIGNED" {
		t.Fatalf("post-StartWork state = %q, want ASSIGNED", st)
	}
	var historyRows int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL",
		wuID, volID).Scan(&historyRows)
	if historyRows != 1 {
		t.Fatalf("expected 1 active history row after StartWork in flush window, got %d", historyRows)
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
