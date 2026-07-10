//go:build integration

package assignment

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
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
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
		id, "Test Leaf "+slug, slug, "A test leaf for assignment tests", creatorID,
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
			$1, $2, 'QUEUED', 'NORMAL', $3, $4, $5, 3600, 3
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

func TestAssignmentCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator1")
	leafID := createTestLeaf(t, pool, &userID)
	workUnitID := createTestWorkUnit(t, pool, leafID)
	volunteerID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	now := time.Now().UTC()
	entry := &AssignmentHistoryEntry{
		WorkUnitID:  workUnitID,
		VolunteerID: volunteerID,
		AssignedAt:  now,
	}

	err := repo.Create(ctx, entry)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(entry.ID) {
		t.Error("ID should be set after Create")
	}
	if entry.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if entry.Outcome != nil {
		t.Error("Outcome should be nil for new assignment")
	}
}

func TestAssignmentGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator2")
	leafID := createTestLeaf(t, pool, &userID)
	workUnitID := createTestWorkUnit(t, pool, leafID)
	volunteerID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry := &AssignmentHistoryEntry{
		WorkUnitID:  workUnitID,
		VolunteerID: volunteerID,
		AssignedAt:  time.Now().UTC(),
	}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, entry.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != entry.ID {
		t.Errorf("ID = %v, want %v", got.ID, entry.ID)
	}
	if got.WorkUnitID != workUnitID {
		t.Errorf("WorkUnitID = %v, want %v", got.WorkUnitID, workUnitID)
	}
	if got.VolunteerID != volunteerID {
		t.Errorf("VolunteerID = %v, want %v", got.VolunteerID, volunteerID)
	}
}

func TestAssignmentGetByIDNotFound(t *testing.T) {
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

func TestAssignmentListByWorkUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator3")
	leafID := createTestLeaf(t, pool, &userID)
	workUnitID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create two assignments for the same work unit.
	now := time.Now().UTC()
	for _, volID := range []types.ID{vol1, vol2} {
		entry := &AssignmentHistoryEntry{
			WorkUnitID:  workUnitID,
			VolunteerID: volID,
			AssignedAt:  now,
		}
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("Create: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	entries, err := repo.ListByWorkUnit(ctx, workUnitID)
	if err != nil {
		t.Fatalf("ListByWorkUnit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestAssignmentCountActiveByWorkUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator4")
	leafID := createTestLeaf(t, pool, &userID)
	workUnitID := createTestWorkUnit(t, pool, leafID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create two assignments, one active and one completed.
	entry1 := &AssignmentHistoryEntry{
		WorkUnitID:  workUnitID,
		VolunteerID: vol1,
		AssignedAt:  time.Now().UTC(),
	}
	if err := repo.Create(ctx, entry1); err != nil {
		t.Fatalf("Create entry1: %v", err)
	}

	entry2 := &AssignmentHistoryEntry{
		WorkUnitID:  workUnitID,
		VolunteerID: vol2,
		AssignedAt:  time.Now().UTC(),
	}
	if err := repo.Create(ctx, entry2); err != nil {
		t.Fatalf("Create entry2: %v", err)
	}

	// Complete one assignment.
	if err := repo.UpdateOutcome(ctx, entry1.ID, OutcomeCompleted, nil); err != nil {
		t.Fatalf("UpdateOutcome: %v", err)
	}

	count, err := repo.CountActiveByWorkUnit(ctx, workUnitID)
	if err != nil {
		t.Fatalf("CountActiveByWorkUnit: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 active assignment, got %d", count)
	}
}

func TestAssignmentUpdateOutcome(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator5")
	leafID := createTestLeaf(t, pool, &userID)
	workUnitID := createTestWorkUnit(t, pool, leafID)
	volunteerID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry := &AssignmentHistoryEntry{
		WorkUnitID:  workUnitID,
		VolunteerID: volunteerID,
		AssignedAt:  time.Now().UTC(),
	}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update outcome to EXPIRED.
	if err := repo.UpdateOutcome(ctx, entry.ID, OutcomeExpired, nil); err != nil {
		t.Fatalf("UpdateOutcome: %v", err)
	}

	got, err := repo.GetByID(ctx, entry.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Outcome == nil || *got.Outcome != OutcomeExpired {
		t.Errorf("Outcome = %v, want EXPIRED", got.Outcome)
	}
	if got.OutcomeAt == nil {
		t.Error("OutcomeAt should be set after outcome update")
	}
}

func TestAssignmentUpdateOutcomeNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.UpdateOutcome(ctx, types.NewID(), OutcomeCompleted, nil)
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

func TestAssignmentListByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-creator6")
	leafID := createTestLeaf(t, pool, &userID)
	volunteerID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 3 assignments for the same volunteer.
	for i := 0; i < 3; i++ {
		wuID := createTestWorkUnit(t, pool, leafID)
		entry := &AssignmentHistoryEntry{
			WorkUnitID:  wuID,
			VolunteerID: volunteerID,
			AssignedAt:  time.Now().UTC(),
		}
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	entries, pagination, err := repo.ListByVolunteer(ctx, volunteerID, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("page 1: expected 2 entries, got %d", len(entries))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	// Page 2.
	entries2, pagination2, err := repo.ListByVolunteer(ctx, volunteerID, types.PaginationRequest{PageSize: 2, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("ListByVolunteer page 2: %v", err)
	}
	if len(entries2) != 1 {
		t.Fatalf("page 2: expected 1 entry, got %d", len(entries2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}
