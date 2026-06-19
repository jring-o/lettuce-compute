//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestBatchDispatch_ReturnsDistinctUnitsAndDelay verifies the Layer-1 head
// dispatch contract end to end through the gRPC service:
//   - a single RequestWorkUnit with max_assignments=N returns up to N distinct
//     work units (no duplicate work_unit_id within one batch),
//   - every reply carries a server-directed retry_after_seconds >= 1,
//   - each returned assignment carries a reserved_until_unix lease in the future,
//   - the no-work path is an OK response with empty assignments + a delay.
func TestBatchDispatch_ReturnsDistinctUnitsAndDelay(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "batch-distinct")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Batch Distinct Leaf"))
	generateLeafWUs(t, env, lf.ID, 5)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "batch-vol")

	resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID,
		PublicKey:      pubKey,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 4,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit (batch): %v", err)
	}
	if len(resp.Assignments) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(resp.Assignments))
	}
	if resp.RetryAfterSeconds < 1 {
		t.Errorf("expected retry_after_seconds >= 1, got %d", resp.RetryAfterSeconds)
	}

	seen := map[string]bool{}
	nowUnix := time.Now().Unix()
	for _, a := range resp.Assignments {
		if seen[a.WorkUnitId] {
			t.Fatalf("duplicate work_unit_id %s in one batch", a.WorkUnitId)
		}
		seen[a.WorkUnitId] = true
		if a.ReservedUntilUnix <= nowUnix {
			t.Errorf("assignment %s reserved_until_unix=%d not in the future (now=%d)",
				a.WorkUnitId, a.ReservedUntilUnix, nowUnix)
		}
	}

	// A second volunteer cannot see the 4 reserved units; only the 1 remaining
	// QUEUED unit is assignable to it.
	pubKey2 := genVolunteerKey(t)
	volID2 := registerHLVolunteer(t, env, ctx, pubKey2, "batch-vol2")
	resp2, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey2), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID2,
		PublicKey:      pubKey2,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 4,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit (vol2): %v", err)
	}
	if len(resp2.Assignments) != 1 {
		t.Fatalf("expected vol2 to get only the 1 unreserved unit, got %d", len(resp2.Assignments))
	}
	if seen[resp2.Assignments[0].WorkUnitId] {
		t.Fatalf("vol2 was handed a unit already reserved by vol1: %s", resp2.Assignments[0].WorkUnitId)
	}

	// No more assignable work for vol2: OK response, empty assignments, delay set.
	noWork, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey2), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID2,
		PublicKey:      pubKey2,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 4,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit (no-work): %v", err)
	}
	if len(noWork.Assignments) != 0 {
		t.Fatalf("expected no-work empty assignments, got %d", len(noWork.Assignments))
	}
	if noWork.RetryAfterSeconds < 1 {
		t.Errorf("expected retry_after_seconds >= 1 on no-work reply, got %d", noWork.RetryAfterSeconds)
	}
}

