//go:build integration

package stats_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

// createTestUser inserts a minimal user for FK references.
func createTestUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(t.Context(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id,
		username+"@test.example.com",
		username,
		"Test User "+username,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("failed to create test user %s: %v", username, err)
	}
	return id
}

// createTestLeaf inserts a minimal project for FK references.
func createTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID, name string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(t.Context(), `
		INSERT INTO leafs (id, name, slug, description, research_area, creator_id,
			state, task_pattern, is_ongoing, visibility)
		VALUES ($1, $2, $3, $4, $5::text[], $6, 'ACTIVE', 'PARAMETER_SWEEP', false, 'PUBLIC')`,
		id, name, name+"-slug", "A test leaf for stats tests", "{physics}", creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

// createTestWorkUnits inserts work units for a leaf. Most states map directly to
// work_units.state. ASSIGNED/RUNNING are special: under the per-copy dispatch model
// (migration 00006) a unit stays QUEUED while its copies run, and the stats engine
// derives "assigned"/"running" from live work_unit_assignment_history copies, NOT from
// work_units.state. So those two insert a QUEUED unit plus the matching live copy:
//
//	ASSIGNED -> RESERVED copy (outcome NULL, started_at NULL, reserved_until future)
//	RUNNING  -> RUNNING  copy (outcome NULL, started_at set)
func createTestWorkUnits(t *testing.T, pool *pgxpool.Pool, leafID types.ID, states []string) {
	t.Helper()
	for _, state := range states {
		switch state {
		case "ASSIGNED":
			createQueuedUnitWithCopy(t, pool, leafID, false /* started */)
		case "RUNNING":
			createQueuedUnitWithCopy(t, pool, leafID, true /* started */)
		default:
			insertWorkUnit(t, pool, leafID, state)
		}
	}
}

// insertWorkUnit inserts a single work_units row in the given state and returns its id.
func insertWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, state string) types.ID {
	t.Helper()
	id := types.NewID()
	params, _ := json.Marshal(map[string]interface{}{"x": 1})
	_, err := pool.Exec(t.Context(), `
		INSERT INTO work_units (id, leaf_id, state, priority, code_artifact_ref,
			parameters, deadline_seconds)
		VALUES ($1, $2, $3, 'NORMAL', 'ref://test', $4, 3600)`,
		id, leafID, state, params,
	)
	if err != nil {
		t.Fatalf("failed to create work unit in state %s: %v", state, err)
	}
	return id
}

// createQueuedUnitWithCopy inserts a QUEUED unit plus one live assignment-history copy,
// mirroring real per-copy dispatch (a unit stays QUEUED while its copies run).
// started=false -> RESERVED copy (counts as "assigned"); started=true -> RUNNING copy
// (counts as "running").
func createQueuedUnitWithCopy(t *testing.T, pool *pgxpool.Pool, leafID types.ID, started bool) {
	t.Helper()
	unitID := insertWorkUnit(t, pool, leafID, "QUEUED")
	volID := createTestVolunteer(t, pool)
	var startedAt any // nil -> RESERVED copy; time -> RUNNING copy
	if started {
		startedAt = time.Now().UTC()
	}
	_, err := pool.Exec(t.Context(), `
		INSERT INTO work_unit_assignment_history
			(work_unit_id, volunteer_id, assigned_at, reserved_until, started_at, deadline_seconds)
		VALUES ($1, $2, NOW(), NOW() + INTERVAL '5 minutes', $3, 3600)`,
		unitID, volID, startedAt,
	)
	if err != nil {
		t.Fatalf("failed to create assignment-history copy (started=%v): %v", started, err)
	}
}

// createTestVolunteer inserts a minimal volunteer for the copy FK
// (work_unit_assignment_history.volunteer_id -> volunteers.id). public_key is
// bytea NOT NULL + UNIQUE, so derive a unique 32-byte key from the volunteer's own
// UUID. numeric_id is covered by a sequence default.
func createTestVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	pk := make([]byte, 32)
	copy(pk, id[:])
	copy(pk[16:], id[:])
	_, err := pool.Exec(t.Context(), `
		INSERT INTO volunteers (id, public_key, is_active) VALUES ($1, $2, true)`,
		id, pk,
	)
	if err != nil {
		t.Fatalf("failed to create test volunteer: %v", err)
	}
	return id
}

