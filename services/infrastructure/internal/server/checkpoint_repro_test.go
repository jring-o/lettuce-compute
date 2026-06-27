//go:build integration

package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/status"
)

// registerCPVolunteer registers a fresh volunteer and returns its keypair + id.
func registerCPVolunteer(t *testing.T, ctx context.Context, client lettucev1.VolunteerServiceClient) (cpKey, types.ID) {
	t.Helper()
	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	id, _ := types.ParseID(regResp.VolunteerId)
	return key, id
}

// TestRepro_Bug3_SecondRedundancyCopyCannotCheckpoint reproduces QuaXeros's
// "volunteer is not assigned to this work unit" (PermissionDenied) error.
//
// The leaf is redundancy_factor=2, so two DISTINCT volunteers hold concurrent
// open copies of the same work unit. The head authorizes SaveCheckpoint against
// the single work_units.assigned_volunteer_id, which can only name ONE of them.
// The other volunteer — running a perfectly valid copy — is refused.
//
// DESIRED: a volunteer with an OPEN copy may checkpoint. This test fails on the
// pre-fix head with PermissionDenied and passes once SaveCheckpoint authorizes
// against the open assignment-history copy.
func TestRepro_Bug3_SecondRedundancyCopyCannotCheckpoint(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true) // redundancy 2, checkpointing on

	keyA, volA := registerCPVolunteer(t, ctx, client)
	keyB, volB := registerCPVolunteer(t, ctx, client)
	_ = keyA

	// WU assigned to A, with A's open copy.
	wuID := createCPAssignedWorkUnit(t, pool, leafID, volA)

	// B holds a second concurrent open copy (redundancy 2).
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volB, time.Now().UTC()); err != nil {
		t.Fatalf("insert B copy: %v", err)
	}

	// B checkpoints its own valid copy.
	_, err := client.SaveCheckpoint(signCP(ctx, keyB), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volB.String(),
		CheckpointData:     []byte("B-seq1"),
		CheckpointSequence: 1,
	})
	if err != nil {
		st, _ := status.FromError(err)
		t.Fatalf("BUG 3 REPRODUCED: B holds a valid open copy but SaveCheckpoint was refused: code=%s msg=%q",
			st.Code(), st.Message())
	}
}

// TestRepro_Bug2_ReassignedVolunteerSequenceCollision reproduces QuaXeros's
// "checkpoint sequence must be greater than 1" (AlreadyExists) error.
//
// Volunteer A checkpoints sequence 1 (work_units.last_checkpoint_sequence -> 1).
// The unit is then reassigned to B (a different volunteer with a fresh local
// checkpoint counter). B's first checkpoint at sequence 1 is rejected because
// the head validates against the single shared per-WU sequence rather than B's
// own per-volunteer chain.
//
// DESIRED: B's first checkpoint is accepted (independent per-volunteer chain).
// Fails pre-fix with AlreadyExists; passes once the sequence is scoped per
// (work_unit, volunteer).
func TestRepro_Bug2_ReassignedVolunteerSequenceCollision(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	keyA, volA := registerCPVolunteer(t, ctx, client)
	keyB, volB := registerCPVolunteer(t, ctx, client)

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volA)

	// A saves sequence 1.
	if _, err := client.SaveCheckpoint(signCP(ctx, keyA), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volA.String(),
		CheckpointData:     []byte("A-seq1"),
		CheckpointSequence: 1,
	}); err != nil {
		t.Fatalf("A SaveCheckpoint seq 1: %v", err)
	}

	// Reassign the unit to B: assigned_volunteer_id -> B, give B an open copy.
	if _, err := pool.Exec(ctx,
		`UPDATE work_units SET assigned_volunteer_id = $2 WHERE id = $1`, wuID, volB); err != nil {
		t.Fatalf("reassign wu to B: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volB, time.Now().UTC()); err != nil {
		t.Fatalf("insert B copy: %v", err)
	}

	// B saves its first checkpoint (fresh local counter -> sequence 1).
	_, err := client.SaveCheckpoint(signCP(ctx, keyB), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volB.String(),
		CheckpointData:     []byte("B-seq1"),
		CheckpointSequence: 1,
	})
	if err != nil {
		st, _ := status.FromError(err)
		t.Fatalf("BUG 2 REPRODUCED: reassigned volunteer B's first checkpoint refused: code=%s msg=%q",
			st.Code(), st.Message())
	}
}
