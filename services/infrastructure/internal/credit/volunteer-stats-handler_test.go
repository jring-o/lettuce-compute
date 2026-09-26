//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

	// Seed the per-volunteer running counters with DELIBERATELY WRONG values. The
	// stats endpoint must IGNORE these stale caches and report ledger-derived
	// numbers instead — this is the regression guard for the counter-vs-ledger
	// drift that surfaced as total_work_units_completed >> the per-leaf sum.
	_, err := pool.Exec(ctx,
		"UPDATE volunteers SET total_work_units_completed = 999, total_work_units_rejected = 7 WHERE id = $1", volID)
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

	// One DISAGREED result (no credit_ledger row): the endpoint must report exactly
	// one rejected work unit from the results table, not the stale "7" counter.
	wu4 := createTestWorkUnit(t, pool, proj1)
	res4 := createTestResult(t, pool, wu4, volID, "dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444")
	if _, err := pool.Exec(ctx, "UPDATE results SET validation_status = 'DISAGREED' WHERE id = $1", res4); err != nil {
		t.Fatalf("mark result DISAGREED: %v", err)
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
	// Ledger-derived totals, NOT the stale 999/7 counters.
	if resp.TotalWorkUnitsCompleted != 3 {
		t.Errorf("TotalWorkUnitsCompleted = %d, want 3 (ledger row count, not the stale 999 counter)", resp.TotalWorkUnitsCompleted)
	}
	if resp.TotalWorkUnitsRejected != 1 {
		t.Errorf("TotalWorkUnitsRejected = %d, want 1 (DISAGREED results, not the stale 7 counter)", resp.TotalWorkUnitsRejected)
	}
	if resp.TotalCredit != 50.0 {
		t.Errorf("TotalCredit = %v, want 50.0 (ledger sum 10+20+20, not the volunteer_rac accumulator)", resp.TotalCredit)
	}
	if len(resp.Leafs) != 2 {
		t.Fatalf("Projects count = %d, want 2", len(resp.Leafs))
	}
	if resp.PublicKey == "" {
		t.Error("PublicKey should not be empty")
	}

	// Per-leaf credit and work-unit counts must also come from the ledger.
	byLeaf := make(map[types.ID]LeafStatsEntry, len(resp.Leafs))
	for _, p := range resp.Leafs {
		byLeaf[p.LeafID] = p
		if p.LeafName == "" {
			t.Error("LeafName should not be empty")
		}
		if p.RAC <= 0 {
			t.Errorf("RAC for project %v should be > 0, got %v", p.LeafID, p.RAC)
		}
	}
	if got := byLeaf[proj1]; got.WorkUnitsCompleted != 1 || got.TotalCredit != 10.0 {
		t.Errorf("proj1 breakdown = {wu:%d credit:%v}, want {wu:1 credit:10}", got.WorkUnitsCompleted, got.TotalCredit)
	}
	if got := byLeaf[proj2]; got.WorkUnitsCompleted != 2 || got.TotalCredit != 40.0 {
		t.Errorf("proj2 breakdown = {wu:%d credit:%v}, want {wu:2 credit:40}", got.WorkUnitsCompleted, got.TotalCredit)
	}
}

