//go:build integration

package leaf

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/database"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func setupHeadHandlerServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	if err := database.RunMigrations(dbURL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	pool, poolCleanup := setupTestDB(t)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	headCfg := &config.HeadConfig{
		Name:        "test-head",
		Description: "A test head for unit tests",
		URL:         "https://test-head.example.com",
		DefaultLeafWeights: map[string]int{
			"leaf-a": 70,
			"leaf-b": 30,
		},
	}
	handler := NewHeadHandler(headCfg, pool, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/head", handler.HandleGetHeadInfo)

	// Also register leaf CRUD for creating test leafs.
	leafHandler := NewLeafHandler(NewPgxRepository(pool), pool, logger)
	leafHandler.RegisterRoutes(mux)
	// Create binds creator_id to the caller (★BG-11d-write); drive it as an
	// operator (admin viewer) so an explicit body creator_id is honored.
	mux.HandleFunc("POST /api/v1/leafs", withAdminViewer(leafHandler.HandleCreate))
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)

	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		poolCleanup()
	}
	return ts, pool, cleanup
}

func TestHeadInfo_EmptyLeafs(t *testing.T) {
	ts, _, cleanup := setupHeadHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/head", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var headInfo HeadInfoResponse
	decodeJSON(t, resp, &headInfo)

	if headInfo.Name != "test-head" {
		t.Errorf("name = %q, want %q", headInfo.Name, "test-head")
	}
	if headInfo.Description != "A test head for unit tests" {
		t.Errorf("description = %q, want %q", headInfo.Description, "A test head for unit tests")
	}
	if headInfo.URL != "https://test-head.example.com" {
		t.Errorf("url = %q, want %q", headInfo.URL, "https://test-head.example.com")
	}
	if len(headInfo.Leafs) != 0 {
		t.Errorf("leafs count = %d, want 0", len(headInfo.Leafs))
	}
	if len(headInfo.DefaultLeafWeights) != 2 {
		t.Errorf("default_leaf_weights count = %d, want 2", len(headInfo.DefaultLeafWeights))
	}
}

func TestHeadInfo_CacheControlHeader(t *testing.T) {
	ts, _, cleanup := setupHeadHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/head", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cc := resp.Header.Get("Cache-Control")
	if cc != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=60")
	}
	resp.Body.Close()
}

func TestHeadInfo_OnlyActivePublicLeafs(t *testing.T) {
	ts, pool, cleanup := setupHeadHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "head-test")

	// Create 3 leafs: one active+public, one active+private, one draft+public.
	activePublic := createLeafForHeadTest(t, ts, &userID, "Active Public", VisibilityPublic, true)
	createLeafForHeadTest(t, ts, &userID, "Active Private", VisibilityPrivate, true)
	createLeafForHeadTest(t, ts, &userID, "Draft Public", VisibilityPublic, false)

	resp := doRequest(t, "GET", ts.URL+"/api/v1/head", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var headInfo HeadInfoResponse
	decodeJSON(t, resp, &headInfo)

	if len(headInfo.Leafs) != 1 {
		t.Fatalf("leafs count = %d, want 1 (only active+public)", len(headInfo.Leafs))
	}
	if headInfo.Leafs[0].ID != activePublic.String() {
		t.Errorf("leaf id = %q, want %q", headInfo.Leafs[0].ID, activePublic.String())
	}
	if headInfo.Leafs[0].State != "ACTIVE" {
		t.Errorf("leaf state = %q, want ACTIVE", headInfo.Leafs[0].State)
	}
}

func TestHeadInfo_LeafHasQueuedWUCount(t *testing.T) {
	ts, pool, cleanup := setupHeadHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "head-wu-test")
	leafID := createLeafForHeadTest(t, ts, &userID, "WU Count Leaf", VisibilityPublic, true)

	// Insert some queued work units directly.
	for i := 0; i < 5; i++ {
		wuID := types.NewID()
		_, err := pool.Exec(t.Context(), `
			INSERT INTO work_units (id, leaf_id, state, priority, code_artifact_ref, deadline_seconds)
			VALUES ($1, $2, 'QUEUED', 'NORMAL', 'ref://test', 3600)`, wuID, leafID)
		if err != nil {
			t.Fatalf("insert WU: %v", err)
		}
	}

	resp := doRequest(t, "GET", ts.URL+"/api/v1/head", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var headInfo HeadInfoResponse
	decodeJSON(t, resp, &headInfo)

	if len(headInfo.Leafs) != 1 {
		t.Fatalf("leafs count = %d, want 1", len(headInfo.Leafs))
	}
	if headInfo.Leafs[0].QueuedWorkUnits != 5 {
		t.Errorf("queued_work_units = %d, want 5", headInfo.Leafs[0].QueuedWorkUnits)
	}
}

func TestHeadInfo_NilDefaultWeights(t *testing.T) {
	pool, poolCleanup := setupTestDB(t)
	defer poolCleanup()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	headCfg := &config.HeadConfig{Name: "nil-weights-head", DefaultLeafWeights: nil}

	handler := NewHeadHandler(headCfg, pool, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/head", handler.HandleGetHeadInfo)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/head", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var raw map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()

	// default_leaf_weights should be an empty object, not null.
	weights := string(raw["default_leaf_weights"])
	if weights == "null" {
		t.Error("default_leaf_weights serialized as null, want {}")
	}
}

// createLeafForHeadTest creates a leaf and optionally activates it.
func createLeafForHeadTest(t *testing.T, ts *httptest.Server, userID *types.ID, name string, vis LeafVisibility, activate bool) types.ID {
	t.Helper()

	req := CreateLeafRequest{
		Name:         name,
		Description:  "Head handler test leaf: " + name,
		ResearchArea: []string{"testing"},
		TaskPattern:  PatternParameterSweep,
		Visibility:   vis,
		CreatorID:    userID,
	}
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: expected 201, got %d", name, resp.StatusCode)
	}
	var lf Leaf
	decodeJSON(t, resp, &lf)

	if !activate {
		return lf.ID
	}

	// Configure.
	resp = doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+lf.ID.String()+"/configure", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure %s: expected 200, got %d", name, resp.StatusCode)
	}
	decodeJSON(t, resp, &lf)

	// Update with full config.
	update := fullConfigUpdate()
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+lf.ID.String(), update)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update %s: expected 200, got %d", name, resp.StatusCode)
	}
	decodeJSON(t, resp, &lf)

	// Activate.
	resp = doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+lf.ID.String()+"/activate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activate %s: expected 200, got %d", name, resp.StatusCode)
	}
	decodeJSON(t, resp, &lf)

	return lf.ID
}