// TestBatchDispatch_RunStartFlipsCopyToRunningUnitStaysQueued verifies that the
// first RUNNING heartbeat run-starts THIS volunteer's COPY (RESERVED -> RUNNING,
// starting its per-copy deadline clock) while the WORK UNIT stays QUEUED so its other
// redundancy copies keep dispatching in parallel (per-copy model, migration 00006).
func TestBatchDispatch_RunStartFlipsCopyToRunningUnitStaysQueued(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "batch-runstart")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Batch RunStart Leaf"))
	generateLeafWUs(t, env, lf.ID, 1)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "runstart-vol")

	resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID,
		PublicKey:      pubKey,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}
	if len(resp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(resp.Assignments))
	}
	wuID := resp.Assignments[0].WorkUnitId

	// While reserved, the unit is QUEUED with a live RESERVED copy (started_at NULL)
	// held by volID — the per-copy replacement for the retired reserved_volunteer_id
	// column.
	var state string
	var reservedVol *string
	if err := env.pool.QueryRow(ctx,
		`SELECT wu.state,
		        (SELECT h.volunteer_id::text FROM work_unit_assignment_history h
		         WHERE h.work_unit_id = wu.id AND h.outcome IS NULL AND h.started_at IS NULL
		         ORDER BY h.assigned_at DESC LIMIT 1)
		 FROM work_units wu WHERE wu.id = $1`, wuID).
		Scan(&state, &reservedVol); err != nil {
		t.Fatalf("query reserved state: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("reserved unit state = %q, want QUEUED", state)
	}
	if reservedVol == nil || *reservedVol != volID {
		t.Fatalf("reserved copy volunteer = %v, want %s", reservedVol, volID)
	}

	// StartWork = run start: flips THIS volunteer's RESERVED copy to RUNNING (started_at
	// set, the per-copy deadline clock start). The WORK UNIT stays QUEUED; assigned_at on
	// the unit is updated best-effort for observability.
	if _, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
		WorkUnitId:  wuID,
		VolunteerId: volID,
	}); err != nil {
		t.Fatalf("StartWork (run start): %v", err)
	}

	var state2 string
	var assignedAt *time.Time
	var runningCopies int
	if err := env.pool.QueryRow(ctx,
		`SELECT wu.state, wu.assigned_at,
		        (SELECT COUNT(*) FROM work_unit_assignment_history h
		         WHERE h.work_unit_id = wu.id AND h.volunteer_id::text = $2
		           AND h.outcome IS NULL AND h.started_at IS NOT NULL)
		 FROM work_units wu WHERE wu.id = $1`, wuID, volID).
		Scan(&state2, &assignedAt, &runningCopies); err != nil {
		t.Fatalf("query post-runstart state: %v", err)
	}
	if state2 != "QUEUED" {
		t.Fatalf("post-runstart unit state = %q, want QUEUED (per-copy: only the copy runs)", state2)
	}
	if runningCopies != 1 {
		t.Errorf("expected the volunteer's copy RUNNING (started_at set) after run start, got %d running copies", runningCopies)
	}
	if assignedAt == nil {
		t.Errorf("expected assigned_at set at run start (deadline clock starts)")
	}
}

// TestBatchDispatch_AbandonReservedUnitIsReReservable verifies the head handles a
// volunteer abandoning a BUFFERED (reserved, never-started) unit: the abandon
// succeeds (not Internal), the reservation is dropped, the unit stays QUEUED, and
// a second volunteer can immediately reserve it. This is the head side of the
// client's prepare-failure / queue-full / slot-start-failure abandon paths.
func TestBatchDispatch_AbandonReservedUnitIsReReservable(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "batch-abandon")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Batch Abandon Leaf"))
	generateLeafWUs(t, env, lf.ID, 1)

	pubKey1 := genVolunteerKey(t)
	volID1 := registerHLVolunteer(t, env, ctx, pubKey1, "abandon-vol1")
	pubKey2 := genVolunteerKey(t)
	volID2 := registerHLVolunteer(t, env, ctx, pubKey2, "abandon-vol2")

	resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey1), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID1,
		PublicKey:      pubKey1,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit vol1: %v", err)
	}
	if len(resp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(resp.Assignments))
	}
	wuID := resp.Assignments[0].WorkUnitId

	// Abandon the buffered (reserved, un-started) copy before it ever heartbeats.
	// Per-copy model: abandon closes THIS volunteer's live copy as ABANDONED; the unit
	// stays QUEUED and redispatches a fresh copy (uncapped). Pre-fix this returned
	// codes.Internal (TransitionToExpired required ASSIGNED/RUNNING).
	ab, err := env.grpc.AbandonWorkUnit(signFor(t, ctx, pubKey1), &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wuID,
		VolunteerId: volID1,
		Reason:      "prepare failed",
	})
	if err != nil {
		t.Fatalf("AbandonWorkUnit on reserved unit failed: %v", err)
	}
	if !ab.Requeued {
		t.Fatalf("expected reserved unit requeued on abandon, got %+v", ab)
	}

	// The unit is QUEUED with NO live copy (the abandoned copy is closed, outcome set).
	var state string
	var liveCopies int
	if err := env.pool.QueryRow(ctx,
		`SELECT wu.state,
		        (SELECT COUNT(*) FROM work_unit_assignment_history h
		         WHERE h.work_unit_id = wu.id AND h.outcome IS NULL)
		 FROM work_units wu WHERE wu.id = $1`, wuID).
		Scan(&state, &liveCopies); err != nil {
		t.Fatalf("query post-abandon state: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("post-abandon state = %q, want QUEUED", state)
	}
	if liveCopies != 0 {
		t.Fatalf("expected no live copy after abandon, got %d", liveCopies)
	}

	// A second volunteer can now reserve the freed unit.
	resp2, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey2), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID2,
		PublicKey:      pubKey2,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit vol2: %v", err)
	}
	if len(resp2.Assignments) != 1 || resp2.Assignments[0].WorkUnitId != wuID {
		t.Fatalf("expected vol2 to reserve the abandoned unit %s, got %+v", wuID, resp2.Assignments)
	}
}