func TestComputeSnapshotWithWorkUnits(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-snap1")
	leafID := createTestLeaf(t, pool, userID, "Stats Compute Test")

	// Create work units in various states.
	createTestWorkUnits(t, pool, leafID, []string{
		"QUEUED", "QUEUED", "QUEUED",
		"ASSIGNED",
		"RUNNING", "RUNNING",
		"COMPLETED",
		"VALIDATED",
		"REJECTED",
		"EXPIRED",
	})

	engine := stats.NewEngine(pool)
	snap, err := engine.ComputeSnapshot(t.Context(), leafID)
	if err != nil {
		t.Fatalf("ComputeSnapshot failed: %v", err)
	}

	if snap.TotalWorkUnits != 10 {
		t.Errorf("total_work_units = %d, want 10", snap.TotalWorkUnits)
	}
	if snap.WorkUnitsQueued != 3 {
		t.Errorf("work_units_queued = %d, want 3", snap.WorkUnitsQueued)
	}
	if snap.WorkUnitsAssigned != 1 {
		t.Errorf("work_units_assigned = %d, want 1", snap.WorkUnitsAssigned)
	}
	if snap.WorkUnitsRunning != 2 {
		t.Errorf("work_units_running = %d, want 2", snap.WorkUnitsRunning)
	}
	if snap.WorkUnitsCompleted != 1 {
		t.Errorf("work_units_completed = %d, want 1", snap.WorkUnitsCompleted)
	}
	if snap.WorkUnitsValidated != 1 {
		t.Errorf("work_units_validated = %d, want 1", snap.WorkUnitsValidated)
	}
	if snap.WorkUnitsFailed != 2 {
		t.Errorf("work_units_failed = %d, want 2 (REJECTED + EXPIRED)", snap.WorkUnitsFailed)
	}
	if snap.ActiveVolunteers != 0 {
		t.Errorf("active_volunteers = %d, want 0 for v0.2", snap.ActiveVolunteers)
	}
	if snap.TotalCreditGranted != 0 {
		t.Errorf("total_credit_granted = %f, want 0 for v0.2", snap.TotalCreditGranted)
	}
	if snap.AvgCompletionSeconds != nil {
		t.Errorf("avg_completion_seconds should be nil for v0.2")
	}
	if snap.AgreementRate != nil {
		t.Errorf("agreement_rate should be nil for v0.2")
	}
	if snap.ThroughputPerHour != nil {
		t.Errorf("throughput_per_hour should be nil for v0.2")
	}
	if types.IsNilID(snap.ID) {
		t.Error("snapshot ID should be set")
	}
	if snap.SnapshotAt.IsZero() {
		t.Error("snapshot_at should be set")
	}
	if snap.LeafID != leafID {
		t.Errorf("leaf_id = %v, want %v", snap.LeafID, leafID)
	}
}

func TestComputeSnapshotNoWorkUnits(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-snap2")
	leafID := createTestLeaf(t, pool, userID, "Stats Empty Test")

	engine := stats.NewEngine(pool)
	snap, err := engine.ComputeSnapshot(t.Context(), leafID)
	if err != nil {
		t.Fatalf("ComputeSnapshot failed: %v", err)
	}

	if snap.TotalWorkUnits != 0 {
		t.Errorf("total_work_units = %d, want 0", snap.TotalWorkUnits)
	}
	if snap.WorkUnitsQueued != 0 {
		t.Errorf("work_units_queued = %d, want 0", snap.WorkUnitsQueued)
	}
	if snap.WorkUnitsAssigned != 0 {
		t.Errorf("work_units_assigned = %d, want 0", snap.WorkUnitsAssigned)
	}
	if snap.WorkUnitsRunning != 0 {
		t.Errorf("work_units_running = %d, want 0", snap.WorkUnitsRunning)
	}
	if snap.WorkUnitsCompleted != 0 {
		t.Errorf("work_units_completed = %d, want 0", snap.WorkUnitsCompleted)
	}
	if snap.WorkUnitsValidated != 0 {
		t.Errorf("work_units_validated = %d, want 0", snap.WorkUnitsValidated)
	}
	if snap.WorkUnitsFailed != 0 {
		t.Errorf("work_units_failed = %d, want 0", snap.WorkUnitsFailed)
	}
}

