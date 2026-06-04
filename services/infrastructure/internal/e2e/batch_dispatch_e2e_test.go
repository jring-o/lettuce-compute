//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

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

// TestBatchDispatch_RunStartFlipsAssignedAndClearsReservation verifies that the
// first RUNNING heartbeat on a reserved unit flips QUEUED -> ASSIGNED, starting
// the deadline/heartbeat clock and clearing the reservation columns.
func TestBatchDispatch_RunStartFlipsAssignedAndClearsReservation(t *testing.T) {
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

	// While reserved, the unit is still QUEUED with reservation columns set.
	var state string
	var reservedVol *string
	if err := env.pool.QueryRow(ctx,
		"SELECT state, reserved_volunteer_id::text FROM work_units WHERE id = $1", wuID).
		Scan(&state, &reservedVol); err != nil {
		t.Fatalf("query reserved state: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("reserved unit state = %q, want QUEUED", state)
	}
	if reservedVol == nil || *reservedVol != volID {
		t.Fatalf("reserved_volunteer_id = %v, want %s", reservedVol, volID)
	}

	// StartWork = run start: flips the reserved QUEUED unit to ASSIGNED, clears the
	// reservation columns, and sets assigned_at (the deadline clock start). With
	// per-task heartbeats removed there is no separate ASSIGNED->RUNNING step; the
	// unit stays ASSIGNED until the result is submitted.
	if _, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
		WorkUnitId:  wuID,
		VolunteerId: volID,
	}); err != nil {
		t.Fatalf("StartWork (run start): %v", err)
	}

	var state2 string
	var reservedUntil *time.Time
	var assignedAt *time.Time
	if err := env.pool.QueryRow(ctx,
		"SELECT state, reserved_until, assigned_at FROM work_units WHERE id = $1", wuID).
		Scan(&state2, &reservedUntil, &assignedAt); err != nil {
		t.Fatalf("query post-runstart state: %v", err)
	}
	if state2 != "ASSIGNED" {
		t.Fatalf("post-runstart state = %q, want ASSIGNED", state2)
	}
	if reservedUntil != nil {
		t.Errorf("expected reservation cleared at run start, got reserved_until=%v", reservedUntil)
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

	// Abandon the buffered (reserved, un-started) unit before it ever heartbeats.
	// Pre-fix this returned codes.Internal (TransitionToExpired requires
	// ASSIGNED/RUNNING); now it drops the reservation and requeues.
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

	// The unit is QUEUED with no reservation columns.
	var state string
	var reservedVol *string
	if err := env.pool.QueryRow(ctx,
		"SELECT state, reserved_volunteer_id::text FROM work_units WHERE id = $1", wuID).
		Scan(&state, &reservedVol); err != nil {
		t.Fatalf("query post-abandon state: %v", err)
	}
	if state != "QUEUED" {
		t.Fatalf("post-abandon state = %q, want QUEUED", state)
	}
	if reservedVol != nil {
		t.Fatalf("expected reservation cleared after abandon, got reserved_volunteer_id=%v", *reservedVol)
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

// TestBatchDispatch_LapsedReservationReReservable verifies the orphaned-buffered-
// work leak is fixed end to end: a unit whose holder vanished (its lease lapsed,
// simulated by backdating reserved_until) stays QUEUED with no stale active history
// row and is re-reservable by another volunteer through the normal gRPC path, with
// no manual transition or sweeper.
func TestBatchDispatch_LapsedReservationReReservable(t *testing.T) {
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

	// Simulate the holder crashing: backdate the lease so it has lapsed. No reclaim
	// sweep runs — the lapsed lease is re-reservable purely via the assignment guard.
	if _, err := env.pool.Exec(ctx,
		"UPDATE work_units SET reserved_until = NOW() - INTERVAL '1 minute' WHERE id = $1", wuID); err != nil {
		t.Fatalf("backdate reservation: %v", err)
	}

	// Confirm there is NO stale active assignment_history row (the leak's signature).
	var activeRows int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL", wuID).
		Scan(&activeRows); err != nil {
		t.Fatalf("count active history rows: %v", err)
	}
	if activeRows != 0 {
		t.Fatalf("expected no active assignment_history row for a reservation, got %d (leak)", activeRows)
	}

	// vol2 re-reserves the lapsed unit with no manual transition.
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
