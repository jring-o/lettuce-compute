//go:build integration

package server_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// insertContribVolunteer inserts a volunteer keyed by the given Ed25519 public key
// so GetMyContribution (which resolves the caller by its verified key) can find it.
func insertContribVolunteer(t *testing.T, pool *pgxpool.Pool, pub ed25519.PublicKey) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, $3, $4, 'ALWAYS', true, NOW())`,
		id, []byte(pub), json.RawMessage(`{"cpu_cores":4}`), []string{"NATIVE"},
	)
	if err != nil {
		t.Fatalf("insert contrib volunteer: %v", err)
	}
	return id
}

// insertContribCredit writes one AGREED result (carrying the given cpu/gpu seconds)
// and a credit_ledger row of `amount` for (volID, leafID, wuID).
func insertContribCredit(t *testing.T, pool *pgxpool.Pool, leafID, volID, wuID types.ID, amount, cpuSeconds, gpuSeconds float64) {
	t.Helper()
	ctx := context.Background()
	resID := types.NewID()
	meta := fmt.Sprintf(`{"cpu_seconds_user":%g,"gpu_seconds":%g,"wall_clock_seconds":1}`, cpuSeconds, gpuSeconds)
	_, err := pool.Exec(ctx, `
		INSERT INTO results (id, work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status)
		VALUES ($1, $2, $3, $4, $5, $6, 'AGREED')`,
		resID, wuID, volID, json.RawMessage(`{"x":1}`),
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		json.RawMessage(meta),
	)
	if err != nil {
		t.Fatalf("insert contrib result: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO credit_ledger (volunteer_id, leaf_id, work_unit_id, result_id, credit_amount)
		VALUES ($1, $2, $3, $4, $5)`,
		volID, leafID, wuID, resID, amount,
	)
	if err != nil {
		t.Fatalf("insert contrib credit: %v", err)
	}
}

// TestGetMyContribution verifies the full self-service breakdown an authenticated
// volunteer gets back: total, per-leaf, the cpu/gpu resource split, and timeline.
func TestGetMyContribution(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	k := newCPTestKeyPair(t)
	volID := insertContribVolunteer(t, pool, k.pub)
	userID := createCPTestUser(t, pool)
	leafA := createCPTestProject(t, pool, &userID, false)
	leafB := createCPTestProject(t, pool, &userID, false)
	wuA := createCPAssignedWorkUnit(t, pool, leafA, volID)
	wuB := createCPAssignedWorkUnit(t, pool, leafB, volID)

	// leafA: 2.0 credit, cpu-only. leafB: 3.0 credit, gpu.
	insertContribCredit(t, pool, leafA, volID, wuA, 2.0, 10, 0)
	insertContribCredit(t, pool, leafB, volID, wuB, 3.0, 5, 7)

	resp, err := client.GetMyContribution(signCP(ctx, k), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution: %v", err)
	}

	if resp.GetVolunteerId() != volID.String() {
		t.Errorf("volunteer_id = %q, want %q", resp.GetVolunteerId(), volID.String())
	}
	if resp.GetTotalCredit() != 5.0 {
		t.Errorf("total_credit = %v, want 5.0", resp.GetTotalCredit())
	}
	if len(resp.GetByLeaf()) != 2 {
		t.Errorf("by_leaf len = %d, want 2", len(resp.GetByLeaf()))
	}

	byType := map[string]*lettucev1.ResourceTypeContribution{}
	for _, rt := range resp.GetByResourceType() {
		byType[rt.GetResourceType()] = rt
	}
	if got := byType["cpu_only"].GetCredit(); got != 2.0 {
		t.Errorf("cpu_only credit = %v, want 2.0", got)
	}
	if got := byType["gpu"].GetCredit(); got != 3.0 {
		t.Errorf("gpu credit = %v, want 3.0", got)
	}

	if len(resp.GetDaily()) == 0 {
		t.Error("daily timeline empty; expected at least one bucket")
	}
	if len(resp.GetWeekly()) == 0 {
		t.Error("weekly timeline empty; expected at least one bucket")
	}
}

// TestGetMyContributionIdentityIsolation proves a volunteer only ever sees its own
// credit: identity is derived from the verified signing key, not a request field.
// Two registered volunteers see only their own totals, and an unregistered key sees
// an empty breakdown.
func TestGetMyContributionIdentityIsolation(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leaf := createCPTestProject(t, pool, &userID, false)

	kA := newCPTestKeyPair(t)
	kB := newCPTestKeyPair(t)
	volA := insertContribVolunteer(t, pool, kA.pub)
	volB := insertContribVolunteer(t, pool, kB.pub)

	wuA := createCPAssignedWorkUnit(t, pool, leaf, volA)
	wuB := createCPAssignedWorkUnit(t, pool, leaf, volB)
	insertContribCredit(t, pool, leaf, volA, wuA, 2.0, 1, 0)
	insertContribCredit(t, pool, leaf, volB, wuB, 9.0, 1, 0)

	respA, err := client.GetMyContribution(signCP(ctx, kA), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(A): %v", err)
	}
	if respA.GetVolunteerId() != volA.String() || respA.GetTotalCredit() != 2.0 {
		t.Errorf("A saw volunteer_id=%q total=%v, want %q / 2.0", respA.GetVolunteerId(), respA.GetTotalCredit(), volA.String())
	}

	respB, err := client.GetMyContribution(signCP(ctx, kB), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(B): %v", err)
	}
	if respB.GetVolunteerId() != volB.String() || respB.GetTotalCredit() != 9.0 {
		t.Errorf("B saw volunteer_id=%q total=%v, want %q / 9.0", respB.GetVolunteerId(), respB.GetTotalCredit(), volB.String())
	}

	// An authenticated key with no volunteer row gets an empty, zero breakdown.
	kUnknown := newCPTestKeyPair(t)
	respU, err := client.GetMyContribution(signCP(ctx, kUnknown), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(unknown): %v", err)
	}
	if respU.GetTotalCredit() != 0 || len(respU.GetByLeaf()) != 0 {
		t.Errorf("unknown key saw total=%v by_leaf=%d, want 0 / 0", respU.GetTotalCredit(), len(respU.GetByLeaf()))
	}
}