func TestGetOrComputeSnapshotCaching(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-cache1")
	leafID := createTestLeaf(t, pool, userID, "Stats Cache Test")

	createTestWorkUnits(t, pool, leafID, []string{"QUEUED", "QUEUED"})

	engine := stats.NewEngine(pool)

	// First call: should compute.
	snap1, err := engine.GetOrComputeSnapshot(t.Context(), leafID, 15*time.Minute)
	if err != nil {
		t.Fatalf("GetOrComputeSnapshot (1st) failed: %v", err)
	}
	if snap1.TotalWorkUnits != 2 {
		t.Errorf("snap1 total = %d, want 2", snap1.TotalWorkUnits)
	}

	// Add more work units.
	createTestWorkUnits(t, pool, leafID, []string{"QUEUED"})

	// Second call: should return cached (snapshot is fresh).
	snap2, err := engine.GetOrComputeSnapshot(t.Context(), leafID, 15*time.Minute)
	if err != nil {
		t.Fatalf("GetOrComputeSnapshot (2nd) failed: %v", err)
	}
	if snap2.ID != snap1.ID {
		t.Errorf("expected cached snapshot (same ID), got different: %v vs %v", snap2.ID, snap1.ID)
	}
	if snap2.TotalWorkUnits != 2 {
		t.Errorf("cached snap should still show 2, got %d", snap2.TotalWorkUnits)
	}

	// Third call with 0 maxAge: should force recompute.
	snap3, err := engine.GetOrComputeSnapshot(t.Context(), leafID, 0)
	if err != nil {
		t.Fatalf("GetOrComputeSnapshot (3rd) failed: %v", err)
	}
	if snap3.ID == snap1.ID {
		t.Error("expected fresh snapshot, got cached")
	}
	if snap3.TotalWorkUnits != 3 {
		t.Errorf("fresh snap total = %d, want 3", snap3.TotalWorkUnits)
	}
}

func TestListSnapshots(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-list1")
	leafID := createTestLeaf(t, pool, userID, "Stats List Test")

	createTestWorkUnits(t, pool, leafID, []string{"QUEUED", "QUEUED"})

	engine := stats.NewEngine(pool)

	before := time.Now().UTC().Add(-1 * time.Second)

	// Create 3 snapshots.
	for i := 0; i < 3; i++ {
		_, err := engine.ComputeSnapshot(t.Context(), leafID)
		if err != nil {
			t.Fatalf("ComputeSnapshot (%d) failed: %v", i, err)
		}
	}

	after := time.Now().UTC().Add(1 * time.Second)

	snapshots, err := engine.ListSnapshots(t.Context(), leafID, stats.StatsHistoryFilters{
		From:     before,
		To:       after,
		Interval: "raw",
	})
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}

	// Verify ascending order.
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i].SnapshotAt.Before(snapshots[i-1].SnapshotAt) {
			t.Errorf("snapshots not in ascending order: [%d]=%v > [%d]=%v",
				i-1, snapshots[i-1].SnapshotAt, i, snapshots[i].SnapshotAt)
		}
	}

	// Query a range that excludes all snapshots.
	oldFrom := before.Add(-2 * time.Hour)
	oldTo := before.Add(-1 * time.Hour)
	empty, err := engine.ListSnapshots(t.Context(), leafID, stats.StatsHistoryFilters{
		From:     oldFrom,
		To:       oldTo,
		Interval: "raw",
	})
	if err != nil {
		t.Fatalf("ListSnapshots (empty range) failed: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 snapshots in old range, got %d", len(empty))
	}
}

