//go:build integration

package internal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// adaptGenerate wraps paramsweep.Generate as a workunit.GenerateFunc.
var adaptGenerate workunit.GenerateFunc = paramsweep.Generate

func setupE2EServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, func()) {
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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	leafRepo := leaf.NewPgxRepository(pool)
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, adaptGenerate, logger)

	statsEngine := stats.NewEngine(pool)
	statsHandler := stats.NewStatsHandler(statsEngine, leafRepo, logger)

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)
	// Mutating routes: production registers these behind auth middleware; the e2e
	// harness drives them directly (unauthenticated) to exercise the full lifecycle.
	mux.HandleFunc("POST /api/v1/leafs", leafHandler.HandleCreate)
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}", leafHandler.HandleDelete)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/pause", leafHandler.HandlePause)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/resume", leafHandler.HandleResume)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/archive", leafHandler.HandleArchive)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", wuHandler.HandleGenerate)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units", wuHandler.HandleList)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", wuHandler.HandleGet)

	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}
	return ts, pool, cleanup
}

func e2eRequest(t *testing.T, method, url string, body any) *http.Response {
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

func e2eRequireStatus(t *testing.T, resp *http.Response, want int, step string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("[%s] expected %d, got %d: %s", step, want, resp.StatusCode, body)
	}
}

