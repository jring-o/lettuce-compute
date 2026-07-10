//go:build integration

package credit

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
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
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
			'{"credit_per_validated_work_unit":1.5}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $5
		)`,
		id, "Test Leaf "+slug, slug, "A test leaf for credit tests", creatorID,
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
			$1, $2, 'COMPLETED', 'NORMAL', $3, $4, $5, 3600, 3
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

func createTestResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, checksum string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO results (
			id, work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'AGREED'
		)`,
		id, wuID, volID,
		json.RawMessage(`{"answer": 42}`),
		checksum,
		json.RawMessage(`{"wall_clock_seconds":3600,"cpu_seconds_user":3200,"cpu_seconds_system":50,"cpu_cores_used":4,"peak_memory_mb":2048}`),
	)
	if err != nil {
		t.Fatalf("failed to create test result: %v", err)
	}
	return id
}

func TestCreditCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator1")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)
	resultID := createTestResult(t, pool, wuID, volID, "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:    leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: 1.5,
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
	if entry.GrantedAt.IsZero() {
		t.Error("GrantedAt should be set")
	}
	if entry.CreditAmount != 1.5 {
		t.Errorf("CreditAmount = %v, want 1.5", entry.CreditAmount)
	}
}

func TestCreditGetByResultID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator2")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)
	resultID := createTestResult(t, pool, wuID, volID, "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:    leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: 2.0,
	}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByResultID(ctx, resultID)
	if err != nil {
		t.Fatalf("GetByResultID: %v", err)
	}
	if got.ID != entry.ID {
		t.Errorf("ID = %v, want %v", got.ID, entry.ID)
	}
	if got.CreditAmount != 2.0 {
		t.Errorf("CreditAmount = %v, want 2.0", got.CreditAmount)
	}
}

func TestCreditGetByResultIDNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByResultID(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent result ID")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestCreditSumByVolunteerProject(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator3")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Sum with no entries should be 0.
	sum, err := repo.SumByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("SumByVolunteerProject (empty): %v", err)
	}
	if sum != 0 {
		t.Errorf("expected 0, got %v", sum)
	}

	// Create two credit entries.
	for i := 0; i < 2; i++ {
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID,
			"abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789"+string(rune('0'+i)))
		entry := &LedgerEntry{
			VolunteerID:  volID,
			LeafID:    leafID,
			WorkUnitID:   wuID,
			ResultID:     resultID,
			CreditAmount: 1.5,
		}
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	sum, err = repo.SumByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("SumByVolunteerProject: %v", err)
	}
	if sum != 3.0 {
		t.Errorf("expected 3.0, got %v", sum)
	}
}

func TestCreditUniqueResultConstraint(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator4")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)
	resultID := createTestResult(t, pool, wuID, volID, "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry1 := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:    leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: 1.0,
	}
	if err := repo.Create(ctx, entry1); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second entry with the same result_id should fail.
	entry2 := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:    leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: 1.0,
	}
	err := repo.Create(ctx, entry2)
	if err == nil {
		t.Fatal("expected error for duplicate result_id")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestCreditOnDeleteRestrict(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator5")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)
	resultID := createTestResult(t, pool, wuID, volID, "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	entry := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:    leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: 1.0,
	}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Attempting to delete the volunteer should fail due to ON DELETE RESTRICT.
	_, err := pool.Exec(ctx, "DELETE FROM volunteers WHERE id = $1", volID)
	if err == nil {
		t.Fatal("expected error due to ON DELETE RESTRICT on credit_ledger")
	}
}

func TestCreditListByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator6")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 3 credit entries.
	for i := 0; i < 3; i++ {
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID,
			"abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789"+string(rune('0'+i)))
		entry := &LedgerEntry{
			VolunteerID:  volID,
			LeafID:    leafID,
			WorkUnitID:   wuID,
			ResultID:     resultID,
			CreditAmount: 1.0,
		}
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	entries, pagination, err := repo.ListByVolunteer(ctx, volID, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("page 1: expected 2, got %d", len(entries))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	entries2, pagination2, err := repo.ListByVolunteer(ctx, volID, types.PaginationRequest{PageSize: 2, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("ListByVolunteer page 2: %v", err)
	}
	if len(entries2) != 1 {
		t.Fatalf("page 2: expected 1, got %d", len(entries2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}

func TestCreditListByLeaf(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "credit-creator7")
	leafID := createTestLeaf(t, pool, &userID)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create entries from 2 different volunteers.
	for i := 0; i < 2; i++ {
		volID := createTestVolunteer(t, pool)
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID,
			"abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789"+string(rune('0'+i)))
		entry := &LedgerEntry{
			VolunteerID:  volID,
			LeafID:    leafID,
			WorkUnitID:   wuID,
			ResultID:     resultID,
			CreditAmount: 1.0,
		}
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	entries, _, err := repo.ListByLeaf(ctx, leafID, types.PaginationRequest{PageSize: 50})
	if err != nil {
		t.Fatalf("ListByLeaf: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}
}
