//go:build integration

package workunit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
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

// createTestUser inserts a minimal user for FK references and returns the user ID.
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

// createTestLeaf inserts a minimal leaf and returns the leaf ID.
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
			$1, $2, $3, $4, 'DRAFT', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $5
		)`,
		id, "Test Leaf "+slug, slug, "A test leaf", creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

func newTestWorkUnit(leafID types.ID, batchID *types.ID) *WorkUnit {
	return &WorkUnit{
		LeafID:        leafID,
		BatchID:          batchID,
		State:            WorkUnitStateCreated,
		Priority:         WorkUnitPriorityNormal,
		InputData:        json.RawMessage(`{"x": 42}`),
		CodeArtifactRef:  "ref://test-binary-" + uuid.New().String()[:8],
		Parameters:       json.RawMessage(`{"iterations": 1000}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
	}
}

func newTestBatch(leafID types.ID, seqNum, totalWUs int) *Batch {
	return &Batch{
		LeafID:      leafID,
		SequenceNumber: seqNum,
		TotalWorkUnits: totalWUs,
	}
}

// --- Work Unit Repository Tests ---

func TestWorkUnitCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wucreator1")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	err := repo.Create(ctx, wu)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(wu.ID) {
		t.Error("ID should be set after Create")
	}
	if wu.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if wu.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}
	if wu.State != WorkUnitStateCreated {
		t.Errorf("State = %s, want CREATED", wu.State)
	}
	if wu.Priority != WorkUnitPriorityNormal {
		t.Errorf("Priority = %s, want NORMAL", wu.Priority)
	}
	if wu.LeafID != leafID {
		t.Errorf("LeafID = %v, want %v", wu.LeafID, leafID)
	}
	if wu.DeadlineSeconds != 3600 {
		t.Errorf("DeadlineSeconds = %d, want 3600", wu.DeadlineSeconds)
	}
	if wu.MaxReassignments != 3 {
		t.Errorf("MaxReassignments = %d, want 3", wu.MaxReassignments)
	}
	if wu.ReassignmentCount != 0 {
		t.Errorf("ReassignmentCount = %d, want 0", wu.ReassignmentCount)
	}
	if wu.FlaggedForReview {
		t.Error("FlaggedForReview should be false")
	}
}

func TestWorkUnitGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wuget1")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != wu.ID {
		t.Errorf("ID = %v, want %v", got.ID, wu.ID)
	}
	if got.CodeArtifactRef != wu.CodeArtifactRef {
		t.Errorf("CodeArtifactRef = %s, want %s", got.CodeArtifactRef, wu.CodeArtifactRef)
	}
}

func TestWorkUnitGetByIDNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxWorkUnitRepository(pool)
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

