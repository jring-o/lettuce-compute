//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// insertCreditAt creates a new work unit + AGREED result for the leaf and writes a
// credit_ledger row of `amount` stamped at `grantedAt`, so a test can place credit
// on specific days/weeks for the timeline.
func insertCreditAt(t *testing.T, pool *pgxpool.Pool, leafID, volID types.ID, amount float64, grantedAt time.Time) {
	t.Helper()
	wuID := createTestWorkUnit(t, pool, leafID)
	resID := createTestResult(t, pool, wuID, volID,
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	_, err := pool.Exec(context.Background(), `
		INSERT INTO credit_ledger (volunteer_id, leaf_id, work_unit_id, result_id, credit_amount, granted_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		volID, leafID, wuID, resID, amount, grantedAt,
	)
	if err != nil {
		t.Fatalf("insertCreditAt: %v", err)
	}
}

// TestComputeVolunteerBreakdown exercises the shared breakdown function directly:
// total credit, per-leaf rows, the resource-type split, and timeline buckets.
func TestComputeVolunteerBreakdown(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "breakdown-fn")
	leafA := createTestLeaf(t, pool, &userID)
	leafB := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	now := time.Now().UTC()
	insertCreditAt(t, pool, leafA, volID, 2.0, now)
	insertCreditAt(t, pool, leafB, volID, 3.0, now)

	bd, err := ComputeVolunteerBreakdown(context.Background(), pool, volID)
	if err != nil {
		t.Fatalf("ComputeVolunteerBreakdown: %v", err)
	}

	if bd.VolunteerID != volID {
		t.Errorf("volunteer_id = %v, want %v", bd.VolunteerID, volID)
	}
	if bd.TotalCredit != 5.0 {
		t.Errorf("total_credit = %v, want 5.0", bd.TotalCredit)
	}
	if len(bd.ByLeaf) != 2 {
		t.Errorf("by_leaf len = %d, want 2", len(bd.ByLeaf))
	}
	// createTestResult records cpu_seconds_user but no gpu_seconds, so all credit
	// lands in the cpu_only bucket.
	if got := bd.ByResourceType["cpu_only"].Credit; got != 5.0 {
		t.Errorf("cpu_only credit = %v, want 5.0", got)
	}
	if got := bd.ByResourceType["gpu"].Credit; got != 0 {
		t.Errorf("gpu credit = %v, want 0", got)
	}
	if len(bd.Timeline.Daily) == 0 {
		t.Error("daily timeline empty")
	}
	if len(bd.Timeline.Weekly) == 0 {
		t.Error("weekly timeline empty")
	}
}

// TestHandleVolunteerBreakdownTimeline is the regression test for the credit
// breakdown timeline. Before the fix, the daily/weekly queries selected a Postgres
// date / timestamptz into a Go string; pgx cannot scan that, every Scan errored,
// and the loop swallowed the error (appending only on scanErr == nil) — so the
// timeline always came back empty even when credit existed. The handler now casts
// to text in SQL and surfaces scan errors, so the buckets populate.
func TestHandleVolunteerBreakdownTimeline(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "breakdown-timeline")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	now := time.Now().UTC()
	// Three credit rows on three distinct days, all within the last 30 days and
	// 12 weeks: 1.0 today, 2.0 yesterday, 3.0 nine days ago.
	insertCreditAt(t, pool, leafID, volID, 1.0, now)
	insertCreditAt(t, pool, leafID, volID, 2.0, now.AddDate(0, 0, -1))
	insertCreditAt(t, pool, leafID, volID, 3.0, now.AddDate(0, 0, -9))

	h := NewAnalysisHandler(pool, nil, slog.Default())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", h.HandleVolunteerBreakdown)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/volunteers/"+volID.String()+"/credit/breakdown", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp VolunteerBreakdown
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	if resp.TotalCredit != 6.0 {
		t.Errorf("total_credit = %v, want 6.0", resp.TotalCredit)
	}

	// The timeline is the regression target.
	if len(resp.Timeline.Daily) != 3 {
		t.Errorf("daily buckets = %d, want 3 (three distinct days)", len(resp.Timeline.Daily))
	}
	if len(resp.Timeline.Weekly) == 0 {
		t.Error("timeline.weekly is empty; expected at least one weekly bucket")
	}

	var dailySum float64
	for _, d := range resp.Timeline.Daily {
		if d.Date == "" {
			t.Error("daily bucket has empty date string")
		}
		dailySum += d.Credit
	}
	if dailySum != 6.0 {
		t.Errorf("daily timeline sum = %v, want 6.0", dailySum)
	}

	var weeklySum float64
	for _, w := range resp.Timeline.Weekly {
		if w.WeekStart == "" {
			t.Error("weekly bucket has empty week_start string")
		}
		weeklySum += w.Credit
	}
	if weeklySum != 6.0 {
		t.Errorf("weekly timeline sum = %v, want 6.0", weeklySum)
	}
}

// TestHandleLeafAnalysis_LabelsUnverifiedMetrics is a BG-06a item-3 regression
// (fails on pre-fix code): the leaf-analysis response aggregates volunteer-reported
// execution_metadata (cpu/gpu/wall/memory percentiles), so the served JSON must carry
// the unverified-metrics provenance marker.
func TestHandleLeafAnalysis_LabelsUnverifiedMetrics(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "leaf-analysis-prov")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	wuID := createTestWorkUnit(t, pool, leafID)
	createTestResult(t, pool, wuID, volID,
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	h := NewAnalysisHandler(pool, nil, slog.Default())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/analysis", h.HandleLeafAnalysis)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/leafs/"+leafID.String()+"/analysis", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), wantMarker) {
		t.Errorf("leaf-analysis response missing provenance marker %s\ngot: %s", wantMarker, rec.Body.String())
	}
}

// TestHandleCrossLeaf_LabelsUnverifiedMetrics is a BG-06a item-3 regression (fails on
// pre-fix code): the cross-leaf response aggregates volunteer-reported
// execution_metadata (avg cpu/gpu-seconds per credit), so the served JSON must carry
// the unverified-metrics provenance marker at the envelope top level.
func TestHandleCrossLeaf_LabelsUnverifiedMetrics(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "cross-leaf-prov")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	insertCreditAt(t, pool, leafID, volID, 2.0, time.Now().UTC())

	h := NewAnalysisHandler(pool, nil, slog.Default())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/analysis/cross-leaf", h.HandleCrossLeaf)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/analysis/cross-leaf", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), wantMarker) {
		t.Errorf("cross-leaf response missing provenance marker %s\ngot: %s", wantMarker, rec.Body.String())
	}
}

// TestHandleVolunteerBreakdown_LabelsUnverifiedMetrics is a BG-06a item-3 regression
// (fails on pre-fix code): the volunteer credit breakdown response aggregates
// volunteer-reported execution_metadata (per-leaf/per-host cpu/gpu-seconds), so the
// served JSON must carry the unverified-metrics provenance marker.
func TestHandleVolunteerBreakdown_LabelsUnverifiedMetrics(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "breakdown-prov")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	insertCreditAt(t, pool, leafID, volID, 2.0, time.Now().UTC())

	h := NewAnalysisHandler(pool, nil, slog.Default())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", h.HandleVolunteerBreakdown)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/volunteers/"+volID.String()+"/credit/breakdown", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), wantMarker) {
		t.Errorf("breakdown response missing provenance marker %s\ngot: %s", wantMarker, rec.Body.String())
	}
}
