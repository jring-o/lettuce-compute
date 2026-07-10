//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// DB-backed tests for the export settlement layer: maturation netting on the fleet feed
// (boundary, per-entry netting incl. the F6 immature-clawback regression, F16 HAVING,
// F4 default-off equivalence) and the emission anomaly halt (trip, cold-start, cache).
// -tags integration -p 1; skips unless LETTUCE_TEST_DB_URL is set (see setupTestDB). The
// fixture helpers (createTestUser/Leaf/WorkUnit/Volunteer/Result, setupTestDB, testLogger)
// come from the other integration test files in this package.

// seedGrantAt writes a credit_ledger row for (leafID, volID) with amount, then stamps
// granted_at to hoursAgo hours before the DB clock (so boundary comparisons are skew-free —
// everything is relative to the same now()). seq gives each row a unique 64-hex-char result
// checksum. Returns the ledger entry id.
func seedGrantAt(t *testing.T, pool *pgxpool.Pool, repo Repository, leafID, volID types.ID, amount, hoursAgo float64, seq int) types.ID {
	t.Helper()
	ctx := context.Background()
	wu := createTestWorkUnit(t, pool, leafID)
	res := createTestResult(t, pool, wu, volID, fmt.Sprintf("%064x", seq))
	entry := &LedgerEntry{VolunteerID: volID, LeafID: leafID, WorkUnitID: wu, ResultID: res, CreditAmount: amount}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("seed grant create (seq %d): %v", seq, err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE credit_ledger SET granted_at = now() - ($1 * interval '1 hour') WHERE id = $2",
		hoursAgo, entry.ID); err != nil {
		t.Fatalf("seed grant set granted_at (seq %d): %v", seq, err)
	}
	return entry.ID
}

