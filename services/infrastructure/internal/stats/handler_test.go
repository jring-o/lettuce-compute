//go:build integration

package stats_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setupHandlerServer creates an httptest.Server with project and stats
// handlers wired up.
func setupHandlerServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, func()) {
	t.Helper()

	pool, poolCleanup := setupTestDB(t)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	leafRepo := leaf.NewPgxRepository(pool)
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)

	engine := stats.NewEngine(pool)
	statsHandler := stats.NewStatsHandler(engine, leafRepo, logger)

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)

	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		poolCleanup()
	}
	return ts, pool, cleanup
}

func doRequest(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func requireStatus(t *testing.T, resp *http.Response, want int, context string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s: expected %d, got %d: %s", context, want, resp.StatusCode, body)
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// createHandlerProject creates a project in ACTIVE state with work units.
func createHandlerProject(t *testing.T, pool *pgxpool.Pool, name string, wuStates []string) types.ID {
	t.Helper()
	userID := createTestUser(t, pool, name)
	leafID := createTestLeaf(t, pool, userID, name)
	if len(wuStates) > 0 {
		createTestWorkUnits(t, pool, leafID, wuStates)
	}
	return leafID
}

func TestHandlerGetStats(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	leafID := createHandlerProject(t, pool, "hstat1", []string{
		"QUEUED", "QUEUED", "QUEUED", "ASSIGNED", "RUNNING",
	})

	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats", ts.URL, leafID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats")

	var snap stats.LeafStatsSnapshot
	decodeJSON(t, resp, &snap)

	if snap.TotalWorkUnits != 5 {
		t.Errorf("total_work_units = %d, want 5", snap.TotalWorkUnits)
	}
	if snap.WorkUnitsQueued != 3 {
		t.Errorf("work_units_queued = %d, want 3", snap.WorkUnitsQueued)
	}
	if snap.WorkUnitsAssigned != 1 {
		t.Errorf("work_units_assigned = %d, want 1", snap.WorkUnitsAssigned)
	}
	if snap.WorkUnitsRunning != 1 {
		t.Errorf("work_units_running = %d, want 1", snap.WorkUnitsRunning)
	}
	// 1 ASSIGNED + 1 RUNNING live copy, each created with a distinct volunteer.
	if snap.ActiveVolunteers != 2 {
		t.Errorf("active_volunteers = %d, want 2 (distinct volunteers on live copies)", snap.ActiveVolunteers)
	}
}

func TestHandlerGetStatsNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats", ts.URL, fakeID)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerGetStatsHistory(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	leafID := createHandlerProject(t, pool, "hstat2", []string{"QUEUED", "QUEUED"})

	// Compute a snapshot first so history has data.
	engine := stats.NewEngine(pool)
	_, err := engine.ComputeSnapshot(t.Context(), leafID)
	if err != nil {
		t.Fatalf("ComputeSnapshot failed: %v", err)
	}

	from := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)

	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats/history?from=%s&to=%s",
		ts.URL, leafID, from, to)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats history")

	var histResp struct {
		Data []stats.LeafStatsSnapshot `json:"data"`
	}
	decodeJSON(t, resp, &histResp)

	if len(histResp.Data) < 1 {
		t.Fatalf("expected at least 1 snapshot, got %d", len(histResp.Data))
	}
	if histResp.Data[0].TotalWorkUnits != 2 {
		t.Errorf("total_work_units = %d, want 2", histResp.Data[0].TotalWorkUnits)
	}
}

func TestHandlerGetStatsHistoryMissingFrom(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	leafID := createHandlerProject(t, pool, "hstat3", nil)

	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats/history", ts.URL, leafID)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerGetStatsHistoryNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	from := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats/history?from=%s", ts.URL, fakeID, from)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandlerGetStatsCustomCacheSeconds verifies that the stats handler
// uses the leaf's stats_cache_seconds setting to determine cache freshness.
func TestHandlerGetStatsCustomCacheSeconds(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	leafID := createHandlerProject(t, pool, "hstat-cache", []string{"QUEUED", "QUEUED"})

	// Set a very short cache (5 seconds) on the leaf.
	_, err := pool.Exec(t.Context(),
		"UPDATE leafs SET stats_cache_seconds = 5 WHERE id = $1", leafID)
	if err != nil {
		t.Fatalf("failed to set stats_cache_seconds: %v", err)
	}

	// First request: computes a fresh snapshot.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats", ts.URL, leafID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats (1st)")

	var snap1 stats.LeafStatsSnapshot
	decodeJSON(t, resp, &snap1)

	if snap1.TotalWorkUnits != 2 {
		t.Errorf("snap1 total_work_units = %d, want 2", snap1.TotalWorkUnits)
	}

	// Add more work units.
	createTestWorkUnits(t, pool, leafID, []string{"QUEUED"})

	// Second request within 5s cache window: should return cached snapshot.
	resp = doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats (2nd, cached)")

	var snap2 stats.LeafStatsSnapshot
	decodeJSON(t, resp, &snap2)

	if snap2.ID != snap1.ID {
		t.Errorf("expected cached snapshot (same ID), got different: %v vs %v", snap2.ID, snap1.ID)
	}
	if snap2.TotalWorkUnits != 2 {
		t.Errorf("cached snap should still show 2, got %d", snap2.TotalWorkUnits)
	}
}

// TestHandlerGetStatsDefaultCacheSeconds verifies the handler uses the
// default cache period when stats_cache_seconds is 0 (unset).
func TestHandlerGetStatsDefaultCacheSeconds(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	leafID := createHandlerProject(t, pool, "hstat-defcache", []string{"QUEUED"})

	// stats_cache_seconds is 0 by default. The handler should fall back
	// to defaultMaxAgeSeconds (60s). Verify by fetching stats and confirming
	// a subsequent call returns the same cached snapshot.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/stats", ts.URL, leafID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats (default cache 1st)")

	var snap1 stats.LeafStatsSnapshot
	decodeJSON(t, resp, &snap1)

	// Add more work units.
	createTestWorkUnits(t, pool, leafID, []string{"QUEUED"})

	// Should still return cached snapshot (within 60s default).
	resp = doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "get stats (default cache 2nd)")

	var snap2 stats.LeafStatsSnapshot
	decodeJSON(t, resp, &snap2)

	if snap2.ID != snap1.ID {
		t.Errorf("expected cached snapshot (same ID), got different: %v vs %v", snap2.ID, snap1.ID)
	}
}