func TestWorkUnitListPagination(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistpag")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create 5 work units.
	for i := 0; i < 5; i++ {
		wu := newTestWorkUnit(leafID, nil)
		if err := repo.Create(ctx, wu); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	filter := WorkUnitListFilters{LeafID: &leafID}

	// Page 1: 3 items.
	wus, pagination, err := repo.List(ctx, filter, types.PaginationRequest{PageSize: 3})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(wus) != 3 {
		t.Fatalf("page 1: got %d work units, want 3", len(wus))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}
	if pagination.NextCursor == "" {
		t.Error("page 1: NextCursor should be set")
	}

	// Page 2: remaining 2 items.
	wus2, pagination2, err := repo.List(ctx, filter, types.PaginationRequest{PageSize: 3, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(wus2) != 2 {
		t.Fatalf("page 2: got %d work units, want 2", len(wus2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}

	// No overlap.
	seen := make(map[types.ID]bool)
	for _, wu := range wus {
		seen[wu.ID] = true
	}
	for _, wu := range wus2 {
		if seen[wu.ID] {
			t.Errorf("duplicate work unit %v across pages", wu.ID)
		}
	}
}

func TestWorkUnitListFilterByProjectID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistproj")
	proj1 := createTestLeaf(t, pool, &userID)
	proj2 := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu1 := newTestWorkUnit(proj1, nil)
	if err := repo.Create(ctx, wu1); err != nil {
		t.Fatalf("Create wu1: %v", err)
	}
	wu2 := newTestWorkUnit(proj2, nil)
	if err := repo.Create(ctx, wu2); err != nil {
		t.Fatalf("Create wu2: %v", err)
	}

	wus, _, err := repo.List(ctx, WorkUnitListFilters{LeafID: &proj1}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, wu := range wus {
		if wu.LeafID != proj1 {
			t.Errorf("expected leaf %v, got %v", proj1, wu.LeafID)
		}
	}
}

func TestWorkUnitListFilterByState(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wuliststate")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	state := WorkUnitStateCreated
	wus, _, err := repo.List(ctx, WorkUnitListFilters{
		LeafID: &leafID,
		State:     &state,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, w := range wus {
		if w.State != WorkUnitStateCreated {
			t.Errorf("expected CREATED, got %s", w.State)
		}
	}
}

func TestWorkUnitListFilterByBatchID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistbatch")
	leafID := createTestLeaf(t, pool, &userID)
	batchRepo := NewPgxBatchRepository(pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	batch := newTestBatch(leafID, 1, 10)
	if err := batchRepo.Create(ctx, batch); err != nil {
		t.Fatalf("Create batch: %v", err)
	}

	wu1 := newTestWorkUnit(leafID, &batch.ID)
	if err := repo.Create(ctx, wu1); err != nil {
		t.Fatalf("Create wu1: %v", err)
	}
	wu2 := newTestWorkUnit(leafID, nil) // no batch
	if err := repo.Create(ctx, wu2); err != nil {
		t.Fatalf("Create wu2: %v", err)
	}

	wus, _, err := repo.List(ctx, WorkUnitListFilters{BatchID: &batch.ID}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(wus) != 1 {
		t.Fatalf("expected 1 work unit in batch, got %d", len(wus))
	}
	if wus[0].ID != wu1.ID {
		t.Errorf("expected work unit %v, got %v", wu1.ID, wus[0].ID)
	}
}

func TestWorkUnitListFilterByPriority(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistpri")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	normal := newTestWorkUnit(leafID, nil)
	normal.Priority = WorkUnitPriorityNormal
	if err := repo.Create(ctx, normal); err != nil {
		t.Fatalf("Create normal: %v", err)
	}

	high := newTestWorkUnit(leafID, nil)
	high.Priority = WorkUnitPriorityHigh
	if err := repo.Create(ctx, high); err != nil {
		t.Fatalf("Create high: %v", err)
	}

	pri := WorkUnitPriorityHigh
	wus, _, err := repo.List(ctx, WorkUnitListFilters{
		LeafID: &leafID,
		Priority:  &pri,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(wus) != 1 {
		t.Fatalf("expected 1 HIGH work unit, got %d", len(wus))
	}
	if wus[0].ID != high.ID {
		t.Errorf("expected work unit %v, got %v", high.ID, wus[0].ID)
	}
}

func TestWorkUnitListFilterByFlaggedForReview(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistflag")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Flag it via direct DB update (simulating FAILED state).
	_, err := pool.Exec(ctx,
		"UPDATE work_units SET flagged_for_review = true WHERE id = $1", wu.ID)
	if err != nil {
		t.Fatalf("flag work unit: %v", err)
	}

	flagged := true
	wus, _, err := repo.List(ctx, WorkUnitListFilters{
		LeafID:        &leafID,
		FlaggedForReview: &flagged,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(wus) != 1 {
		t.Fatalf("expected 1 flagged work unit, got %d", len(wus))
	}
}

func TestWorkUnitListPriorityOrderingForQueued(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistqueueord")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create work units with different priorities, all QUEUED.
	priorities := []WorkUnitPriority{
		WorkUnitPriorityNormal,
		WorkUnitPriorityCritical,
		WorkUnitPriorityHigh,
	}
	for _, pri := range priorities {
		wu := newTestWorkUnit(leafID, nil)
		wu.State = WorkUnitStateQueued
		wu.Priority = pri
		if err := repo.Create(ctx, wu); err != nil {
			t.Fatalf("Create %s: %v", pri, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	state := WorkUnitStateQueued
	wus, _, err := repo.List(ctx, WorkUnitListFilters{
		LeafID: &leafID,
		State:     &state,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(wus) != 3 {
		t.Fatalf("expected 3 QUEUED work units, got %d", len(wus))
	}

	// Should be ordered: CRITICAL, HIGH, NORMAL.
	if wus[0].Priority != WorkUnitPriorityCritical {
		t.Errorf("first should be CRITICAL, got %s", wus[0].Priority)
	}
	if wus[1].Priority != WorkUnitPriorityHigh {
		t.Errorf("second should be HIGH, got %s", wus[1].Priority)
	}
	if wus[2].Priority != WorkUnitPriorityNormal {
		t.Errorf("third should be NORMAL, got %s", wus[2].Priority)
	}
}

func TestWorkUnitUpdateStateValidTransitions(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wuupdstate")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Walk through the success path: CREATED → QUEUED → ASSIGNED → RUNNING → COMPLETED → VALIDATED.
	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	transitions := []struct {
		from WorkUnitState
		to   WorkUnitState
	}{
		{WorkUnitStateCreated, WorkUnitStateQueued},
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateRunning},
		{WorkUnitStateRunning, WorkUnitStateCompleted},
		{WorkUnitStateCompleted, WorkUnitStateValidated},
	}

	for _, tr := range transitions {
		updated, err := repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
		if updated.State != tr.to {
			t.Errorf("State = %s, want %s", updated.State, tr.to)
		}
		wu = updated
	}
}

func TestWorkUnitUpdateStateInvalidTransition(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wuupdinvalid")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// CREATED → RUNNING is invalid.
	_, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateRunning)
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestWorkUnitUpdateStateConcurrentChange(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wuupdconc")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Transition to QUEUED first.
	if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
		t.Fatalf("UpdateState to QUEUED: %v", err)
	}

	// Simulate concurrent change: directly update state in DB.
	_, err := pool.Exec(ctx, "UPDATE work_units SET state = 'ASSIGNED' WHERE id = $1", wu.ID)
	if err != nil {
		t.Fatalf("direct state update: %v", err)
	}

	// Attempt transition from QUEUED (stale) — should fail because DB state is now ASSIGNED.
	_, err = repo.UpdateState(ctx, wu.ID, WorkUnitStateQueued, WorkUnitStateAssigned)
	if err == nil {
		t.Fatal("expected conflict error for concurrent state change")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestWorkUnitUpdateStateRejectedToQueued(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wurequeue")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Walk to REJECTED: CREATED → QUEUED → ASSIGNED → RUNNING → COMPLETED → REJECTED.
	transitions := []struct{ from, to WorkUnitState }{
		{WorkUnitStateCreated, WorkUnitStateQueued},
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateRunning},
		{WorkUnitStateRunning, WorkUnitStateCompleted},
		{WorkUnitStateCompleted, WorkUnitStateRejected},
	}
	for _, tr := range transitions {
		var err error
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// REJECTED → QUEUED should increment reassignment_count, set HIGH, clear fields.
	updated, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateRejected, WorkUnitStateQueued)
	if err != nil {
		t.Fatalf("UpdateState REJECTED → QUEUED: %v", err)
	}
	if updated.State != WorkUnitStateQueued {
		t.Errorf("State = %s, want QUEUED", updated.State)
	}
	if updated.ReassignmentCount != 1 {
		t.Errorf("ReassignmentCount = %d, want 1", updated.ReassignmentCount)
	}
	if updated.Priority != WorkUnitPriorityHigh {
		t.Errorf("Priority = %s, want HIGH", updated.Priority)
	}
	if updated.AssignedVolunteerID != nil {
		t.Error("AssignedVolunteerID should be nil after requeue")
	}
}

// TestWorkUnitUpdateStateExpiredRequeueUncappedThenFailed: requeue is UNCAPPED
// (property 6) — UpdateState EXPIRED → QUEUED no longer caps on max_reassignments, so
// a unit at/over its old cap still requeues. A unit is parked FAILED only via an
// explicit EXPIRED → FAILED transition (or the dead-letter ceiling), not by a
// per-reassignment cap.
func TestWorkUnitUpdateStateExpiredRequeueUncappedThenFailed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wufailed")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.MaxReassignments = 1 // old cap — now ignored for requeue capping.
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Walk to EXPIRED: CREATED → QUEUED → ASSIGNED → EXPIRED.
	transitions := []struct{ from, to WorkUnitState }{
		{WorkUnitStateCreated, WorkUnitStateQueued},
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateExpired},
	}
	for _, tr := range transitions {
		var err error
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// Requeue once (count goes to 1, which equals the old max_reassignments).
	wu, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateExpired, WorkUnitStateQueued)
	if err != nil {
		t.Fatalf("first requeue: %v", err)
	}
	if wu.ReassignmentCount != 1 {
		t.Fatalf("ReassignmentCount = %d, want 1", wu.ReassignmentCount)
	}

	// Walk to EXPIRED again.
	transitions2 := []struct{ from, to WorkUnitState }{
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateExpired},
	}
	for _, tr := range transitions2 {
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// Requeue AGAIN — now SUCCEEDS even though count(1) == old max(1): uncapped.
	wu, err = repo.UpdateState(ctx, wu.ID, WorkUnitStateExpired, WorkUnitStateQueued)
	if err != nil {
		t.Fatalf("second requeue should succeed (requeue is uncapped): %v", err)
	}
	if wu.State != WorkUnitStateQueued {
		t.Errorf("State = %s, want QUEUED", wu.State)
	}
	if wu.ReassignmentCount != 2 {
		t.Errorf("ReassignmentCount = %d, want 2 (bumped past old cap)", wu.ReassignmentCount)
	}

	// Walk to EXPIRED and park it FAILED via the explicit transition.
	for _, tr := range transitions2 {
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}
	updated, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateExpired, WorkUnitStateFailed)
	if err != nil {
		t.Fatalf("UpdateState EXPIRED → FAILED: %v", err)
	}
	if updated.State != WorkUnitStateFailed {
		t.Errorf("State = %s, want FAILED", updated.State)
	}
	if !updated.FlaggedForReview {
		t.Error("FlaggedForReview should be true")
	}
}

func TestWorkUnitBulkCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wubulk")
	leafID := createTestLeaf(t, pool, &userID)
	batchRepo := NewPgxBatchRepository(pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	batch := newTestBatch(leafID, 1, 1100)
	if err := batchRepo.Create(ctx, batch); err != nil {
		t.Fatalf("Create batch: %v", err)
	}

	// Create 1100 work units.
	wus := make([]*WorkUnit, 1100)
	for i := range wus {
		wus[i] = newTestWorkUnit(leafID, &batch.ID)
		wus[i].Parameters = json.RawMessage(fmt.Sprintf(`{"index": %d}`, i))
	}

	err := repo.BulkCreate(ctx, wus)
	if err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}

	// Verify all were persisted.
	var count int
	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE batch_id = $1", batch.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1100 {
		t.Errorf("expected 1100 work units, got %d", count)
	}
}

func TestWorkUnitBulkCreateEmpty(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	err := repo.BulkCreate(ctx, nil)
	if err != nil {
		t.Fatalf("BulkCreate with nil slice: %v", err)
	}

	err = repo.BulkCreate(ctx, []*WorkUnit{})
	if err != nil {
		t.Fatalf("BulkCreate with empty slice: %v", err)
	}
}

// --- Batch Repository Tests ---

func TestBatchCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchcreator1")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	b := newTestBatch(leafID, 1, 100)
	err := repo.Create(ctx, b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(b.ID) {
		t.Error("ID should be set after Create")
	}
	if b.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if b.SequenceNumber != 1 {
		t.Errorf("SequenceNumber = %d, want 1", b.SequenceNumber)
	}
	if b.TotalWorkUnits != 100 {
		t.Errorf("TotalWorkUnits = %d, want 100", b.TotalWorkUnits)
	}
	if b.CompletedWorkUnits != 0 {
		t.Errorf("CompletedWorkUnits = %d, want 0", b.CompletedWorkUnits)
	}
}

func TestBatchCreateDuplicateSequence(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchdup")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	b1 := newTestBatch(leafID, 1, 50)
	if err := repo.Create(ctx, b1); err != nil {
		t.Fatalf("Create b1: %v", err)
	}

	b2 := newTestBatch(leafID, 1, 50) // same sequence
	err := repo.Create(ctx, b2)
	if err == nil {
		t.Fatal("expected conflict for duplicate sequence number")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestBatchGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchget")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	b := newTestBatch(leafID, 1, 100)
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != b.ID {
		t.Errorf("ID = %v, want %v", got.ID, b.ID)
	}
	if got.TotalWorkUnits != 100 {
		t.Errorf("TotalWorkUnits = %d, want 100", got.TotalWorkUnits)
	}
}

func TestBatchGetByIDNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxBatchRepository(pool)
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

func TestBatchListByLeaf(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchlist")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	// Create 3 batches.
	for i := 1; i <= 3; i++ {
		b := newTestBatch(leafID, i, 10*i)
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("Create batch %d: %v", i, err)
		}
	}

	batches, pagination, err := repo.ListByLeaf(ctx, leafID, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByLeaf page 1: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("page 1: got %d batches, want 2", len(batches))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	// Verify ordering by sequence_number.
	if batches[0].SequenceNumber >= batches[1].SequenceNumber {
		t.Errorf("batches not ordered by sequence_number: %d >= %d",
			batches[0].SequenceNumber, batches[1].SequenceNumber)
	}

	// Page 2.
	batches2, pagination2, err := repo.ListByLeaf(ctx, leafID,
		types.PaginationRequest{PageSize: 2, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("ListByLeaf page 2: %v", err)
	}
	if len(batches2) != 1 {
		t.Fatalf("page 2: got %d batches, want 1", len(batches2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}

func TestBatchIncrementCompleted(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchincr")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	b := newTestBatch(leafID, 1, 10)
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Increment 3 times.
	for i := 0; i < 3; i++ {
		if err := repo.IncrementCompleted(ctx, b.ID); err != nil {
			t.Fatalf("IncrementCompleted %d: %v", i, err)
		}
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.CompletedWorkUnits != 3 {
		t.Errorf("CompletedWorkUnits = %d, want 3", got.CompletedWorkUnits)
	}
}

func TestBatchIncrementCompletedNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	err := repo.IncrementCompleted(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent batch")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestBatchIncrementCompletedConcurrent(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batchconc")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxBatchRepository(pool)
	ctx := context.Background()

	b := newTestBatch(leafID, 1, 100)
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Concurrently increment 50 times.
	var wg sync.WaitGroup
	errs := make([]error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = repo.IncrementCompleted(ctx, b.ID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("IncrementCompleted goroutine %d: %v", i, err)
		}
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.CompletedWorkUnits != 50 {
		t.Errorf("CompletedWorkUnits = %d, want 50", got.CompletedWorkUnits)
	}
}

// --- Coverage gap tests (added by /test-coverage) ---

func TestWorkUnitListFilterByAssignedVolunteerID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wulistvol")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu1 := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu1); err != nil {
		t.Fatalf("Create wu1: %v", err)
	}
	wu2 := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu2); err != nil {
		t.Fatalf("Create wu2: %v", err)
	}

	// Create a volunteer for the FK reference.
	volID := types.NewID()
	pubKey := []byte(fmt.Sprintf("test-vol-pubkey-%s!!!!!!!", volID.String()[:14]))
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, display_name)
		VALUES ($1, $2, $3)`, volID, pubKey, "Test Volunteer")
	if err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// Assign wu1 to the volunteer via direct SQL.
	_, err = pool.Exec(ctx,
		"UPDATE work_units SET assigned_volunteer_id = $1, state = 'ASSIGNED' WHERE id = $2",
		volID, wu1.ID)
	if err != nil {
		t.Fatalf("assign volunteer: %v", err)
	}

	wus, _, err := repo.List(ctx, WorkUnitListFilters{
		LeafID:           &leafID,
		AssignedVolunteerID: &volID,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(wus) != 1 {
		t.Fatalf("expected 1 assigned work unit, got %d", len(wus))
	}
	if wus[0].ID != wu1.ID {
		t.Errorf("expected work unit %v, got %v", wu1.ID, wus[0].ID)
	}
}

func TestWorkUnitListInvalidCursor(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	_, _, err := repo.List(ctx, WorkUnitListFilters{}, types.PaginationRequest{Cursor: "not-valid-base64!!"})
	if err == nil {
		t.Fatal("expected error for invalid cursor")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 400 {
		t.Errorf("HTTPStatus = %d, want 400", apiErr.HTTPStatus)
	}
}

func TestWorkUnitUpdateStateNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	_, err := repo.UpdateState(ctx, types.NewID(), WorkUnitStateCreated, WorkUnitStateQueued)
	if err == nil {
		t.Fatal("expected error for non-existent work unit")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestWorkUnitUpdateStateRunningToExpired(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wurunexp")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Walk to RUNNING.
	transitions := []struct{ from, to WorkUnitState }{
		{WorkUnitStateCreated, WorkUnitStateQueued},
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateRunning},
	}
	for _, tr := range transitions {
		var err error
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// RUNNING → EXPIRED.
	updated, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateRunning, WorkUnitStateExpired)
	if err != nil {
		t.Fatalf("UpdateState RUNNING → EXPIRED: %v", err)
	}
	if updated.State != WorkUnitStateExpired {
		t.Errorf("State = %s, want EXPIRED", updated.State)
	}
}

func TestWorkUnitUpdateStateRejectedToFailed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wurejfail")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.MaxReassignments = 1
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Walk to REJECTED.
	transitions := []struct{ from, to WorkUnitState }{
		{WorkUnitStateCreated, WorkUnitStateQueued},
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateRunning},
		{WorkUnitStateRunning, WorkUnitStateCompleted},
		{WorkUnitStateCompleted, WorkUnitStateRejected},
	}
	for _, tr := range transitions {
		var err error
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// Requeue once (count goes to 1 = max).
	wu, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateRejected, WorkUnitStateQueued)
	if err != nil {
		t.Fatalf("first requeue: %v", err)
	}

	// Walk to REJECTED again.
	transitions2 := []struct{ from, to WorkUnitState }{
		{WorkUnitStateQueued, WorkUnitStateAssigned},
		{WorkUnitStateAssigned, WorkUnitStateRunning},
		{WorkUnitStateRunning, WorkUnitStateCompleted},
		{WorkUnitStateCompleted, WorkUnitStateRejected},
	}
	for _, tr := range transitions2 {
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("UpdateState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	// REJECTED → FAILED (max reassignments exceeded).
	updated, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateRejected, WorkUnitStateFailed)
	if err != nil {
		t.Fatalf("UpdateState REJECTED → FAILED: %v", err)
	}
	if updated.State != WorkUnitStateFailed {
		t.Errorf("State = %s, want FAILED", updated.State)
	}
	if !updated.FlaggedForReview {
		t.Error("FlaggedForReview should be true")
	}
}

// --- Assignment Flow Test Helpers ---

// createActiveTestLeaf creates a project with state=ACTIVE and the given resource requirements.
func createActiveTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID, resReqs, execConfig, valConfig string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "active-leaf-" + uuid.New().String()[:8]
	if resReqs == "" {
		resReqs = `{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_gpu_vram_mb":0}`
	}
	if execConfig == "" {
		execConfig = `{"runtime":"NATIVE","gpu_required":false}`
	}
	if valConfig == "" {
		valConfig = `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			$6::jsonb, $7::jsonb,
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.0}',
			$8::jsonb,
			false, 'PUBLIC', $5
		)`,
		id, "Active Leaf "+slug, slug, "An active leaf for assignment tests",
		creatorID, execConfig, valConfig, resReqs,
	)
	if err != nil {
		t.Fatalf("failed to create active test leaf: %v", err)
	}
	return id
}

func createTestVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	// Generate a unique 32-byte public key.
	id1 := uuid.New()
	id2 := uuid.New()
	pubKey := make([]byte, 32)
	copy(pubKey, id1[:])
	copy(pubKey[16:], id2[:])
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at
		) VALUES (
			$1, $2, $3, $4, 'ALWAYS', true, $5
		)`,
		id, pubKey,
		`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`,
		[]string{"NATIVE", "CONTAINER"},
		now,
	)
	if err != nil {
		t.Fatalf("failed to create test volunteer: %v", err)
	}
	return id
}

// --- FindNextAssignable & Assign Tests ---

func TestFindNextAssignable_BasicMatch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-basic")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create a QUEUED work unit.
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		HasGPU:            false,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found == nil {
		t.Fatal("expected a work unit, got nil")
	}
	if found.ID != wu.ID {
		t.Errorf("found.ID = %v, want %v", found.ID, wu.ID)
	}
}

// TestFindNextAssignable_HomogeneousRedundancy verifies the HR filter + first-writer-wins
// pin at the DB layer: EnsureWorkUnitHRClass is idempotent, a unit pinned to one class is
// excluded from a different-class requester, and returned to a same-class requester.
func TestFindNextAssignable_HomogeneousRedundancy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-hr")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const intel = "GenuineIntel/linux/amd64"
	const amd = "AuthenticAMD/linux/amd64"

	got, err := repo.EnsureWorkUnitHRClass(ctx, wu.ID, intel)
	if err != nil {
		t.Fatalf("EnsureWorkUnitHRClass: %v", err)
	}
	if got != intel {
		t.Fatalf("first pin = %q, want %q", got, intel)
	}
	if got, _ := repo.EnsureWorkUnitHRClass(ctx, wu.ID, amd); got != intel {
		t.Fatalf("second pin = %q, want first-writer-wins %q", got, intel)
	}

	base := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	diff := base
	diff.HRClass = amd
	if found, err := repo.FindNextAssignable(ctx, diff); err != nil {
		t.Fatalf("FindNextAssignable(diff): %v", err)
	} else if found != nil {
		t.Fatalf("different-class requester should get nil, got %v", found.ID)
	}

	same := base
	same.HRClass = intel
	if found, err := repo.FindNextAssignable(ctx, same); err != nil {
		t.Fatalf("FindNextAssignable(same): %v", err)
	} else if found == nil || found.ID != wu.ID {
		t.Fatalf("same-class requester should get the unit, got %v", found)
	}
}

func TestFindNextAssignable_NoWorkReturnsNil(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil, got %v", found.ID)
	}
}

func TestFindNextAssignable_PriorityOrdering(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-priority")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create NORMAL priority work unit first.
	wuNormal := newTestWorkUnit(leafID, nil)
	wuNormal.State = WorkUnitStateQueued
	wuNormal.Priority = WorkUnitPriorityNormal
	if err := repo.Create(ctx, wuNormal); err != nil {
		t.Fatalf("Create normal: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// Create HIGH priority work unit second.
	wuHigh := newTestWorkUnit(leafID, nil)
	wuHigh.State = WorkUnitStateQueued
	wuHigh.Priority = WorkUnitPriorityHigh
	if err := repo.Create(ctx, wuHigh); err != nil {
		t.Fatalf("Create high: %v", err)
	}

	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found == nil {
		t.Fatal("expected a work unit")
	}
	// HIGH priority should be returned first despite being created second.
	if found.ID != wuHigh.ID {
		t.Errorf("expected HIGH priority work unit %v, got %v", wuHigh.ID, found.ID)
	}
}

func TestFindNextAssignable_RedundancyLimit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-redundancy")
	// redundancy_factor = 2.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "",
		`{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	vol3 := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	opts := func(volID types.ID) AssignmentOptions {
		return AssignmentOptions{
			VolunteerID:       volID,
			MaxCPUCores:       4,
			MaxMemoryMB:       16384,
			MaxDiskMB:         10240,
			AvailableRuntimes: []string{"NATIVE"},
		}
	}

	// First assignment — should work (0 active < 2).
	found1, err := repo.FindNextAssignable(ctx, opts(vol1))
	if err != nil {
		t.Fatalf("Find 1: %v", err)
	}
	if found1 == nil {
		t.Fatal("expected work unit for first assignment")
	}

	// Record assignment in history.
	now := time.Now().UTC()
	_, err = pool.Exec(ctx,
		"INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, $3)",
		wu.ID, vol1, now)
	if err != nil {
		t.Fatalf("insert history 1: %v", err)
	}

	// Second assignment — should work (1 active < 2).
	found2, err := repo.FindNextAssignable(ctx, opts(vol2))
	if err != nil {
		t.Fatalf("Find 2: %v", err)
	}
	if found2 == nil {
		t.Fatal("expected work unit for second assignment")
	}

	_, err = pool.Exec(ctx,
		"INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, $3)",
		wu.ID, vol2, now)
	if err != nil {
		t.Fatalf("insert history 2: %v", err)
	}

	// Third assignment — should return nil (2 active = 2, not < 2).
	found3, err := repo.FindNextAssignable(ctx, opts(vol3))
	if err != nil {
		t.Fatalf("Find 3: %v", err)
	}
	if found3 != nil {
		t.Errorf("expected nil for third assignment (redundancy exceeded), got %v", found3.ID)
	}
}

func TestFindNextAssignable_CapabilityMismatch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-cap")
	// Project requires 8 CPU cores and 16GB memory.
	leafID := createActiveTestLeaf(t, pool, &userID,
		`{"min_cpu_cores":8,"min_memory_mb":16384,"min_disk_mb":1024,"gpu_required":false}`, "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Volunteer only has 2 CPU cores — doesn't meet leaf requirement of 8.
	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       2,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil for capability mismatch, got %v", found.ID)
	}
}

func TestFindNextAssignable_RuntimeMismatch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-runtime")
	// Project requires "CONTAINER" runtime.
	leafID := createActiveTestLeaf(t, pool, &userID, "",
		`{"runtime":"CONTAINER","gpu_required":false}`, "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Volunteer only supports "NATIVE" — no match.
	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil for runtime mismatch, got %v", found.ID)
	}
}

func TestFindNextAssignable_InlineDataDelivered(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-inline")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create a work unit with inline input_data (parameter sweep).
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	wu.InputData = json.RawMessage(`{"param_a": 0.5, "param_b": 100}`)
	wu.Parameters = json.RawMessage(`{"seed": 42}`)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found == nil {
		t.Fatal("expected work unit")
	}
	if found.InputData == nil {
		t.Fatal("expected inline input_data to be returned")
	}
	if string(found.InputData) != `{"param_a": 0.5, "param_b": 100}` {
		t.Errorf("InputData = %s, want matching inline data", string(found.InputData))
	}
	if string(found.Parameters) != `{"seed": 42}` {
		t.Errorf("Parameters = %s, want matching parameters", string(found.Parameters))
	}
}

// TestAssign_RunStartsReservedCopy: Assign run-starts a volunteer's RESERVED copy
// (sets started_at), keeping the WORK UNIT QUEUED so its other redundancy copies keep
// dispatching in parallel. The denormalized work_units.assigned_* pointer is updated
// best-effort for observability.
func TestAssign_RunStartsReservedCopy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-transition")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Buffer a copy first; Assign run-starts it.
	if _, err := repo.ReserveCopy(ctx, wu.ID, volunteerID, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy: %v", err)
	}

	assigned, err := repo.Assign(ctx, wu.ID, volunteerID)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if assigned.State != WorkUnitStateQueued {
		t.Errorf("State = %s, want QUEUED (unit stays QUEUED through run-start)", assigned.State)
	}
	if assigned.AssignedVolunteerID == nil || *assigned.AssignedVolunteerID != volunteerID {
		t.Errorf("AssignedVolunteerID = %v, want %v", assigned.AssignedVolunteerID, volunteerID)
	}
	if assigned.AssignedAt == nil {
		t.Error("AssignedAt should be set")
	}
	if assigned.LastHeartbeatAt == nil {
		t.Error("LastHeartbeatAt should be set")
	}

	// The volunteer's copy is now RUNNING (started_at set).
	var startedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT started_at FROM work_unit_assignment_history
		 WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`, wu.ID, volunteerID).Scan(&startedAt); err != nil {
		t.Fatalf("read copy started_at: %v", err)
	}
	if startedAt == nil {
		t.Error("copy started_at should be set at run-start")
	}
}

// TestAssign_NoReservedCopy_Fails: Assign fails with Conflict when the volunteer holds
// no live (un-started) copy to run-start.
func TestAssign_NoReservedCopy_Fails(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-notqueued")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// QUEUED unit but the volunteer never reserved a copy.
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := repo.Assign(ctx, wu.ID, volunteerID)
	if err == nil {
		t.Fatal("expected error when the volunteer holds no reserved copy")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestFindNextAssignable_SkipsDraftProjects(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-draft")
	// Create a DRAFT leaf (not ACTIVE) using the original helper.
	leafID := createTestLeaf(t, pool, &userID)
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil for DRAFT leaf, got %v", found.ID)
	}
}

func TestFindNextAssignable_ProjectIDFilter(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-filter")
	leaf1 := createActiveTestLeaf(t, pool, &userID, "", "", "")
	leaf2 := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu1 := newTestWorkUnit(leaf1, nil)
	wu1.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu1); err != nil {
		t.Fatalf("Create wu1: %v", err)
	}

	wu2 := newTestWorkUnit(leaf2, nil)
	wu2.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu2); err != nil {
		t.Fatalf("Create wu2: %v", err)
	}

	// Only request work from leaf 2.
	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		LeafIDs:        []types.ID{leaf2},
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found == nil {
		t.Fatal("expected a work unit from leaf 2")
	}
	if found.LeafID != leaf2 {
		t.Errorf("expected leaf %v, got %v", leaf2, found.LeafID)
	}
}

func TestFindNextAssignable_BlockedProjectFilter(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "assign-blocked")
	leaf1 := createActiveTestLeaf(t, pool, &userID, "", "", "")
	leaf2 := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu1 := newTestWorkUnit(leaf1, nil)
	wu1.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu1); err != nil {
		t.Fatalf("Create wu1: %v", err)
	}

	wu2 := newTestWorkUnit(leaf2, nil)
	wu2.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu2); err != nil {
		t.Fatalf("Create wu2: %v", err)
	}

	// Block leaf 1.
	opts := AssignmentOptions{
		VolunteerID:       volunteerID,
		BlockedLeafIDs: []types.ID{leaf1},
		MaxCPUCores:       4,
		MaxMemoryMB:       16384,
		MaxDiskMB:         10240,
		AvailableRuntimes: []string{"NATIVE"},
	}

	found, err := repo.FindNextAssignable(ctx, opts)
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if found == nil {
		t.Fatal("expected a work unit from leaf 2")
	}
	if found.LeafID != leaf2 {
		t.Errorf("expected project %v (not blocked), got %v", leaf2, found.LeafID)
	}
}

func TestWorkUnitUpdateStateConcurrentRace(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "wurace")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Transition to QUEUED.
	if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
		t.Fatalf("to QUEUED: %v", err)
	}

	// Two goroutines both try QUEUED → ASSIGNED simultaneously.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, results[idx] = repo.UpdateState(ctx, wu.ID, WorkUnitStateQueued, WorkUnitStateAssigned)
		}(i)
	}
	wg.Wait()

	successCount := 0
	conflictCount := 0
	for _, err := range results {
		if err == nil {
			successCount++
		} else {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 409 {
				conflictCount++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 success, got %d", successCount)
	}
	if conflictCount != 1 {
		t.Errorf("expected exactly 1 conflict, got %d", conflictCount)
	}
}

// --- Reassign Tests (S23) ---

// walkToState transitions a work unit through states until it reaches the target state.
func walkToState(t *testing.T, repo *PgxWorkUnitRepository, wu *WorkUnit, target WorkUnitState) *WorkUnit {
	t.Helper()
	path := map[WorkUnitState][]struct{ from, to WorkUnitState }{
		WorkUnitStateExpired: {
			{WorkUnitStateCreated, WorkUnitStateQueued},
			{WorkUnitStateQueued, WorkUnitStateAssigned},
			{WorkUnitStateAssigned, WorkUnitStateExpired},
		},
		WorkUnitStateRejected: {
			{WorkUnitStateCreated, WorkUnitStateQueued},
			{WorkUnitStateQueued, WorkUnitStateAssigned},
			{WorkUnitStateAssigned, WorkUnitStateCompleted},
			{WorkUnitStateCompleted, WorkUnitStateRejected},
		},
	}

	transitions, ok := path[target]
	if !ok {
		t.Fatalf("walkToState: no path to %s", target)
	}

	ctx := context.Background()
	var err error
	for _, tr := range transitions {
		if wu.State != tr.from {
			continue
		}
		wu, err = repo.UpdateState(ctx, wu.ID, tr.from, tr.to)
		if err != nil {
			t.Fatalf("walkToState %s → %s: %v", tr.from, tr.to, err)
		}
	}

	if wu.State != target {
		t.Fatalf("walkToState: ended in %s, want %s", wu.State, target)
	}
	return wu
}

func TestReassign_ExpiredToQueued(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reassign-exp-q")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.MaxReassignments = 3
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	wu = walkToState(t, repo, wu, WorkUnitStateExpired)

	updated, requeued, err := repo.Reassign(ctx, wu.ID)
	if err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if !requeued {
		t.Error("expected requeued = true")
	}
	if updated.State != WorkUnitStateQueued {
		t.Errorf("State = %s, want QUEUED", updated.State)
	}
	if updated.ReassignmentCount != 1 {
		t.Errorf("ReassignmentCount = %d, want 1", updated.ReassignmentCount)
	}
	if updated.Priority != WorkUnitPriorityHigh {
		t.Errorf("Priority = %s, want HIGH", updated.Priority)
	}
	if updated.AssignedVolunteerID != nil {
		t.Error("AssignedVolunteerID should be nil")
	}
	if updated.AssignedAt != nil {
		t.Error("AssignedAt should be nil")
	}
	if updated.StartedAt != nil {
		t.Error("StartedAt should be nil")
	}
	if updated.CompletedAt != nil {
		t.Error("CompletedAt should be nil")
	}
	if updated.ValidatedAt != nil {
		t.Error("ValidatedAt should be nil")
	}
	if updated.LastHeartbeatAt != nil {
		t.Error("LastHeartbeatAt should be nil")
	}

	// Verify re-read from DB.
	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Errorf("DB State = %s, want QUEUED", got.State)
	}
}

func TestReassign_RejectedToQueued(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reassign-rej-q")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.MaxReassignments = 3
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	wu = walkToState(t, repo, wu, WorkUnitStateRejected)

	updated, requeued, err := repo.Reassign(ctx, wu.ID)
	if err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if !requeued {
		t.Error("expected requeued = true")
	}
	if updated.State != WorkUnitStateQueued {
		t.Errorf("State = %s, want QUEUED", updated.State)
	}
	if updated.ReassignmentCount != 1 {
		t.Errorf("ReassignmentCount = %d, want 1", updated.ReassignmentCount)
	}
	if updated.Priority != WorkUnitPriorityHigh {
		t.Errorf("Priority = %s, want HIGH", updated.Priority)
	}
}

// insertClosedCopy writes a CLOSED copy (outcome set) for a unit/volunteer — a
// dispatch attempt that timed out/abandoned, counting toward the dead-letter total
// but not toward the live-copy count.
func insertClosedCopy(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, outcome string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at, outcome, outcome_at)
		VALUES ($1, $2, NOW(), $3::assignment_outcome, NOW())`, wuID, volID, outcome); err != nil {
		t.Fatalf("insert closed copy: %v", err)
	}
}

// TestDeadLetterIfExhausted replaces the retired per-reassignment cap (property 6):
// requeue is uncapped, and the ONLY terminal stop is the dead-letter ceiling. A
// QUEUED unit with NO live copy, redundancy still unmet, and total copies ever
// created >= its ceiling (max_total_copies) is parked FAILED + flagged. Units that
// still have a live copy, or are under the ceiling, are NOT dead-lettered.
func TestDeadLetterIfExhausted(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "deadletter")
	leafID := createTestLeaf(t, pool, &userID) // redundancy_factor 2
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)

	mkQueued := func(maxTotal int) *WorkUnit {
		wu := newTestWorkUnit(leafID, nil)
		wu.MaxTotalCopies = maxTotal
		if err := repo.Create(ctx, wu); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
			t.Fatalf("UpdateState CREATED→QUEUED: %v", err)
		}
		return wu
	}

	// Exhausted: ceiling 2, two CLOSED copies (total 2 >= 2), no live copy, redundancy
	// unmet (0 PENDING results < 2) → dead-lettered.
	exhausted := mkQueued(2)
	insertClosedCopy(t, pool, exhausted.ID, volA, "EXPIRED")
	insertClosedCopy(t, pool, exhausted.ID, volB, "ABANDONED")

	failed, err := repo.DeadLetterIfExhausted(ctx, exhausted.ID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted(exhausted): %v", err)
	}
	if !failed {
		t.Fatal("expected exhausted unit to be dead-lettered")
	}
	got, err := repo.GetByID(ctx, exhausted.ID)
	if err != nil {
		t.Fatalf("GetByID(exhausted): %v", err)
	}
	if got.State != WorkUnitStateFailed {
		t.Errorf("State = %s, want FAILED", got.State)
	}
	if !got.FlaggedForReview {
		t.Error("dead-lettered unit should be flagged for review")
	}

	// Under ceiling: only 1 total copy (< 2) → NOT dead-lettered.
	underCeiling := mkQueued(2)
	insertClosedCopy(t, pool, underCeiling.ID, volA, "EXPIRED")
	if failed, err := repo.DeadLetterIfExhausted(ctx, underCeiling.ID); err != nil {
		t.Fatalf("DeadLetterIfExhausted(underCeiling): %v", err)
	} else if failed {
		t.Error("a unit under its copy ceiling must not be dead-lettered")
	}

	// Live copy present: total >= ceiling but a copy is still live → NOT dead-lettered.
	hasLive := mkQueued(2)
	insertClosedCopy(t, pool, hasLive.ID, volA, "EXPIRED")
	if _, err := repo.ReserveCopy(ctx, hasLive.ID, volB, nil, time.Now().UTC().Add(time.Hour), 3600); err != nil {
		t.Fatalf("ReserveCopy(hasLive): %v", err)
	}
	if failed, err := repo.DeadLetterIfExhausted(ctx, hasLive.ID); err != nil {
		t.Fatalf("DeadLetterIfExhausted(hasLive): %v", err)
	} else if failed {
		t.Error("a unit with a live copy must not be dead-lettered")
	}
}

func TestReassign_InvalidState(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reassign-inv")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Transition to QUEUED — not a valid state for Reassign.
	wu, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued)
	if err != nil {
		t.Fatalf("UpdateState: %v", err)
	}

	_, _, err = repo.Reassign(ctx, wu.ID)
	if err == nil {
		t.Fatal("expected error for non-EXPIRED/REJECTED state")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestReassign_FieldsClearedOnRequeue(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reassign-clear")
	leafID := createTestLeaf(t, pool, &userID)
	volunteerID := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	wu.MaxReassignments = 3
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a dispatched-then-EXPIRED unit with the denormalized assignment
	// pointer populated (in the per-copy model run-start keeps the unit QUEUED, so
	// set the EXPIRED state + denormalized fields directly to reach the requeue case).
	if _, err := pool.Exec(ctx, `
		UPDATE work_units SET state = 'EXPIRED',
			assigned_volunteer_id = $2, assigned_at = NOW(),
			started_at = NOW(), last_heartbeat_at = NOW()
		WHERE id = $1`, wu.ID, volunteerID); err != nil {
		t.Fatalf("seed EXPIRED unit with assignment fields: %v", err)
	}

	// Precondition: denormalized assignment fields are populated.
	wu, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if wu.AssignedVolunteerID == nil {
		t.Fatal("precondition: AssignedVolunteerID should be set")
	}
	if wu.AssignedAt == nil {
		t.Fatal("precondition: AssignedAt should be set")
	}

	// Reassign.
	updated, requeued, err := repo.Reassign(ctx, wu.ID)
	if err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if !requeued {
		t.Fatal("expected requeued = true")
	}

	// Verify all assignment fields cleared.
	if updated.AssignedVolunteerID != nil {
		t.Errorf("AssignedVolunteerID should be nil, got %v", updated.AssignedVolunteerID)
	}
	if updated.AssignedAt != nil {
		t.Errorf("AssignedAt should be nil, got %v", updated.AssignedAt)
	}
	if updated.StartedAt != nil {
		t.Errorf("StartedAt should be nil, got %v", updated.StartedAt)
	}
	if updated.CompletedAt != nil {
		t.Errorf("CompletedAt should be nil, got %v", updated.CompletedAt)
	}
	if updated.ValidatedAt != nil {
		t.Errorf("ValidatedAt should be nil, got %v", updated.ValidatedAt)
	}
	if updated.LastHeartbeatAt != nil {
		t.Errorf("LastHeartbeatAt should be nil, got %v", updated.LastHeartbeatAt)
	}
	if updated.Priority != WorkUnitPriorityHigh {
		t.Errorf("Priority = %s, want HIGH", updated.Priority)
	}
}

func TestFindStuckSpotCheckUnits_QueuedTimeout(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "spotcheckexp")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Create 3 work units and transition all to QUEUED.
	// WU1: spot-check, created >1h ago — should be found
	// WU2: spot-check, created recently — should NOT be found
	// WU3: not spot-check, created >1h ago — should NOT be found

	wu1 := newTestWorkUnit(leafID, nil)
	wu2 := newTestWorkUnit(leafID, nil)
	wu3 := newTestWorkUnit(leafID, nil)
	for _, wu := range []*WorkUnit{wu1, wu2, wu3} {
		if err := repo.Create(ctx, wu); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
			t.Fatalf("UpdateState CREATED→QUEUED: %v", err)
		}
	}

	// Mark WU1 and WU2 as spot-check (WU3 stays normal).
	if err := repo.MarkSpotCheck(ctx, wu1.ID); err != nil {
		t.Fatalf("MarkSpotCheck wu1: %v", err)
	}
	if err := repo.MarkSpotCheck(ctx, wu2.ID); err != nil {
		t.Fatalf("MarkSpotCheck wu2: %v", err)
	}

	// Backdate WU1 and WU3 to >1h ago.
	_, err := pool.Exec(ctx,
		"UPDATE work_units SET created_at = NOW() - INTERVAL '2 hours' WHERE id = ANY($1)",
		[]types.ID{wu1.ID, wu3.ID})
	if err != nil {
		t.Fatalf("backdating created_at: %v", err)
	}

	// FindStuckSpotCheckUnits should return only WU1 (QUEUED spot-check unit that
	// has sat over an hour without a second corroborator).
	stuck, err := repo.FindStuckSpotCheckUnits(ctx, 100)
	if err != nil {
		t.Fatalf("FindStuckSpotCheckUnits: %v", err)
	}

	if len(stuck) != 1 {
		t.Fatalf("expected 1 stuck spot-check work unit, got %d", len(stuck))
	}
	if stuck[0].ID != wu1.ID {
		t.Errorf("expected stuck WU to be wu1 (%v), got %v", wu1.ID, stuck[0].ID)
	}
	if !stuck[0].SpotCheck {
		t.Error("stuck WU should have SpotCheck=true")
	}
	if stuck[0].State != WorkUnitStateQueued {
		t.Errorf("stuck WU state = %s, want QUEUED", stuck[0].State)
	}
}

func TestClearSpotCheck(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "clearspot")
	leafID := createTestLeaf(t, pool, &userID)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if err := repo.MarkSpotCheck(ctx, wu.ID); err != nil {
		t.Fatalf("MarkSpotCheck: %v", err)
	}

	// Verify it's marked.
	fetched, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !fetched.SpotCheck {
		t.Fatal("SpotCheck should be true after MarkSpotCheck")
	}

	// Clear it.
	if err := repo.ClearSpotCheck(ctx, wu.ID); err != nil {
		t.Fatalf("ClearSpotCheck: %v", err)
	}

	// Verify cleared.
	fetched, err = repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if fetched.SpotCheck {
		t.Error("SpotCheck should be false after ClearSpotCheck")
	}
}
