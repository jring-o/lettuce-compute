//go:build integration

package result

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
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

func createTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID) types.ID {
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
			'{"runtime":"NATIVE","gpu_required":false}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $5
		)`,
		id, "Test Leaf "+slug, slug, "A test leaf for result tests", creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

func createTestWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority, input_data, code_artifact_ref,
			parameters, deadline_seconds, max_reassignments
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL', $3, $4, $5, 3600, 3
		)`,
		id, leafID,
		json.RawMessage(`{"x": 42}`),
		"ref://test-binary",
		json.RawMessage(`{"iterations": 1000}`),
	)
	if err != nil {
		t.Fatalf("failed to create test work unit: %v", err)
	}
	return id
}

func createTestVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	pubKey := make([]byte, 32)
	copy(pubKey, uuid.New().NodeID())
	copy(pubKey[6:], uuid.New().NodeID())
	copy(pubKey[12:], uuid.New().NodeID())
	copy(pubKey[18:], uuid.New().NodeID())
	copy(pubKey[24:], uuid.New().NodeID())
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at
		) VALUES (
			$1, $2, $3, $4, 'ALWAYS', true, $5
		)`,
		id, pubKey,
		json.RawMessage(`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`),
		[]string{"NATIVE", "CONTAINER"},
		now,
	)
	if err != nil {
		t.Fatalf("failed to create test volunteer: %v", err)
	}
	return id
}

func sampleMetadata() ExecutionMetadata {
	return ExecutionMetadata{
		WallClockSeconds: 3600,
		CPUSecondsUser:   3200,
		CPUSecondsSystem: 50,
		CPUCoresUsed:     4,
		PeakMemoryMB:     2048,
		DiskReadMB:       500,
		DiskWriteMB:      100,
		NetworkRxMB:      10,
		NetworkTxMB:      5,
	}
}

func TestResultCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator1")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	r := &Result{
		WorkUnitID:        wuID,
		VolunteerID:       volID,
		OutputData:        json.RawMessage(`{"answer": 42}`),
		OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}

	err := repo.Create(ctx, r)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(r.ID) {
		t.Error("ID should be set after Create")
	}
	if r.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if r.ValidationStatus != ValidationPending {
		t.Errorf("ValidationStatus = %q, want PENDING", r.ValidationStatus)
	}
}

func TestResultGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator2")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	r := &Result{
		WorkUnitID:        wuID,
		VolunteerID:       volID,
		OutputData:        json.RawMessage(`{"answer": 42}`),
		OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("ID = %v, want %v", got.ID, r.ID)
	}
	if got.WorkUnitID != wuID {
		t.Errorf("WorkUnitID = %v, want %v", got.WorkUnitID, wuID)
	}
	if got.VolunteerID != volID {
		t.Errorf("VolunteerID = %v, want %v", got.VolunteerID, volID)
	}
	if got.OutputChecksum != r.OutputChecksum {
		t.Errorf("OutputChecksum = %q, want %q", got.OutputChecksum, r.OutputChecksum)
	}
	if got.ExecutionMetadata.WallClockSeconds != 3600 {
		t.Errorf("WallClockSeconds = %v, want 3600", got.ExecutionMetadata.WallClockSeconds)
	}
}

func TestResultGetByIDNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestResultListByWorkUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator3")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	for i, volID := range []types.ID{vol1, vol2} {
		r := &Result{
			WorkUnitID:        wuID,
			VolunteerID:       volID,
			OutputData:        json.RawMessage(`{"answer": 42}`),
			OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789" + string(rune('0'+i)),
			ExecutionMetadata: sampleMetadata(),
			ValidationStatus:  ValidationPending,
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	results, err := repo.ListByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("ListByWorkUnit: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestResultListByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator4")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 3 results for the same volunteer, different work units.
	for i := 0; i < 3; i++ {
		wuID := createTestWorkUnit(t, pool, leafID)
		r := &Result{
			WorkUnitID:        wuID,
			VolunteerID:       volID,
			OutputData:        json.RawMessage(`{"answer": 42}`),
			OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			ExecutionMetadata: sampleMetadata(),
			ValidationStatus:  ValidationPending,
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	results, pagination, err := repo.ListByVolunteer(ctx, volID, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("page 1: expected 2 results, got %d", len(results))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	results2, pagination2, err := repo.ListByVolunteer(ctx, volID, types.PaginationRequest{PageSize: 2, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("ListByVolunteer page 2: %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("page 2: expected 1 result, got %d", len(results2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}

func TestResultCountByWorkUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator5")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	count, err := repo.CountByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("CountByWorkUnit: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 results, got %d", count)
	}

	for i, volID := range []types.ID{vol1, vol2} {
		r := &Result{
			WorkUnitID:        wuID,
			VolunteerID:       volID,
			OutputData:        json.RawMessage(`{"answer": 42}`),
			OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789" + string(rune('0'+i)),
			ExecutionMetadata: sampleMetadata(),
			ValidationStatus:  ValidationPending,
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	count, err = repo.CountByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("CountByWorkUnit: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 results, got %d", count)
	}
}

func TestResultCountPendingByWorkUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator6")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create two results.
	var resultIDs []types.ID
	for i, volID := range []types.ID{vol1, vol2} {
		r := &Result{
			WorkUnitID:        wuID,
			VolunteerID:       volID,
			OutputData:        json.RawMessage(`{"answer": 42}`),
			OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789" + string(rune('0'+i)),
			ExecutionMetadata: sampleMetadata(),
			ValidationStatus:  ValidationPending,
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		resultIDs = append(resultIDs, r.ID)
	}

	count, err := repo.CountPendingByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("CountPendingByWorkUnit: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 pending, got %d", count)
	}

	// Mark one as AGREED.
	if err := repo.UpdateValidationStatus(ctx, resultIDs[0], ValidationAgreed); err != nil {
		t.Fatalf("UpdateValidationStatus: %v", err)
	}

	count, err = repo.CountPendingByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("CountPendingByWorkUnit after update: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 pending after marking one AGREED, got %d", count)
	}
}

func TestResultUpdateValidationStatus(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator7")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	r := &Result{
		WorkUnitID:        wuID,
		VolunteerID:       volID,
		OutputData:        json.RawMessage(`{"answer": 42}`),
		OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.UpdateValidationStatus(ctx, r.ID, ValidationAgreed); err != nil {
		t.Fatalf("UpdateValidationStatus: %v", err)
	}

	got, err := repo.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.ValidationStatus != ValidationAgreed {
		t.Errorf("ValidationStatus = %q, want AGREED", got.ValidationStatus)
	}
	if got.ValidatedAt == nil {
		t.Error("ValidatedAt should be set after validation status update")
	}
}

func TestResultUpdateValidationStatusNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.UpdateValidationStatus(ctx, types.NewID(), ValidationAgreed)
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestResultUniqueConstraint(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator8")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	r1 := &Result{
		WorkUnitID:        wuID,
		VolunteerID:       volID,
		OutputData:        json.RawMessage(`{"answer": 42}`),
		OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}
	if err := repo.Create(ctx, r1); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second result with same (work_unit_id, volunteer_id) should fail.
	r2 := &Result{
		WorkUnitID:        wuID,
		VolunteerID:       volID,
		OutputData:        json.RawMessage(`{"answer": 99}`),
		OutputChecksum:    "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}
	err := repo.Create(ctx, r2)
	if err == nil {
		t.Fatal("expected error for duplicate (work_unit_id, volunteer_id)")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestResultListByLeaf(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator-lbp")
	leafID := createTestLeaf(t, pool, &userID)
	wuID1 := createTestWorkUnit(t, pool, leafID)
	wuID2 := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 3 results: 2 for wuID1 (PENDING, AGREED), 1 for wuID2 (PENDING).
	r1 := &Result{
		WorkUnitID: wuID1, VolunteerID: vol1,
		OutputData: json.RawMessage(`{"v":1}`), OutputChecksum: "aaaa1234567890abcdef1234567890abcdef1234567890abcdef1234567890ab",
		ExecutionMetadata: sampleMetadata(), ValidationStatus: ValidationPending,
	}
	if err := repo.Create(ctx, r1); err != nil {
		t.Fatalf("Create r1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	r2 := &Result{
		WorkUnitID: wuID1, VolunteerID: vol2,
		OutputData: json.RawMessage(`{"v":2}`), OutputChecksum: "bbbb1234567890abcdef1234567890abcdef1234567890abcdef1234567890ab",
		ExecutionMetadata: sampleMetadata(), ValidationStatus: ValidationPending,
	}
	if err := repo.Create(ctx, r2); err != nil {
		t.Fatalf("Create r2: %v", err)
	}
	if err := repo.UpdateValidationStatus(ctx, r2.ID, ValidationAgreed); err != nil {
		t.Fatalf("Update r2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	r3 := &Result{
		WorkUnitID: wuID2, VolunteerID: vol1,
		OutputData: json.RawMessage(`{"v":3}`), OutputChecksum: "cccc1234567890abcdef1234567890abcdef1234567890abcdef1234567890ab",
		ExecutionMetadata: sampleMetadata(), ValidationStatus: ValidationPending,
	}
	if err := repo.Create(ctx, r3); err != nil {
		t.Fatalf("Create r3: %v", err)
	}

	// Test: list all results for the project.
	results, pagination, err := repo.ListByLeaf(ctx, leafID, ResultFilters{}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("ListByLeaf (all): %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if pagination.HasMore {
		t.Error("HasMore should be false for 3 results with default page size")
	}

	// Test: filter by validation_status=AGREED.
	agreed := ValidationAgreed
	results, _, err = repo.ListByLeaf(ctx, leafID, ResultFilters{ValidationStatus: &agreed}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("ListByLeaf (AGREED): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 AGREED result, got %d", len(results))
	}
	if results[0].ID != r2.ID {
		t.Errorf("expected result %v, got %v", r2.ID, results[0].ID)
	}

	// Test: filter by work_unit_id.
	results, _, err = repo.ListByLeaf(ctx, leafID, ResultFilters{WorkUnitID: &wuID2}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("ListByLeaf (wuID2): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for wuID2, got %d", len(results))
	}

	// Test: pagination (page size 2).
	results, pagination, err = repo.ListByLeaf(ctx, leafID, ResultFilters{}, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByLeaf page 1: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("page 1: expected 2 results, got %d", len(results))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	results2, pagination2, err := repo.ListByLeaf(ctx, leafID, ResultFilters{}, types.PaginationRequest{PageSize: 2, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("ListByLeaf page 2: %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("page 2: expected 1 result, got %d", len(results2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}

	// Test: empty result for non-existent project.
	results, _, err = repo.ListByLeaf(ctx, types.NewID(), ResultFilters{}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("ListByLeaf (nonexistent): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for nonexistent project, got %d", len(results))
	}
}

func TestResultBatchUpdateValidationStatus(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-creator9")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	var ids []types.ID
	for i, volID := range []types.ID{vol1, vol2} {
		r := &Result{
			WorkUnitID:        wuID,
			VolunteerID:       volID,
			OutputData:        json.RawMessage(`{"answer": 42}`),
			OutputChecksum:    "abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789" + string(rune('0'+i)),
			ExecutionMetadata: sampleMetadata(),
			ValidationStatus:  ValidationPending,
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}

	if err := repo.BatchUpdateValidationStatus(ctx, ids, ValidationAgreed); err != nil {
		t.Fatalf("BatchUpdateValidationStatus: %v", err)
	}

	for _, id := range ids {
		got, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID %v: %v", id, err)
		}
		if got.ValidationStatus != ValidationAgreed {
			t.Errorf("result %v: ValidationStatus = %q, want AGREED", id, got.ValidationStatus)
		}
		if got.ValidatedAt == nil {
			t.Errorf("result %v: ValidatedAt should be set", id)
		}
	}
}

// TestResultTrustSnapshot proves the submission-time trust snapshot columns
// (trust_subject, trust_score_at_submit) persist on Create and round-trip through
// GetByID, and that a result created without them stores NULL and reads back as nil
// (the legacy / pre-feature row shape).
func TestResultTrustSnapshot(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "result-trust")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	wuID2 := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// With the snapshot set.
	subject := "did:plc:trusted"
	score := 42
	r := &Result{
		WorkUnitID:         wuID,
		VolunteerID:        vol1,
		OutputData:         json.RawMessage(`{"answer": 42}`),
		OutputChecksum:     "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		ExecutionMetadata:  sampleMetadata(),
		ValidationStatus:   ValidationPending,
		TrustSubject:       &subject,
		TrustScoreAtSubmit: &score,
	}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("Create with trust snapshot: %v", err)
	}
	// Create's RETURNING already populates the fields; verify there.
	if r.TrustSubject == nil || *r.TrustSubject != subject {
		t.Errorf("Create RETURNING TrustSubject = %v, want %q", r.TrustSubject, subject)
	}
	if r.TrustScoreAtSubmit == nil || *r.TrustScoreAtSubmit != score {
		t.Errorf("Create RETURNING TrustScoreAtSubmit = %v, want %d", r.TrustScoreAtSubmit, score)
	}

	got, err := repo.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.TrustSubject == nil || *got.TrustSubject != subject {
		t.Errorf("GetByID TrustSubject = %v, want %q", got.TrustSubject, subject)
	}
	if got.TrustScoreAtSubmit == nil || *got.TrustScoreAtSubmit != score {
		t.Errorf("GetByID TrustScoreAtSubmit = %v, want %d", got.TrustScoreAtSubmit, score)
	}

	// Without the snapshot: NULL round-trips as nil.
	rLegacy := &Result{
		WorkUnitID:        wuID2,
		VolunteerID:       vol2,
		OutputData:        json.RawMessage(`{"answer": 7}`),
		OutputChecksum:    "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		ExecutionMetadata: sampleMetadata(),
		ValidationStatus:  ValidationPending,
	}
	if err := repo.Create(ctx, rLegacy); err != nil {
		t.Fatalf("Create without trust snapshot: %v", err)
	}
	gotLegacy, err := repo.GetByID(ctx, rLegacy.ID)
	if err != nil {
		t.Fatalf("GetByID legacy: %v", err)
	}
	if gotLegacy.TrustSubject != nil {
		t.Errorf("legacy TrustSubject = %v, want nil", *gotLegacy.TrustSubject)
	}
	if gotLegacy.TrustScoreAtSubmit != nil {
		t.Errorf("legacy TrustScoreAtSubmit = %v, want nil", *gotLegacy.TrustScoreAtSubmit)
	}
}
