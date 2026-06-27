//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"
)

// leaf execution_config carrying a per-unit FP-ops estimate so the feasibility
// gate has something to compute against. 1e12 FP ops per unit.
const feasibilityExecConfig = `{"runtime":"NATIVE","gpu_required":false,"rsc_fpops_est":1000000000000}`

// TestFindNextAssignable_FeasibilityByDeadline verifies the dispatch query excludes
// a unit when the requester's benchmark (carried on AssignmentOptions) says it
// cannot finish before the deadline, serves it to a fast-enough host, and is inert
// when no benchmark is reported.
func TestFindNextAssignable_FeasibilityByDeadline(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-feasible")
	leafID := createActiveTestLeaf(t, pool, &userID, "", feasibilityExecConfig, "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	wu.DeadlineSeconds = 10 // est 1e12/bench vs 10s
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	base := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	// Slow host: est = 1e12 / 1e9 = 1000s > 10s -> excluded.
	slow := base
	slow.BenchmarkFPOPS = 1e9
	if found, err := repo.FindNextAssignable(ctx, slow); err != nil {
		t.Fatalf("FindNextAssignable(slow): %v", err)
	} else if found != nil {
		t.Fatalf("slow host should get nil (infeasible), got %v", found.ID)
	}

	// Fast host: est = 1e12 / 1e12 = 1s <= 10s -> served.
	fast := base
	fast.BenchmarkFPOPS = 1e12
	if found, err := repo.FindNextAssignable(ctx, fast); err != nil {
		t.Fatalf("FindNextAssignable(fast): %v", err)
	} else if found == nil || found.ID != wu.ID {
		t.Fatalf("fast host should get the unit, got %v", found)
	}

	// No benchmark reported: cannot estimate -> served (never refuse on a guess).
	none := base
	none.BenchmarkFPOPS = 0
	if found, err := repo.FindNextAssignable(ctx, none); err != nil {
		t.Fatalf("FindNextAssignable(none): %v", err)
	} else if found == nil || found.ID != wu.ID {
		t.Fatalf("un-benchmarked host should still get the unit, got %v", found)
	}
}

// TestReserveCopy_FeasibilityByDeadline verifies the authoritative reservation gate
// reads the volunteer's STORED benchmark and refuses a copy a too-slow host could
// not finish before the deadline, while allowing a fast-enough host.
func TestReserveCopy_FeasibilityByDeadline(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-feasible")
	leafID := createActiveTestLeaf(t, pool, &userID, "", feasibilityExecConfig, "")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	mkUnit := func() *WorkUnit {
		wu := newTestWorkUnit(leafID, nil)
		wu.State = WorkUnitStateQueued
		wu.DeadlineSeconds = 10
		if err := repo.Create(ctx, wu); err != nil {
			t.Fatalf("Create: %v", err)
		}
		return wu
	}

	until := time.Now().UTC().Add(15 * time.Minute)

	// Slow volunteer (stored benchmark 1e9): est 1000s > 10s -> reservation refused.
	slowVol := createTestVolunteer(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE volunteers SET hardware_capabilities = '{"benchmark_fpops":1000000000}'::jsonb WHERE id = $1`,
		slowVol); err != nil {
		t.Fatalf("set slow benchmark: %v", err)
	}
	slowUnit := mkUnit()
	if _, err := repo.ReserveCopy(ctx, slowUnit.ID, slowVol, nil, until, 10); err == nil {
		t.Fatal("ReserveCopy(slow) should have been refused (infeasible), but succeeded")
	}

	// Fast volunteer (stored benchmark 1e12): est 1s <= 10s -> reservation succeeds.
	fastVol := createTestVolunteer(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE volunteers SET hardware_capabilities = '{"benchmark_fpops":1000000000000}'::jsonb WHERE id = $1`,
		fastVol); err != nil {
		t.Fatalf("set fast benchmark: %v", err)
	}
	fastUnit := mkUnit()
	if _, err := repo.ReserveCopy(ctx, fastUnit.ID, fastVol, nil, until, 10); err != nil {
		t.Fatalf("ReserveCopy(fast) should have succeeded, got: %v", err)
	}
}
