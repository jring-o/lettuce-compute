//go:build integration

package server_test

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestCountActiveHostsByLeaf verifies Feature C: two machines (hosts) running the
// same identity key on a leaf count as 1 active volunteer but 2 active hosts —
// the exact "1 online volunteer for my 2 hosts" case QuaXeros reported.
func TestCountActiveHostsByLeaf(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()
	_ = client

	ctx := context.Background()
	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, false)

	// One account (volunteer), two machines (hosts), one open copy per host on
	// distinct work units.
	_, volID := registerCPVolunteer(t, ctx, client)
	host1, host2 := types.NewID(), types.NewID()

	wu1 := createCPAssignedWorkUnit(t, pool, leafID, volID)
	if _, err := pool.Exec(ctx, "UPDATE work_unit_assignment_history SET host_id=$1 WHERE work_unit_id=$2", host1, wu1); err != nil {
		t.Fatalf("set host1: %v", err)
	}
	wu2 := createCPAssignedWorkUnit(t, pool, leafID, volID)
	if _, err := pool.Exec(ctx, "UPDATE work_unit_assignment_history SET host_id=$1 WHERE work_unit_id=$2", host2, wu2); err != nil {
		t.Fatalf("set host2: %v", err)
	}

	vols, err := leaf.CountActiveVolunteersByLeaf(ctx, pool)
	if err != nil {
		t.Fatalf("CountActiveVolunteersByLeaf: %v", err)
	}
	hosts, err := leaf.CountActiveHostsByLeaf(ctx, pool)
	if err != nil {
		t.Fatalf("CountActiveHostsByLeaf: %v", err)
	}

	if vols[leafID] != 1 {
		t.Errorf("active volunteers = %d, want 1 (two machines share one account key)", vols[leafID])
	}
	if hosts[leafID] != 2 {
		t.Errorf("active hosts = %d, want 2", hosts[leafID])
	}
}