// TestBatchDispatch_LapsedReservationReclaimedBySweepThenReReservable verifies the
// orphaned-buffered-work reclaim under the per-copy model: a unit whose holder
// vanished (its RESERVED copy's lease lapsed, simulated by backdating the copy's
// reserved_until) is reclaimed by the fault-monitor copy sweep — FindExpiredCopies
// surfaces the lapsed RESERVED copy and CloseCopy parks it EXPIRED — after which the
// unit (QUEUED, no live copy) is re-reservable by another volunteer through the normal
// gRPC path. Per-copy model (migration 00006): a reservation is a live history row
// (outcome IS NULL), so unlike the retired reserved_until column it counts toward
// redundancy until the deadline sweep closes it (property 5: the deadline is the only
// early-reclaim clock).
func TestBatchDispatch_LapsedReservationReclaimedBySweepThenReReservable(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "batch-lapse")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Batch Lapse Leaf"))
	generateLeafWUs(t, env, lf.ID, 1)

	pubKey1 := genVolunteerKey(t)
	volID1 := registerHLVolunteer(t, env, ctx, pubKey1, "lapse-vol1")
	pubKey2 := genVolunteerKey(t)
	volID2 := registerHLVolunteer(t, env, ctx, pubKey2, "lapse-vol2")

	resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey1), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID1,
		PublicKey:      pubKey1,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit vol1: %v", err)
	}
	if len(resp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(resp.Assignments))
	}
	wuID := resp.Assignments[0].WorkUnitId

	// Simulate the holder crashing: backdate the RESERVED copy's lease so it has lapsed.
	if _, err := env.pool.Exec(ctx,
		`UPDATE work_unit_assignment_history SET reserved_until = NOW() - INTERVAL '1 minute'
		 WHERE work_unit_id = $1 AND outcome IS NULL AND started_at IS NULL`, wuID); err != nil {
		t.Fatalf("backdate reserved copy lease: %v", err)
	}

	// The lapsed RESERVED copy is still a live history row, so it still counts toward the
	// leaf's redundancy until the deadline sweep closes it.
	var liveBefore int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL", wuID).
		Scan(&liveBefore); err != nil {
		t.Fatalf("count live copies before sweep: %v", err)
	}
	if liveBefore != 1 {
		t.Fatalf("expected the lapsed reservation to remain a live copy until swept, got %d", liveBefore)
	}

	// Fault-monitor copy sweep: FindExpiredCopies surfaces the lapsed RESERVED copy and
	// CloseCopy parks it EXPIRED — the per-copy reclaim path (no per-unit transition).
	wuRepo := workunit.NewPgxWorkUnitRepository(env.pool)
	expired, err := wuRepo.FindExpiredCopies(ctx, 10)
	if err != nil {
		t.Fatalf("FindExpiredCopies: %v", err)
	}
	swept := false
	for _, cp := range expired {
		if cp.WorkUnitID.String() == wuID {
			if cerr := wuRepo.CloseCopy(ctx, cp.ID, "EXPIRED"); cerr != nil {
				t.Fatalf("CloseCopy(EXPIRED): %v", cerr)
			}
			swept = true
		}
	}
	if !swept {
		t.Fatalf("expected the lapsed RESERVED copy of %s to be surfaced by FindExpiredCopies", wuID)
	}

	// After the sweep there is no live copy and the unit stays QUEUED, so it is
	// dispatchable again.
	var liveAfter int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL", wuID).
		Scan(&liveAfter); err != nil {
		t.Fatalf("count live copies after sweep: %v", err)
	}
	if liveAfter != 0 {
		t.Fatalf("expected no live copy after the sweep closed the lapsed reservation, got %d", liveAfter)
	}

	// vol2 re-reserves the reclaimed unit through the normal gRPC path.
	resp2, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey2), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID2,
		PublicKey:      pubKey2,
		LeafIds:        []string{lf.ID.String()},
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit vol2: %v", err)
	}
	if len(resp2.Assignments) != 1 || resp2.Assignments[0].WorkUnitId != wuID {
		t.Fatalf("expected vol2 to re-reserve lapsed unit %s, got %+v", wuID, resp2.Assignments)
	}
}