func e2eDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestE2EV02Lifecycle covers the full v0.2 lifecycle from project creation
// through work unit generation, stats verification, and archival.
func TestE2EV02Lifecycle(t *testing.T) {
	ts, pool, cleanup := setupE2EServer(t)
	defer cleanup()

	// Create a test user for FK.
	userID := types.NewID()
	_, err := pool.Exec(t.Context(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-%s", uuid.New().String()[:8]),
		"E2E Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	// --- Step 1: POST /api/v1/projects → 201 ---
	createReq := leaf.CreateLeafRequest{
		Name:         "E2E V02 Lifecycle Project",
		Description:  "End-to-end test covering the full v0.2 lifecycle",
		ResearchArea: []string{"physics", "ml-ai"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := e2eRequest(t, "POST", ts.URL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "1: create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)

	if proj.State != leaf.StateDraft {
		t.Fatalf("step 1: state = %q, want DRAFT", proj.State)
	}

	leafURL := ts.URL + "/api/v1/leafs/" + proj.ID.String()

	// --- Step 2: POST .../configure → 200 ---
	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "2: configure")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateConfiguring {
		t.Fatalf("step 2: state = %q, want CONFIGURING", proj.State)
	}

	// --- Step 3: PUT .../projects/{id} with full configs → 200 ---
	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY",
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   86400,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds:  300,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
		SplittingConfig: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2), float64(3)},
			"y": []interface{}{float64(10), float64(20)},
		},
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	}
	resp = e2eRequest(t, "PUT", leafURL, updateReq)
	e2eRequireStatus(t, resp, http.StatusOK, "3: update configs")
	e2eDecode(t, resp, &proj)

	// --- Step 4: POST .../activate → 200 ---
	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "4: activate")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateActive {
		t.Fatalf("step 4: state = %q, want ACTIVE", proj.State)
	}

	// --- Step 5: POST .../work-units/generate → 202 ---
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2), float64(3)},
			"y": []interface{}{float64(10), float64(20)},
		},
	}
	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", genReq)
	e2eRequireStatus(t, resp, http.StatusAccepted, "5: generate work units")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 6 {
		t.Fatalf("step 5: work_units_created = %d, want 6 (3x2)", genResp.WorkUnitsCreated)
	}

	// --- Step 6: GET .../work-units → 200 (verify 6 work units in QUEUED state) ---
	resp = e2eRequest(t, "GET", leafURL+"/work-units", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "6: list work units")
	var listResp types.ListResponse[workunit.WorkUnitSummary]
	e2eDecode(t, resp, &listResp)
	if len(listResp.Data) != 6 {
		t.Fatalf("step 6: expected 6 work units, got %d", len(listResp.Data))
	}
	for _, wu := range listResp.Data {
		if wu.State != workunit.WorkUnitStateQueued {
			t.Errorf("step 6: work unit %v state = %q, want QUEUED", wu.ID, wu.State)
		}
	}

	// --- Step 7: GET .../work-units?state=QUEUED → 200 (verify 6) ---
	resp = e2eRequest(t, "GET", leafURL+"/work-units?state=QUEUED", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "7: list queued work units")
	e2eDecode(t, resp, &listResp)
	if len(listResp.Data) != 6 {
		t.Fatalf("step 7: expected 6 QUEUED work units, got %d", len(listResp.Data))
	}

	// --- Step 8: GET .../work-units/{id} → 200 (verify parameters) ---
	wuID := listResp.Data[0].ID
	resp = e2eRequest(t, "GET", fmt.Sprintf("%s/work-units/%s", leafURL, wuID), nil)
	e2eRequireStatus(t, resp, http.StatusOK, "8: get work unit")
	var wu workunit.WorkUnit
	e2eDecode(t, resp, &wu)
	if wu.Parameters == nil {
		t.Fatal("step 8: parameters should be set")
	}

	// --- Step 9: GET .../stats → 200 (verify total=6, queued=6) ---
	resp = e2eRequest(t, "GET", leafURL+"/stats", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "9: get stats")
	var snap stats.LeafStatsSnapshot
	e2eDecode(t, resp, &snap)
	if snap.TotalWorkUnits != 6 {
		t.Errorf("step 9: total_work_units = %d, want 6", snap.TotalWorkUnits)
	}
	if snap.WorkUnitsQueued != 6 {
		t.Errorf("step 9: work_units_queued = %d, want 6", snap.WorkUnitsQueued)
	}
	if snap.WorkUnitsAssigned != 0 {
		t.Errorf("step 9: work_units_assigned = %d, want 0", snap.WorkUnitsAssigned)
	}
	if snap.WorkUnitsRunning != 0 {
		t.Errorf("step 9: work_units_running = %d, want 0", snap.WorkUnitsRunning)
	}
	if snap.WorkUnitsCompleted != 0 {
		t.Errorf("step 9: work_units_completed = %d, want 0", snap.WorkUnitsCompleted)
	}
	if snap.WorkUnitsValidated != 0 {
		t.Errorf("step 9: work_units_validated = %d, want 0", snap.WorkUnitsValidated)
	}
	if snap.WorkUnitsFailed != 0 {
		t.Errorf("step 9: work_units_failed = %d, want 0", snap.WorkUnitsFailed)
	}
	if snap.ActiveVolunteers != 0 {
		t.Errorf("step 9: active_volunteers = %d, want 0", snap.ActiveVolunteers)
	}

	// --- Step 10: POST .../pause → 200 ---
	resp = e2eRequest(t, "POST", leafURL+"/pause", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "10: pause")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StatePaused {
		t.Fatalf("step 10: state = %q, want PAUSED", proj.State)
	}

	// --- Step 11: POST .../archive → 200 ---
	resp = e2eRequest(t, "POST", leafURL+"/archive", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "11: archive")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateArchived {
		t.Fatalf("step 11: state = %q, want ARCHIVED", proj.State)
	}

	// --- Step 12: Verify project is ARCHIVED, work units still queryable ---
	resp = e2eRequest(t, "GET", leafURL, nil)
	e2eRequireStatus(t, resp, http.StatusOK, "12a: get archived leaf")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateArchived {
		t.Errorf("step 12a: state = %q, want ARCHIVED", proj.State)
	}

	resp = e2eRequest(t, "GET", leafURL+"/work-units", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "12b: list work units after archive")
	e2eDecode(t, resp, &listResp)
	if len(listResp.Data) != 6 {
		t.Errorf("step 12b: expected 6 work units after archive, got %d", len(listResp.Data))
	}
}
