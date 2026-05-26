//go:build integration

package leaf

import (
	"context"
	"fmt"
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
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
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

func newTestLeaf(creatorID *types.ID) *Leaf {
	return &Leaf{
		Name:        "Test Leaf " + uuid.New().String()[:8],
		Description: "A test leaf for integration testing purposes",
		ResearchArea: []string{"physics", "ml-ai"},
		CreatorID:   creatorID,
		State:       StateDraft,
		TaskPattern: PatternParameterSweep,
		ExecutionConfig: ExecutionConfig{
			Runtime:       "NATIVE",
			GPUType:       "ANY",
			MaxMemoryMB:   4096,
			MaxDiskMB:     10240,
			MaxCPUSeconds: 86400,
		},
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		FaultToleranceConfig: FaultToleranceConfig{
			HeartbeatIntervalSeconds:  300,
			MissedHeartbeatsThreshold: 3,
			DeadlineMultiplier:        3.0,
			MaxReassignments:          3,
		},
		DataConfig: DataConfig{
			TransferStrategy:   "INLINE",
			AggregationFormat:  "JSON",
			MaxInputSizeBytes:  1048576,
			MaxOutputSizeBytes: 104857600,
		},
		CreditConfig: CreditConfig{
			CreditPerValidatedWorkUnit: 1.0,
		},
		ResourceRequirements: ResourceRequirements{
			MinCPUCores: 1,
			MinDiskMB:   1024,
		},
		IsOngoing:  false,
		Visibility: VisibilityPublic,
	}
}

func TestCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator1")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	err := repo.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify DB-generated fields.
	if types.IsNilID(p.ID) {
		t.Error("ID should be set after Create")
	}
	if p.Slug == "" {
		t.Error("Slug should be generated")
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if p.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}
	if p.State != StateDraft {
		t.Errorf("State = %q, want %q", p.State, StateDraft)
	}
}

func TestCreateSlugCollision(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator2")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create two leafs with the same name.
	p1 := newTestLeaf(&userID)
	p1.Name = "Duplicate Name Project"
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}

	p2 := newTestLeaf(&userID)
	p2.Name = "Duplicate Name Project"
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	if p1.Slug == p2.Slug {
		t.Errorf("slugs should differ: p1=%q, p2=%q", p1.Slug, p2.Slug)
	}
	if p2.Slug != p1.Slug+"-2" {
		t.Errorf("p2.Slug = %q, want %q", p2.Slug, p1.Slug+"-2")
	}
}

func TestGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator3")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID = %v, want %v", got.ID, p.ID)
	}
	if got.Name != p.Name {
		t.Errorf("Name = %q, want %q", got.Name, p.Name)
	}
	if got.Slug != p.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, p.Slug)
	}
}

func TestGetByIDNotFound(t *testing.T) {
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

func TestGetBySlug(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator4")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetBySlug(ctx, p.Slug, &userID)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID = %v, want %v", got.ID, p.ID)
	}
}

func TestGetBySlugNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator5")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, err := repo.GetBySlug(ctx, "nonexistent-slug", &userID)
	if err == nil {
		t.Fatal("expected error for non-existent slug")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestListPagination(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator6")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 5 leafs.
	for i := 0; i < 5; i++ {
		p := newTestLeaf(&userID)
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		// Small sleep to ensure different created_at values.
		time.Sleep(10 * time.Millisecond)
	}

	// Page 1: 3 items.
	leafs, pagination, err := repo.List(ctx, LeafListFilters{
		Sort:  SortCreatedAt,
		Order: OrderDesc,
	}, types.PaginationRequest{PageSize: 3})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(leafs) != 3 {
		t.Fatalf("page 1: got %d leafs, want 3", len(leafs))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}
	if pagination.NextCursor == "" {
		t.Error("page 1: NextCursor should be set")
	}

	// Page 2: remaining 2 items.
	leafs2, pagination2, err := repo.List(ctx, LeafListFilters{
		Sort:  SortCreatedAt,
		Order: OrderDesc,
	}, types.PaginationRequest{PageSize: 3, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(leafs2) != 2 {
		t.Fatalf("page 2: got %d leafs, want 2", len(leafs2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}

	// Ensure no overlap between pages.
	seen := make(map[types.ID]bool)
	for _, p := range leafs {
		seen[p.ID] = true
	}
	for _, p := range leafs2 {
		if seen[p.ID] {
			t.Errorf("duplicate leaf %v across pages", p.ID)
		}
	}
}

func TestListFilterByState(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator7")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create a DRAFT leaf.
	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	state := StateDraft
	leafs, _, err := repo.List(ctx, LeafListFilters{
		State: &state,
		Sort:  SortCreatedAt,
		Order: OrderDesc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, proj := range leafs {
		if proj.State != StateDraft {
			t.Errorf("expected DRAFT, got %q", proj.State)
		}
	}
}

func TestListFilterByCreatorID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	user1 := createTestUser(t, pool, "creatora")
	user2 := createTestUser(t, pool, "creatorb")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p1 := newTestLeaf(&user1)
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	p2 := newTestLeaf(&user2)
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	leafs, _, err := repo.List(ctx, LeafListFilters{
		CreatorID: &user1,
		Sort:      SortCreatedAt,
		Order:     OrderDesc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, proj := range leafs {
		if proj.CreatorID == nil || *proj.CreatorID != user1 {
			t.Errorf("expected creator %v, got %v", user1, proj.CreatorID)
		}
	}
}

func TestListFilterByVisibility(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator8")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	pub := newTestLeaf(&userID)
	pub.Visibility = VisibilityPublic
	if err := repo.Create(ctx, pub); err != nil {
		t.Fatalf("Create public: %v", err)
	}

	priv := newTestLeaf(&userID)
	priv.Visibility = VisibilityPrivate
	priv.ResearchArea = nil // private doesn't require research_area
	if err := repo.Create(ctx, priv); err != nil {
		t.Fatalf("Create private: %v", err)
	}

	vis := VisibilityPublic
	leafs, _, err := repo.List(ctx, LeafListFilters{
		Visibility: &vis,
		Sort:       SortCreatedAt,
		Order:      OrderDesc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, proj := range leafs {
		if proj.Visibility != VisibilityPublic {
			t.Errorf("expected PUBLIC, got %q", proj.Visibility)
		}
	}
}

func TestListFilterBySearch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creator9")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	p.Name = "Quantum Monte Carlo Simulation"
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	search := "Quantum Monte"
	leafs, _, err := repo.List(ctx, LeafListFilters{
		Search: &search,
		Sort:   SortCreatedAt,
		Order:  OrderDesc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, proj := range leafs {
		if proj.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Error("search did not find the leaf")
	}
}

func TestListSortByName(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creatorsort")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	names := []string{"Zeta Leaf", "Alpha Leaf", "Mu Project"}
	for _, name := range names {
		p := newTestLeaf(&userID)
		p.Name = name
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	creatorFilter := userID
	leafs, _, err := repo.List(ctx, LeafListFilters{
		CreatorID: &creatorFilter,
		Sort:      SortName,
		Order:     OrderAsc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(leafs) < 3 {
		t.Fatalf("expected at least 3 leafs, got %d", len(leafs))
	}
	if leafs[0].Name > leafs[1].Name || leafs[1].Name > leafs[2].Name {
		t.Errorf("not sorted ascending by name: %q, %q, %q",
			leafs[0].Name, leafs[1].Name, leafs[2].Name)
	}
}

func TestUpdate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creatorupd")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	origSlug := p.Slug
	origUpdatedAt := p.UpdatedAt

	// Wait a bit so updated_at changes.
	time.Sleep(50 * time.Millisecond)

	// Update name and config.
	p.Name = "Updated Project Name"
	p.ExecutionConfig.MaxMemoryMB = 8192
	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Slug should NOT change.
	if p.Slug != origSlug {
		t.Errorf("Slug changed from %q to %q", origSlug, p.Slug)
	}

	// updated_at should change.
	if !p.UpdatedAt.After(origUpdatedAt) {
		t.Error("UpdatedAt should have changed")
	}

	// Verify from DB.
	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Name != "Updated Project Name" {
		t.Errorf("Name = %q, want %q", got.Name, "Updated Project Name")
	}
	if got.ExecutionConfig.MaxMemoryMB != 8192 {
		t.Errorf("MaxMemoryMB = %d, want 8192", got.ExecutionConfig.MaxMemoryMB)
	}
}

func TestUpdateNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(nil)
	p.ID = types.NewID()
	err := repo.Update(ctx, p)
	if err == nil {
		t.Fatal("expected error for non-existent leaf")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestDelete(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creatordel")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should be gone.
	_, err := repo.GetByID(ctx, p.ID)
	if err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestDeleteNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.Delete(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent leaf")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestCreateSelfHostedProject(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	pubKey := make([]byte, 32)
	for i := range pubKey {
		pubKey[i] = byte(i)
	}

	p := newTestLeaf(nil)
	p.CreatorPublicKey = pubKey
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(p.ID) {
		t.Error("ID should be set")
	}
	if p.CreatorID != nil {
		t.Errorf("CreatorID should be nil, got %v", p.CreatorID)
	}
	if len(p.CreatorPublicKey) != 32 {
		t.Errorf("CreatorPublicKey length = %d, want 32", len(p.CreatorPublicKey))
	}

	// Verify JSONB configs round-tripped through DB.
	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExecutionConfig.MaxMemoryMB != 4096 {
		t.Errorf("ExecutionConfig.MaxMemoryMB = %d, want 4096", got.ExecutionConfig.MaxMemoryMB)
	}
	if got.ValidationConfig.RedundancyFactor != 2 {
		t.Errorf("ValidationConfig.RedundancyFactor = %d, want 2", got.ValidationConfig.RedundancyFactor)
	}
	if got.CreditConfig.CreditPerValidatedWorkUnit != 1.0 {
		t.Errorf("CreditConfig.CreditPerValidatedWorkUnit = %f, want 1.0", got.CreditConfig.CreditPerValidatedWorkUnit)
	}
}

func TestGetBySlugNilCreatorID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p := newTestLeaf(nil)
	p.CreatorPublicKey = []byte("fake-ed25519-pubkey-32-bytes!!!!")
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetBySlug(ctx, p.Slug, nil)
	if err != nil {
		t.Fatalf("GetBySlug with nil creatorID: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID = %v, want %v", got.ID, p.ID)
	}
}

func TestCreateSlugCollisionNilCreatorID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p1 := newTestLeaf(nil)
	p1.Name = "Self Hosted Project"
	p1.CreatorPublicKey = []byte("fake-ed25519-pubkey-32-bytes!!!!")
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}

	p2 := newTestLeaf(nil)
	p2.Name = "Self Hosted Project"
	p2.CreatorPublicKey = []byte("another-ed25519-pubkey-32bytes!!")
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	if p1.Slug == p2.Slug {
		t.Errorf("slugs should differ: p1=%q, p2=%q", p1.Slug, p2.Slug)
	}
}

func TestListFilterByResearchArea(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "creatorra")
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	p1 := newTestLeaf(&userID)
	p1.ResearchArea = []string{"physics", "chemistry"}
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}

	p2 := newTestLeaf(&userID)
	p2.ResearchArea = []string{"biology"}
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	area := "physics"
	leafs, _, err := repo.List(ctx, LeafListFilters{
		ResearchArea: &area,
		CreatorID:    &userID,
		Sort:         SortCreatedAt,
		Order:        OrderDesc,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(leafs) != 1 {
		t.Fatalf("expected 1 leaf, got %d", len(leafs))
	}
	if leafs[0].ID != p1.ID {
		t.Errorf("expected leaf %v, got %v", p1.ID, leafs[0].ID)
	}
}

func TestListInvalidCursor(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, _, err := repo.List(ctx, LeafListFilters{
		Sort:  SortCreatedAt,
		Order: OrderDesc,
	}, types.PaginationRequest{Cursor: "garbage-cursor"})
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

func TestDeleteWithCreditHistory(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, pool, "creatorcredit")
	repo := NewPgxRepository(pool)

	p := newTestLeaf(&userID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create leaf: %v", err)
	}

	// Create a volunteer so we can create credit history.
	volID := types.NewID()
	pubKey := []byte(fmt.Sprintf("repo-test-pubkey-%s!!!!", volID.String()[:14]))
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, display_name)
		VALUES ($1, $2, $3)`,
		volID, pubKey, "Test Volunteer",
	)
	if err != nil {
		t.Fatalf("Create volunteer: %v", err)
	}

	// Create a work unit.
	wuID := types.NewID()
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (id, leaf_id, state, priority, code_artifact_ref, deadline_seconds)
		VALUES ($1, $2, 'COMPLETED', 'NORMAL', 'ref://test', 3600)`,
		wuID, p.ID,
	)
	if err != nil {
		t.Fatalf("Create work unit: %v", err)
	}

	// Create a result.
	resultID := types.NewID()
	_, err = pool.Exec(ctx, `
		INSERT INTO results (id, work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status, submitted_at)
		VALUES ($1, $2, $3, '{"result": 42}', 'sha256:abc123', '{}', 'PENDING', NOW())`,
		resultID, wuID, volID,
	)
	if err != nil {
		t.Fatalf("Create result: %v", err)
	}

	// Create credit ledger entry (FK RESTRICT on leaf_id).
	_, err = pool.Exec(ctx, `
		INSERT INTO credit_ledger (id, volunteer_id, leaf_id, work_unit_id, result_id, credit_amount, granted_at)
		VALUES ($1, $2, $3, $4, $5, 1.0, NOW())`,
		types.NewID(), volID, p.ID, wuID, resultID,
	)
	if err != nil {
		t.Fatalf("Create credit ledger entry: %v", err)
	}

	// Delete should fail with 409 Conflict.
	err = repo.Delete(ctx, p.ID)
	if err == nil {
		t.Fatal("expected conflict error when deleting leaf with credit history")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}
