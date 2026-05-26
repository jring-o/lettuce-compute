//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"

	"log/slog"
	"os"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestVolunteerStats_NotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	volunteerRepo := volunteer.NewPgxRepository(pool)
	racRepo := NewPgxRACRepository(pool)
	creditRepo := NewPgxRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	logger := testLogger()

	handler := NewVolunteerStatsHandler(pool, volunteerRepo, racRepo, creditRepo, leafRepo, logger)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	nonExistentID := types.NewID()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/"+nonExistentID.String()+"/stats", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestVolunteerStats_InvalidUUID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	volunteerRepo := volunteer.NewPgxRepository(pool)
	racRepo := NewPgxRACRepository(pool)
	creditRepo := NewPgxRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	logger := testLogger()

	handler := NewVolunteerStatsHandler(pool, volunteerRepo, racRepo, creditRepo, leafRepo, logger)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/volunteers/not-a-uuid/stats", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestVolunteerStats_WithProjectBreakdown(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Create test data.
	userID := createTestUser(t, pool, "stats-creator1")
	proj1 := createTestLeaf(t, pool, &userID)
	proj2 := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	// Increment completed work units on the volunteer record.
	_, err := pool.Exec(ctx,
		"UPDATE volunteers SET total_work_units_completed = 3, total_work_units_rejected = 1 WHERE id = $1", volID)
	if err != nil {
		t.Fatalf("update volunteer counters: %v", err)
	}

	// Create RAC entries.
	racRepo := NewPgxRACRepository(pool)
	if err := racRepo.Upsert(ctx, volID, proj1, 10.0); err != nil {
		t.Fatalf("Upsert proj1: %v", err)
	}
	if err := racRepo.Upsert(ctx, volID, proj2, 20.0); err != nil {
		t.Fatalf("Upsert proj2: %v", err)
	}

	// Create credit ledger entries for counting work units per leaf.
	creditRepo := NewPgxRepository(pool)
	wu1 := createTestWorkUnit(t, pool, proj1)
	res1 := createTestResult(t, pool, wu1, volID, "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111")
	if err := creditRepo.Create(ctx, &LedgerEntry{
		VolunteerID: volID, LeafID: proj1, WorkUnitID: wu1, ResultID: res1, CreditAmount: 10.0,
	}); err != nil {
		t.Fatalf("Create credit 1: %v", err)
	}

	wu2 := createTestWorkUnit(t, pool, proj2)
	res2 := createTestResult(t, pool, wu2, volID, "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222")
	if err := creditRepo.Create(ctx, &LedgerEntry{
		VolunteerID: volID, LeafID: proj2, WorkUnitID: wu2, ResultID: res2, CreditAmount: 20.0,
	}); err != nil {
		t.Fatalf("Create credit 2: %v", err)
	}

	wu3 := createTestWorkUnit(t, pool, proj2)
	res3 := createTestResult(t, pool, wu3, volID, "cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333")
	if err := creditRepo.Create(ctx, &LedgerEntry{
		VolunteerID: volID, LeafID: proj2, WorkUnitID: wu3, ResultID: res3, CreditAmount: 20.0,
	}); err != nil {
		t.Fatalf("Create credit 3: %v", err)
	}

	// Create the handler and serve the request.
	volunteerRepo := volunteer.NewPgxRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	logger := testLogger()

	handler := NewVolunteerStatsHandler(pool, volunteerRepo, racRepo, creditRepo, leafRepo, logger)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/volunteers/"+volID.String()+"/stats", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	var resp VolunteerStatsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.VolunteerID != volID {
		t.Errorf("VolunteerID = %v, want %v", resp.VolunteerID, volID)
	}
	if resp.TotalWorkUnitsCompleted != 3 {
		t.Errorf("TotalWorkUnitsCompleted = %d, want 3", resp.TotalWorkUnitsCompleted)
	}
	if resp.TotalWorkUnitsRejected != 1 {
		t.Errorf("TotalWorkUnitsRejected = %d, want 1", resp.TotalWorkUnitsRejected)
	}
	if resp.TotalCredit != 30.0 {
		t.Errorf("TotalCredit = %v, want 30.0", resp.TotalCredit)
	}
	if len(resp.Leafs) != 2 {
		t.Fatalf("Projects count = %d, want 2", len(resp.Leafs))
	}
	if resp.PublicKey == "" {
		t.Error("PublicKey should not be empty")
	}

	// Verify per-leaf breakdown.
	for _, p := range resp.Leafs {
		if p.LeafName == "" {
			t.Error("LeafName should not be empty")
		}
		if p.RAC <= 0 {
			t.Errorf("RAC for project %v should be > 0, got %v", p.LeafID, p.RAC)
		}
	}
}