// insertAdjustment writes a compensating negative adjustment directly (decoupled from the
// adjustments repo, another agent's scope). magnitude is negative (CHECK amount < 0).
func insertAdjustment(t *testing.T, pool *pgxpool.Pool, entryID, volID, leafID types.ID, magnitude float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO credit_adjustments (ledger_entry_id, volunteer_id, leaf_id, amount, reason, created_by)
		VALUES ($1, $2, $3, $4, 'TEST_CLAWBACK', 'OPERATOR')`,
		entryID, volID, leafID, magnitude)
	if err != nil {
		t.Fatalf("insert adjustment: %v", err)
	}
}

func newSettlementHandler(pool *pgxpool.Pool, cfg *SettlementExportConfig) *VolunteerStatsHandler {
	h := NewVolunteerStatsHandler(
		pool,
		volunteer.NewPgxRepository(pool),
		NewPgxRACRepository(pool),
		NewPgxRepository(pool),
		leaf.NewPgxRepository(pool),
		testLogger(),
	)
	if cfg != nil {
		h = h.WithSettlement(cfg)
	}
	return h
}

func serveFleetFeed(t *testing.T, h *VolunteerStatsHandler) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil))
	return rr, rr.Body.Bytes()
}

func findVolEntry(entries []AllVolunteerStatsEntry, id types.ID) (AllVolunteerStatsEntry, bool) {
	for _, e := range entries {
		if e.VolunteerID == id {
			return e, true
		}
	}
	return AllVolunteerStatsEntry{}, false
}

const maturationTestDays = 7

// (a) Maturation boundary: an entry just OLDER than T days is matured (included); one just
// YOUNGER is immature (excluded).
func TestFleetFeed_MaturationBoundary(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "mat-boundary")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	// Just outside T (older) -> matured; just inside T (younger) -> immature.
	seedGrantAt(t, pool, repo, leafID, volID, 10.0, maturationTestDays*24+1, 1) // matured
	seedGrantAt(t, pool, repo, leafID, volID, 5.0, maturationTestDays*24-1, 2)  // immature

	h := newSettlementHandler(pool, &SettlementExportConfig{ExportEnabled: true, MaturationDays: maturationTestDays})
	rr, body := serveFleetFeed(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, body)
	}

	var resp AllVolunteerStatsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MaturationDays != maturationTestDays {
		t.Errorf("maturation_days = %d, want %d", resp.MaturationDays, maturationTestDays)
	}
	e, ok := findVolEntry(resp.Volunteers, volID)
	if !ok {
		t.Fatalf("volunteer missing from feed; want present with matured credit")
	}
	if e.TotalCredit != 10.0 {
		t.Errorf("TotalCredit = %v, want 10.0 (only the matured entry counts)", e.TotalCredit)
	}
}

// (b) Per-entry netting (audit F6): an adjustment on a MATURED entry reduces the feed; an
// adjustment on an IMMATURE entry does NOT change matured totals. A wrong per-account sum
// would subtract the immature clawback too.
func TestFleetFeed_PerEntryNetting(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "per-entry-net")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	entryA := seedGrantAt(t, pool, repo, leafID, volID, 10.0, maturationTestDays*24+2, 1) // matured
	seedGrantAt(t, pool, repo, leafID, volID, 8.0, maturationTestDays*24+2, 2)            // matured
	entryC := seedGrantAt(t, pool, repo, leafID, volID, 6.0, 1, 3)                        // immature (1h old)

	insertAdjustment(t, pool, entryA, volID, leafID, -4.0) // reduces matured A: 10 -> 6
	insertAdjustment(t, pool, entryC, volID, leafID, -6.0) // fully claws IMMATURE C (must not matter)

	h := newSettlementHandler(pool, &SettlementExportConfig{ExportEnabled: true, MaturationDays: maturationTestDays})
	rr, body := serveFleetFeed(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, body)
	}
	var resp AllVolunteerStatsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	e, ok := findVolEntry(resp.Volunteers, volID)
	if !ok {
		t.Fatalf("volunteer missing from feed")
	}
	// Correct per-entry: (10-4) + 8 = 14; C is immature and excluded. A wrong per-account
	// netting would give (10+8) - (4+6) = 8.
	if e.TotalCredit != 14.0 {
		t.Errorf("TotalCredit = %v, want 14.0 (per-entry net of matured entries only; F6)", e.TotalCredit)
	}
}

// (c) Fully-clawed matured volunteer disappears from the feed (audit F16 HAVING > 0).
func TestFleetFeed_FullyClawedDisappears(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "fully-clawed")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	entry := seedGrantAt(t, pool, repo, leafID, volID, 10.0, maturationTestDays*24+2, 1)
	insertAdjustment(t, pool, entry, volID, leafID, -10.0) // full cancel

	h := newSettlementHandler(pool, &SettlementExportConfig{ExportEnabled: true, MaturationDays: maturationTestDays})
	rr, body := serveFleetFeed(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, body)
	}
	var resp AllVolunteerStatsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := findVolEntry(resp.Volunteers, volID); ok {
		t.Errorf("fully-clawed volunteer still present in feed; HAVING SUM(net) > 0 should drop it")
	}
}

// (d) Default-off equivalence (audit F4): with maturation off, a nil settlement config and
// an explicit MaturationDays:0 config produce the same feed (modulo generated_at), and
// neither response carries a maturation_days key.
func TestFleetFeed_DefaultOffEquivalence(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "default-off")
	leafID := createTestLeaf(t, pool, &userID)
	vol1 := createTestVolunteer(t, pool)
	vol2 := createTestVolunteer(t, pool)
	// Recent grants, NO adjustments: the default (raw-sum) path includes them all.
	seedGrantAt(t, pool, repo, leafID, vol1, 3.0, 1, 1)
	seedGrantAt(t, pool, repo, leafID, vol1, 2.0, 100, 2)
	seedGrantAt(t, pool, repo, leafID, vol2, 7.0, 1, 3)

	// nil config = inert default path.
	rrNil, bodyNil := serveFleetFeed(t, newSettlementHandler(pool, nil))
	// Explicit MaturationDays:0 must route through the SAME default query (not netting-with-0).
	rrZero, bodyZero := serveFleetFeed(t, newSettlementHandler(pool,
		&SettlementExportConfig{ExportEnabled: true, MaturationDays: 0}))

	if rrNil.Code != http.StatusOK || rrZero.Code != http.StatusOK {
		t.Fatalf("status nil=%d zero=%d, want 200/200", rrNil.Code, rrZero.Code)
	}
	if strings.Contains(string(bodyNil), "maturation_days") {
		t.Errorf("nil-config response contains maturation_days key: %s", bodyNil)
	}
	if strings.Contains(string(bodyZero), "maturation_days") {
		t.Errorf("MaturationDays:0 response contains maturation_days key: %s", bodyZero)
	}

	var respNil, respZero AllVolunteerStatsResponse
	if err := json.Unmarshal(bodyNil, &respNil); err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if err := json.Unmarshal(bodyZero, &respZero); err != nil {
		t.Fatalf("decode zero: %v", err)
	}
	// Normalize the one legitimately non-deterministic field (types.Now()).
	respNil.GeneratedAt = ""
	respZero.GeneratedAt = ""
	if !reflect.DeepEqual(respNil, respZero) {
		t.Errorf("default-off feeds differ:\n nil=%+v\nzero=%+v", respNil, respZero)
	}
}

// seedAnomalyBaseline seeds `days` distinct baseline days (each amountPerDay) in
// [now()-31d, now()-1d), plus a single burst today.
func seedAnomalyBaseline(t *testing.T, pool *pgxpool.Pool, repo Repository, leafID, volID types.ID, days int, amountPerDay, burstToday float64) {
	t.Helper()
	seq := 1000
	for d := 2; d < 2+days; d++ { // 2..(days+1) days ago: distinct calendar days, all < now()-1d
		seedGrantAt(t, pool, repo, leafID, volID, amountPerDay, float64(d*24), seq)
		seq++
	}
	seedGrantAt(t, pool, repo, leafID, volID, burstToday, 1, seq) // burst 1h ago (today window)
}

// (e) Anomaly halt trips: an armed baseline plus a burst > factor*baseline halts the checker
// and freezes the feed with the anomaly-halt header.
func TestAnomalyHalt_TripsAndFreezesFeed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "anomaly-trip")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	// 10 distinct baseline days x 10 -> windowSum 100, baseline 100/30 ~= 3.333 (armed).
	// Burst today 20 > 3.0 * 3.333 = 10 -> halted.
	seedAnomalyBaseline(t, pool, repo, leafID, volID, 10, 10.0, 20.0)

	checker := NewAnomalyChecker(pool, 3.0)
	v, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Armed {
		t.Fatalf("Armed = false, want true (10 distinct baseline days)")
	}
	if !v.Halted {
		t.Fatalf("Halted = false, want true (today %v > 3x baseline %v)", v.Today, v.Baseline)
	}

	h := newSettlementHandler(pool, &SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: true,
		AnomalyFactor:      3.0,
		AnomalyChecker:     checker,
	})
	rr, body := serveFleetFeed(t, h)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", rr.Code, body)
	}
	if got := rr.Header().Get("X-Lettuce-Export-Status"); got != "anomaly-halt" {
		t.Errorf("X-Lettuce-Export-Status = %q, want anomaly-halt", got)
	}
}

// (e cold-start) Below 7 distinct baseline days the breaker is unarmed: even a huge burst
// does not halt, and the feed serves.
func TestAnomalyHalt_ColdStartServes(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "anomaly-cold")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	// Only 3 distinct baseline days -> unarmed regardless of a huge burst.
	seedAnomalyBaseline(t, pool, repo, leafID, volID, 3, 10.0, 1000.0)

	checker := NewAnomalyChecker(pool, 3.0)
	v, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v.Armed {
		t.Fatalf("Armed = true with 3 distinct days, want false")
	}
	if v.Halted {
		t.Fatalf("Halted = true while unarmed, want false")
	}

	h := newSettlementHandler(pool, &SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: true,
		AnomalyFactor:      3.0,
		AnomalyChecker:     checker,
	})
	rr, body := serveFleetFeed(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unarmed head serves), body: %s", rr.Code, body)
	}
}

// The 60s verdict cache serves the second call from memory: a verdict computed while halted
// stays halted across an immediate re-check even after the ledger is emptied.
func TestAnomalyChecker_VerdictCached(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	userID := createTestUser(t, pool, "anomaly-cache")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	seedAnomalyBaseline(t, pool, repo, leafID, volID, 10, 10.0, 20.0)

	checker := NewAnomalyChecker(pool, 3.0)
	first, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if !first.Halted {
		t.Fatalf("first verdict Halted = false, want true")
	}

	// Empty the ledger: a fresh evaluation would now report not-halted. The cached verdict
	// (< 60s old) must still be served. Adjustments go first — their FK to the ledger is
	// ON DELETE RESTRICT, so a bare ledger delete would fail if any test left one behind.
	if _, err := pool.Exec(context.Background(), "DELETE FROM credit_adjustments"); err != nil {
		t.Fatalf("delete adjustments: %v", err)
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM credit_ledger"); err != nil {
		t.Fatalf("delete ledger: %v", err)
	}
	second, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if !second.Halted {
		t.Fatalf("second verdict Halted = false; the 60s cache should have served the first verdict")
	}
}