// TestVolunteerStats_NamesPublicLeafsOnly: the public per-account stats name only
// PUBLIC leafs. An account with credit on a PUBLIC, an UNLISTED and a PRIVATE leaf gets
// one per-leaf entry, and neither hidden leaf's id nor its name appears anywhere in the
// body. The account totals keep every leaf's share, the response keeps its field set,
// and the shared breakdown behind the account's own view and the operator breakdown
// still lists all three.
func TestVolunteerStats_NamesPublicLeafsOnly(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	userID := createTestUser(t, pool, "stats-visibility")
	publicLeaf, publicName := createTestLeafWithVisibility(t, pool, &userID, "PUBLIC")
	unlistedLeaf, unlistedName := createTestLeafWithVisibility(t, pool, &userID, "UNLISTED")
	privateLeaf, privateName := createTestLeafWithVisibility(t, pool, &userID, "PRIVATE")
	volID := createTestVolunteer(t, pool)

	// One credited unit per leaf, with distinct amounts so each total shows which leafs
	// it counted: 1 + 2 + 4.
	creditRepo := NewPgxRepository(pool)
	racRepo := NewPgxRACRepository(pool)
	seeds := []struct {
		leaf   types.ID
		amount float64
		sum    string
	}{
		{publicLeaf, 1, "a"},
		{unlistedLeaf, 2, "b"},
		{privateLeaf, 4, "c"},
	}
	for _, s := range seeds {
		wu := createTestWorkUnit(t, pool, s.leaf)
		res := createTestResult(t, pool, wu, volID, strings.Repeat(s.sum, 64))
		if err := creditRepo.Create(ctx, &LedgerEntry{
			VolunteerID: volID, LeafID: s.leaf, WorkUnitID: wu, ResultID: res, CreditAmount: s.amount,
		}); err != nil {
			t.Fatalf("create credit on %v: %v", s.leaf, err)
		}
		if err := racRepo.Upsert(ctx, volID, s.leaf, s.amount); err != nil {
			t.Fatalf("upsert rac on %v: %v", s.leaf, err)
		}
	}

	leafRepo := leaf.NewPgxRepository(pool)
	handler := NewVolunteerStatsHandler(pool, volunteer.NewPgxRepository(pool), racRepo, creditRepo, leafRepo, testLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/volunteers/"+volID.String()+"/stats", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, hidden := range []string{unlistedLeaf.String(), unlistedName, privateLeaf.String(), privateName} {
		if strings.Contains(body, hidden) {
			t.Errorf("public per-account stats name a non-PUBLIC leaf (%q): %s", hidden, body)
		}
	}

	var resp VolunteerStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Leafs) != 1 {
		t.Fatalf("leafs = %+v, want only the PUBLIC leaf", resp.Leafs)
	}
	got := resp.Leafs[0]
	if got.LeafID != publicLeaf || got.LeafName != publicName || got.TotalCredit != 1 || got.RAC != 1 || got.WorkUnitsCompleted != 1 {
		t.Errorf("PUBLIC entry = %+v, want {%v %q credit 1, rac 1, 1 unit}", got, publicLeaf, publicName)
	}

	// The totals keep the hidden leafs' share (the fleet feed counts it too).
	if resp.TotalCredit != 7 {
		t.Errorf("total_credit = %v, want 7 (every leaf's credit)", resp.TotalCredit)
	}
	if resp.TotalWorkUnitsCompleted != 3 {
		t.Errorf("total_work_units_completed = %d, want 3 (every leaf's units)", resp.TotalWorkUnitsCompleted)
	}

	// The field set is unchanged.
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &shape); err != nil {
		t.Fatalf("decode response shape: %v", err)
	}
	assertJSONKeys(t, "response", shape,
		"volunteer_id", "public_key", "total_credit", "total_work_units_completed", "total_work_units_rejected", "leafs")
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(shape["leafs"], &entries); err != nil {
		t.Fatalf("decode leafs shape: %v", err)
	}
	for _, e := range entries {
		assertJSONKeys(t, "leaf entry", e, "leaf_id", "leaf_name", "total_credit", "rac", "work_units_completed")
	}

	// The shared breakdown (the account's own view) still lists all three leafs.
	bd, err := ComputeVolunteerBreakdown(ctx, pool, volID)
	if err != nil {
		t.Fatalf("ComputeVolunteerBreakdown: %v", err)
	}
	if n := len(bd.ByLeaf); n != 3 {
		t.Errorf("shared breakdown lists %d leafs, want 3 (it must stay unfiltered)", n)
	}

	// So does the operator breakdown.
	analysis := NewAnalysisHandler(pool, leafRepo, testLogger())
	opMux := http.NewServeMux()
	opMux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", analysis.HandleVolunteerBreakdown)
	opReq := httptest.NewRequest(http.MethodGet, "/api/v1/volunteers/"+volID.String()+"/credit/breakdown", nil)
	opRec := httptest.NewRecorder()
	opMux.ServeHTTP(opRec, opReq)
	if opRec.Code != http.StatusOK {
		t.Fatalf("operator breakdown status = %d, body = %s", opRec.Code, opRec.Body.String())
	}
	var op VolunteerBreakdown
	if err := json.Unmarshal(opRec.Body.Bytes(), &op); err != nil {
		t.Fatalf("decode operator breakdown: %v", err)
	}
	seen := map[types.ID]bool{}
	for _, lc := range op.ByLeaf {
		seen[lc.LeafID] = true
	}
	if !seen[publicLeaf] || !seen[unlistedLeaf] || !seen[privateLeaf] {
		t.Errorf("operator breakdown by_leaf = %+v, want all three leafs", op.ByLeaf)
	}
}

// assertJSONKeys fails unless obj has exactly the given keys.
func assertJSONKeys(t *testing.T, what string, obj map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(obj) != len(want) {
		t.Errorf("%s keys = %v, want exactly %v", what, keysOf(obj), want)
		return
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s keys = %v, want exactly %v", what, keysOf(obj), want)
			return
		}
	}
}

func keysOf(obj map[string]json.RawMessage) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
