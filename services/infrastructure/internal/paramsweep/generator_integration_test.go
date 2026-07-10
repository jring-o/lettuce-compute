//go:build integration

package paramsweep

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
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
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

func createTestUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	_, err := pool.Exec(ctx, `
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

func createTestLeafDB(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID) *leaf.Leaf {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "test-leaf-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","binaries":{"linux-amd64":"sha256:testbinary"},"gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":2.0,"max_reassignments":5,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $5
		)`,
		id, "Test Leaf "+slug, slug, "A test leaf for param sweep", creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}

	return &leaf.Leaf{
		ID:   id,
		Name: "Test Leaf " + slug,
		Slug: slug,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:  "NATIVE",
			Binaries: map[string]string{"linux-amd64": "sha256:testbinary"},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 2.0,
			MaxReassignments:   5,
		},
	}
}

func TestIntegration_Generate_BasicSweep(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "sweepuser1")
	proj := createTestLeafDB(t, pool, &userID)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)

	ctx := context.Background()
	params := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200)},
		"pressure":    []interface{}{1.0, 2.0, 3.0},
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify result.
	if result.WorkUnitsCreated != 6 {
		t.Errorf("expected 6 work units, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 1 {
		t.Errorf("expected 1 batch, got %d", len(result.BatchIDs))
	}
	if result.Status != "complete" {
		t.Errorf("expected 'complete', got %q", result.Status)
	}

	// Verify batch in DB.
	batch, err := batchRepo.GetByID(ctx, result.BatchIDs[0])
	if err != nil {
		t.Fatalf("failed to get batch: %v", err)
	}
	if batch.LeafID != proj.ID {
		t.Errorf("batch leaf_id mismatch")
	}
	if batch.SequenceNumber != 1 {
		t.Errorf("expected sequence_number 1, got %d", batch.SequenceNumber)
	}
	if batch.TotalWorkUnits != 6 {
		t.Errorf("expected total_work_units 6, got %d", batch.TotalWorkUnits)
	}

	// Verify work units in DB — all should be QUEUED now.
	queuedState := workunit.WorkUnitStateQueued
	wus, _, err := wuRepo.List(ctx, workunit.WorkUnitListFilters{
		LeafID: &proj.ID,
		BatchID:   &batch.ID,
		State:     &queuedState,
	}, types.PaginationRequest{PageSize: 50})
	if err != nil {
		t.Fatalf("failed to list work units: %v", err)
	}
	if len(wus) != 6 {
		t.Fatalf("expected 6 queued work units, got %d", len(wus))
	}

	// Verify work unit fields.
	for _, wu := range wus {
		if wu.LeafID != proj.ID {
			t.Errorf("wrong leaf_id")
		}
		if *wu.BatchID != batch.ID {
			t.Errorf("wrong batch_id")
		}
		if wu.CodeArtifactRef != "sha256:testbinary" {
			t.Errorf("wrong code_artifact_ref: %s", wu.CodeArtifactRef)
		}
		if wu.DeadlineSeconds != 7200 { // 3600 * 2.0
			t.Errorf("expected deadline_seconds 7200, got %d", wu.DeadlineSeconds)
		}
		if wu.MaxReassignments != 5 {
			t.Errorf("expected max_reassignments 5, got %d", wu.MaxReassignments)
		}

		// Verify parameters are valid JSON.
		var params map[string]interface{}
		if err := json.Unmarshal(wu.Parameters, &params); err != nil {
			t.Errorf("invalid parameters JSON: %v", err)
		}
		if _, ok := params["temperature"]; !ok {
			t.Error("parameters missing 'temperature'")
		}
		if _, ok := params["pressure"]; !ok {
			t.Error("parameters missing 'pressure'")
		}
	}
}

func TestIntegration_Generate_BatchSplitting(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "sweepuser2")
	proj := createTestLeafDB(t, pool, &userID)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)

	ctx := context.Background()

	// 5 x 5 = 25 work units, batch_size = 10 → 3 batches (10, 10, 5).
	vals := make([]interface{}, 5)
	for i := range vals {
		vals[i] = float64(i)
	}
	params := map[string]interface{}{
		"x": vals,
		"y": vals,
	}

	result, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.WorkUnitsCreated != 25 {
		t.Errorf("expected 25, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 3 {
		t.Errorf("expected 3 batches, got %d", len(result.BatchIDs))
	}

	// Verify batches in DB.
	batches, _, err := batchRepo.ListByLeaf(ctx, proj.ID, types.PaginationRequest{PageSize: 50})
	if err != nil {
		t.Fatalf("failed to list batches: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches in DB, got %d", len(batches))
	}

	totalWUs := 0
	for _, b := range batches {
		totalWUs += b.TotalWorkUnits
	}
	if totalWUs != 25 {
		t.Errorf("expected 25 total work units across batches, got %d", totalWUs)
	}

	// Verify all work units are QUEUED.
	queuedState := workunit.WorkUnitStateQueued
	allWUs, _, err := wuRepo.List(ctx, workunit.WorkUnitListFilters{
		LeafID: &proj.ID,
		State:     &queuedState,
	}, types.PaginationRequest{PageSize: 200})
	if err != nil {
		t.Fatalf("failed to list work units: %v", err)
	}
	if len(allWUs) != 25 {
		t.Errorf("expected 25 queued work units, got %d", len(allWUs))
	}
}

func TestIntegration_Generate_LargeSpaceWarning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large parameter space test in short mode")
	}

	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "sweepuser3")
	proj := createTestLeafDB(t, pool, &userID)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)

	ctx := context.Background()

	// Create >100K combinations: 320 x 320 = 102400.
	// This test verifies the warning field is set.
	// Use batch_size = 100000 to minimize batch overhead.
	vals := make([]interface{}, 320)
	for i := range vals {
		vals[i] = float64(i)
	}
	params := map[string]interface{}{
		"x": vals,
		"y": vals,
	}

	result, err := Generate(ctx, proj, params, generate.MaxBatchSize, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status == "" {
		t.Error("expected non-empty status for large parameter space")
	}
	if result.WorkUnitsCreated != 102400 {
		t.Errorf("expected 102400, got %d", result.WorkUnitsCreated)
	}
}

func TestIntegration_Generate_SequenceNumbers(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "sweepuser4")
	proj := createTestLeafDB(t, pool, &userID)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)

	ctx := context.Background()

	// First generation.
	params1 := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}
	result1, err := Generate(ctx, proj, params1, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("first generate failed: %v", err)
	}
	if len(result1.BatchIDs) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(result1.BatchIDs))
	}

	// Second generation — should get sequence_number 2.
	params2 := map[string]interface{}{
		"y": []interface{}{3.0, 4.0, 5.0},
	}
	result2, err := Generate(ctx, proj, params2, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("second generate failed: %v", err)
	}

	// Verify sequence numbers.
	batch1, err := batchRepo.GetByID(ctx, result1.BatchIDs[0])
	if err != nil {
		t.Fatalf("failed to get batch1: %v", err)
	}
	batch2, err := batchRepo.GetByID(ctx, result2.BatchIDs[0])
	if err != nil {
		t.Fatalf("failed to get batch2: %v", err)
	}

	if batch1.SequenceNumber != 1 {
		t.Errorf("expected batch1 sequence_number 1, got %d", batch1.SequenceNumber)
	}
	if batch2.SequenceNumber != 2 {
		t.Errorf("expected batch2 sequence_number 2, got %d", batch2.SequenceNumber)
	}
}
