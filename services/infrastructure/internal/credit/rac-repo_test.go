//go:build integration

package credit

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestRACUpsert_CreatesNewRow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator1")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	err := repo.Upsert(ctx, volID, leafID, 5.0)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	entry, err := repo.GetByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject: %v", err)
	}
	if entry.TotalCredit != 5.0 {
		t.Errorf("TotalCredit = %v, want 5.0", entry.TotalCredit)
	}
	if entry.RAC != 5.0 {
		t.Errorf("RAC = %v, want 5.0 (initial grant)", entry.RAC)
	}
	if entry.LastCreditAt == nil {
		t.Error("LastCreditAt should be set")
	}
}

func TestRACUpsert_UpdatesExistingRow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator2")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	// First credit.
	if err := repo.Upsert(ctx, volID, leafID, 10.0); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}

	// Second credit.
	if err := repo.Upsert(ctx, volID, leafID, 5.0); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	entry, err := repo.GetByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject: %v", err)
	}

	if entry.TotalCredit != 15.0 {
		t.Errorf("TotalCredit = %v, want 15.0", entry.TotalCredit)
	}
	// RAC should be >= 10 (previous RAC decayed + new credit contribution).
	// With near-zero elapsed time, RAC ≈ previousRAC + newCredit = 15.
	if entry.RAC < 10.0 {
		t.Errorf("RAC = %v, expected >= 10.0 after second grant", entry.RAC)
	}
}

func TestRACGetByVolunteerProject_NotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRACRepository(pool)
	_, err := repo.GetByVolunteerProject(context.Background(), types.NewID(), types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent entry")
	}
}

func TestRACListByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator3")
	proj1 := createTestLeaf(t, pool, &userID)
	proj2 := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	if err := repo.Upsert(ctx, volID, proj1, 10.0); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if err := repo.Upsert(ctx, volID, proj2, 20.0); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	entries, err := repo.ListByVolunteer(ctx, volID)
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Ordered by RAC DESC, so proj2 (20) should be first.
	if entries[0].RAC < entries[1].RAC {
		t.Error("entries should be ordered by RAC DESC")
	}
}

func TestRACListByLeaf_OrderedByRACDesc(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator4")
	leafID := createTestLeaf(t, pool, &userID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	vol3 := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	// Grant different amounts with small delays to ensure distinct created_at.
	if err := repo.Upsert(ctx, vol1, leafID, 5.0); err != nil {
		t.Fatalf("Upsert vol1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repo.Upsert(ctx, vol2, leafID, 20.0); err != nil {
		t.Fatalf("Upsert vol2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repo.Upsert(ctx, vol3, leafID, 10.0); err != nil {
		t.Fatalf("Upsert vol3: %v", err)
	}

	// Page 1 of 2 (ordered by created_at DESC — most recent first).
	entries, pagination, err := repo.ListByLeaf(ctx, leafID, types.PaginationRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListByLeaf: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("page 1: expected 2, got %d", len(entries))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}
	// First entry should be vol3 (most recently created).
	if entries[0].VolunteerID != vol3 {
		t.Errorf("expected vol3 first (most recent), got %v", entries[0].VolunteerID)
	}

	// Page 2.
	entries2, pagination2, err := repo.ListByLeaf(ctx, leafID, types.PaginationRequest{
		PageSize: 2,
		Cursor:   pagination.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListByLeaf page 2: %v", err)
	}
	if len(entries2) != 1 {
		t.Fatalf("page 2: expected 1, got %d", len(entries2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}

func TestRACDecayAll(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator5")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	// Create a RAC entry.
	if err := repo.Upsert(ctx, volID, leafID, 100.0); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Artificially backdate last_updated_at to 2 hours ago.
	_, err := pool.Exec(ctx,
		"UPDATE volunteer_rac SET last_updated_at = NOW() - INTERVAL '2 hours' WHERE volunteer_id = $1 AND leaf_id = $2",
		volID, leafID,
	)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	rows, err := repo.DecayAll(ctx)
	if err != nil {
		t.Fatalf("DecayAll: %v", err)
	}
	if rows != 1 {
		t.Errorf("DecayAll rows = %d, want 1", rows)
	}

	entry, err := repo.GetByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject after decay: %v", err)
	}

	// After 2 hours of decay, RAC should be less than 100.
	if entry.RAC >= 100.0 {
		t.Errorf("RAC = %v, expected < 100.0 after decay", entry.RAC)
	}
	// Two hours = 7200 seconds. decay_factor = exp(-7200 * ln2 / 604800) ≈ 0.9918.
	expectedDecay := math.Exp(-7200 * math.Ln2 / HalfLifeSeconds)
	expectedRAC := 100.0 * expectedDecay
	if math.Abs(entry.RAC-expectedRAC) > 1.0 {
		t.Errorf("RAC = %v, expected ~%v", entry.RAC, expectedRAC)
	}
}

func TestRACDecayAll_SkipsRecentlyUpdated(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator6")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	// Create a freshly updated RAC entry.
	if err := repo.Upsert(ctx, volID, leafID, 100.0); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// DecayAll should skip this entry (updated < 1 hour ago).
	rows, err := repo.DecayAll(ctx)
	if err != nil {
		t.Fatalf("DecayAll: %v", err)
	}
	if rows != 0 {
		t.Errorf("DecayAll rows = %d, want 0 (recently updated)", rows)
	}

	entry, err := repo.GetByVolunteerProject(ctx, volID, leafID)
	if err != nil {
		t.Fatalf("GetByVolunteerProject: %v", err)
	}
	if entry.RAC != 100.0 {
		t.Errorf("RAC = %v, want 100.0 (should not have decayed)", entry.RAC)
	}
}

func TestRACDecayAll_SkipsNearZeroRAC(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "rac-creator7")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	repo := NewPgxRACRepository(pool)
	ctx := context.Background()

	// Create a RAC entry with near-zero RAC.
	if err := repo.Upsert(ctx, volID, leafID, 0.0000000001); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Backdate to make it eligible.
	_, err := pool.Exec(ctx,
		"UPDATE volunteer_rac SET last_updated_at = NOW() - INTERVAL '2 hours' WHERE volunteer_id = $1 AND leaf_id = $2",
		volID, leafID,
	)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	rows, err := repo.DecayAll(ctx)
	if err != nil {
		t.Fatalf("DecayAll: %v", err)
	}
	// Near-zero RAC (< 1e-9) should be skipped.
	if rows != 0 {
		t.Errorf("DecayAll rows = %d, want 0 (near-zero RAC)", rows)
	}
}