func TestListSnapshotsHourlyDownsampling(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-hourly")
	leafID := createTestLeaf(t, pool, userID, "Stats Hourly Test")

	createTestWorkUnits(t, pool, leafID, []string{"QUEUED"})

	engine := stats.NewEngine(pool)

	// Insert 5 snapshots all within the same hour (now).
	for i := 0; i < 5; i++ {
		_, err := engine.ComputeSnapshot(t.Context(), leafID)
		if err != nil {
			t.Fatalf("ComputeSnapshot (%d) failed: %v", i, err)
		}
	}

	before := time.Now().UTC().Add(-1 * time.Hour)
	after := time.Now().UTC().Add(1 * time.Hour)

	// "hourly" should collapse all 5 snapshots (same hour) into 1.
	snapshots, err := engine.ListSnapshots(t.Context(), leafID, stats.StatsHistoryFilters{
		From:     before,
		To:       after,
		Interval: "hourly",
	})
	if err != nil {
		t.Fatalf("ListSnapshots (hourly) failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Errorf("hourly: expected 1 snapshot (all in same hour), got %d", len(snapshots))
	}
}

func TestComputeLeafStatsBatch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-batch1")
	leaf1ID := createTestLeaf(t, pool, userID, "Batch Stats 1")
	leaf2ID := createTestLeaf(t, pool, userID, "Batch Stats 2")
	nonExistentID := types.NewID()

	createTestWorkUnits(t, pool, leaf1ID, []string{"QUEUED", "QUEUED", "ASSIGNED", "RUNNING", "COMPLETED"})
	createTestWorkUnits(t, pool, leaf2ID, []string{"QUEUED", "VALIDATED", "REJECTED"})

	engine := stats.NewEngine(pool)
	result, err := engine.ComputeLeafStatsBatch(t.Context(), []types.ID{leaf1ID, leaf2ID, nonExistentID})
	if err != nil {
		t.Fatalf("ComputeLeafStatsBatch failed: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}

	// Project 1: 5 total, 2 queued, 1 assigned, 1 running, 1 completed
	s1 := result[leaf1ID]
	if s1.TotalWorkUnits != 5 {
		t.Errorf("project1 total = %d, want 5", s1.TotalWorkUnits)
	}
	if s1.WorkUnitsQueued != 2 {
		t.Errorf("project1 queued = %d, want 2", s1.WorkUnitsQueued)
	}
	if s1.WorkUnitsAssigned != 1 {
		t.Errorf("project1 assigned = %d, want 1", s1.WorkUnitsAssigned)
	}
	if s1.WorkUnitsRunning != 1 {
		t.Errorf("project1 running = %d, want 1", s1.WorkUnitsRunning)
	}
	if s1.WorkUnitsCompleted != 1 {
		t.Errorf("project1 completed = %d, want 1", s1.WorkUnitsCompleted)
	}

	// Project 2: 3 total, 1 queued, 1 validated, 1 failed (REJECTED)
	s2 := result[leaf2ID]
	if s2.TotalWorkUnits != 3 {
		t.Errorf("project2 total = %d, want 3", s2.TotalWorkUnits)
	}
	if s2.WorkUnitsValidated != 1 {
		t.Errorf("project2 validated = %d, want 1", s2.WorkUnitsValidated)
	}
	if s2.WorkUnitsFailed != 1 {
		t.Errorf("project2 failed = %d, want 1", s2.WorkUnitsFailed)
	}

	// Non-existent project: zero-value stats
	s3 := result[nonExistentID]
	if s3.TotalWorkUnits != 0 {
		t.Errorf("non-existent project total = %d, want 0", s3.TotalWorkUnits)
	}
}

func TestListSnapshotsDailyDownsampling(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "eng-daily")
	leafID := createTestLeaf(t, pool, userID, "Stats Daily Test")

	createTestWorkUnits(t, pool, leafID, []string{"QUEUED"})

	engine := stats.NewEngine(pool)

	// Insert 3 snapshots all within the same day (now).
	for i := 0; i < 3; i++ {
		_, err := engine.ComputeSnapshot(t.Context(), leafID)
		if err != nil {
			t.Fatalf("ComputeSnapshot (%d) failed: %v", i, err)
		}
	}

	before := time.Now().UTC().Add(-1 * time.Hour)
	after := time.Now().UTC().Add(1 * time.Hour)

	// "daily" should collapse all 3 snapshots (same day) into 1.
	snapshots, err := engine.ListSnapshots(t.Context(), leafID, stats.StatsHistoryFilters{
		From:     before,
		To:       after,
		Interval: "daily",
	})
	if err != nil {
		t.Fatalf("ListSnapshots (daily) failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Errorf("daily: expected 1 snapshot (all in same day), got %d", len(snapshots))
	}
}
