//go:build integration

package workunit_test

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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// adaptGenerate wraps paramsweep.Generate as a workunit.GenerateFunc.
var adaptGenerate workunit.GenerateFunc = paramsweep.Generate

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
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
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

// setupHandlerServer creates an httptest.Server with both project and work unit
// handlers wired up, mirroring the real router setup.
func setupHandlerServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, func()) {
	t.Helper()

	pool, poolCleanup := setupTestDB(t)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	leafRepo := leaf.NewPgxRepository(pool)
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, adaptGenerate, logger)

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	// Leaf management routes (no auth in tests).
	mux.HandleFunc("POST /api/v1/leafs", leafHandler.HandleCreate)
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	// Work unit routes (no auth in tests).
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units", wuHandler.HandleList)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", wuHandler.HandleGet)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", wuHandler.HandleGenerate)

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

// createProjectInState creates a fully configured project via the API and
// transitions it to the desired state. Supports DRAFT, CONFIGURING, ACTIVE.
func createProjectInState(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, state leaf.LeafState) leaf.Leaf {
	t.Helper()

	userID := createHandlerTestUser(t, pool, fmt.Sprintf("wuhs-%s-%s", time.Now().Format("150405000"), uuid.New().String()[:6]))

	createReq := leaf.CreateLeafRequest{
		Name:         "WU Handler Test Project",
		Description:  "A test leaf for work unit handler integration tests",
		ResearchArea: []string{"physics"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var p leaf.Leaf
	decodeJSON(t, resp, &p)

	if state == leaf.StateDraft {
		return p
	}

	leafURL := ts.URL + "/api/v1/leafs/" + p.ID.String()

	// Set all 4 configs.
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
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	}
	resp = doRequest(t, "PUT", leafURL, updateReq)
	requireStatus(t, resp, http.StatusOK, "update configs")
	decodeJSON(t, resp, &p)

	// Transition to CONFIGURING.
	resp = doRequest(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	decodeJSON(t, resp, &p)

	if state == leaf.StateConfiguring {
		return p
	}

	// Transition to ACTIVE.
	resp = doRequest(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	decodeJSON(t, resp, &p)

	return p
}

// createHandlerTestUser inserts a minimal user for FK references.
func createHandlerTestUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(t.Context(), `
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

// generateWorkUnits calls the generate endpoint with the given parameter space.
func generateWorkUnits(t *testing.T, ts *httptest.Server, leafID types.ID, paramSpace map[string]interface{}) workunit.GenerateResponse {
	t.Helper()
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, leafID)
	resp := doRequest(t, "POST", url, req)
	requireStatus(t, resp, http.StatusAccepted, "generate work units")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	return genResp
}

// --- Generate Tests ---

func TestHandlerGenerateSuccess(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200), float64(300)},
		"pressure":    map[string]interface{}{"min": 1.0, "max": 3.0, "step": 1.0},
	}
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	requireStatus(t, resp, http.StatusAccepted, "generate")

	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)

	// 3 temperatures x 3 pressures = 9 work units.
	if genResp.WorkUnitsCreated != 9 {
		t.Errorf("work_units_created = %d, want 9", genResp.WorkUnitsCreated)
	}
	if genResp.Status != "complete" {
		t.Errorf("status = %q, want complete", genResp.Status)
	}
	if len(genResp.BatchIDs) == 0 {
		t.Error("batch_ids should not be empty")
	}
	if types.IsNilID(genResp.BatchIDs[0]) {
		t.Error("first batch_id should be set")
	}
}

func TestHandlerGenerateActiveProject(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateActive)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	requireStatus(t, resp, http.StatusAccepted, "generate from active")

	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 2 {
		t.Errorf("work_units_created = %d, want 2", genResp.WorkUnitsCreated)
	}
}

func TestHandlerGenerateMissingParams(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	// No parameter_space and project has no splitting_config → 400.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, workunit.GenerateRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandlerGenerateWrongState(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateDraft)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0},
	}
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandlerGenerateProjectNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	req := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{1.0},
		},
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, fakeID)
	resp := doRequest(t, "POST", url, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerGenerateListRangeMix(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200)},
		"pressure":    map[string]interface{}{"min": 1.0, "max": 5.0, "step": 2.0},
	}
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	requireStatus(t, resp, http.StatusAccepted, "generate range+list mix")

	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)

	// 2 temperatures x 3 pressures (1,3,5) = 6 work units.
	if genResp.WorkUnitsCreated != 6 {
		t.Errorf("work_units_created = %d, want 6", genResp.WorkUnitsCreated)
	}
}

func TestHandlerGenerateWithBatchSize(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0, 3.0, 4.0, 5.0},
	}
	req := workunit.GenerateRequest{
		BatchSize:      2,
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	requireStatus(t, resp, http.StatusAccepted, "generate with batch size")

	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)

	if genResp.WorkUnitsCreated != 5 {
		t.Errorf("work_units_created = %d, want 5", genResp.WorkUnitsCreated)
	}
}

// --- List Tests ---

func TestHandlerListWorkUnits(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	// Generate work units.
	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0, 3.0, 4.0, 5.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// List all.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list work units")

	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) != 5 {
		t.Fatalf("expected 5 work units, got %d", len(listResp.Data))
	}

	// Verify summary fields.
	for _, s := range listResp.Data {
		if types.IsNilID(s.ID) {
			t.Error("summary ID should not be nil")
		}
		if s.State != workunit.WorkUnitStateQueued {
			t.Errorf("state = %q, want QUEUED", s.State)
		}
		if s.Priority != workunit.WorkUnitPriorityNormal {
			t.Errorf("priority = %q, want NORMAL", s.Priority)
		}
	}
}

func TestHandlerListWorkUnitsFilterByState(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0, 3.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// All generated work units are QUEUED. Filter by QUEUED should return all.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units?state=QUEUED", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by state")

	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) != 3 {
		t.Fatalf("expected 3 QUEUED work units, got %d", len(listResp.Data))
	}

	// Filter by COMPLETED should return none.
	url = fmt.Sprintf("%s/api/v1/leafs/%s/work-units?state=COMPLETED", ts.URL, p.ID)
	resp = doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by COMPLETED")

	decodeJSON(t, resp, &listResp)
	if len(listResp.Data) != 0 {
		t.Errorf("expected 0 COMPLETED work units, got %d", len(listResp.Data))
	}
}

func TestHandlerListWorkUnitsFilterByBatchID(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}
	genResp := generateWorkUnits(t, ts, p.ID, paramSpace)

	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units?batch_id=%s",
		ts.URL, p.ID, genResp.BatchIDs[0])
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by batch_id")

	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) != 2 {
		t.Fatalf("expected 2 work units in batch, got %d", len(listResp.Data))
	}
}

func TestHandlerListWorkUnitsFilterByPriority(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// All generated work units are NORMAL priority.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units?priority=NORMAL", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by priority")

	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) != 2 {
		t.Fatalf("expected 2 NORMAL work units, got %d", len(listResp.Data))
	}

	// HIGH priority should return none.
	url = fmt.Sprintf("%s/api/v1/leafs/%s/work-units?priority=HIGH", ts.URL, p.ID)
	resp = doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by HIGH")

	decodeJSON(t, resp, &listResp)
	if len(listResp.Data) != 0 {
		t.Errorf("expected 0 HIGH work units, got %d", len(listResp.Data))
	}
}

func TestHandlerListWorkUnitsFilterByFlagged(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0, 3.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// Flag one work unit directly in DB.
	_, err := pool.Exec(t.Context(), `
		UPDATE work_units SET flagged_for_review = true
		WHERE leaf_id = $1
		AND id = (SELECT id FROM work_units WHERE leaf_id = $1 LIMIT 1)`, p.ID)
	if err != nil {
		t.Fatalf("flag work unit: %v", err)
	}

	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units?flagged_for_review=true", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list by flagged")

	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) != 1 {
		t.Fatalf("expected 1 flagged work unit, got %d", len(listResp.Data))
	}
	if !listResp.Data[0].FlaggedForReview {
		t.Error("expected flagged_for_review = true")
	}
}

func TestHandlerListWorkUnitsPagination(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0, 3.0, 4.0, 5.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// Page 1: limit=3.
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units?limit=3", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list page 1")

	var page1 types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &page1)

	if len(page1.Data) != 3 {
		t.Fatalf("page 1: got %d, want 3", len(page1.Data))
	}
	if !page1.Pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}
	if page1.Pagination.NextCursor == "" {
		t.Error("page 1: NextCursor should be set")
	}

	// Page 2.
	url = fmt.Sprintf("%s/api/v1/leafs/%s/work-units?limit=3&cursor=%s",
		ts.URL, p.ID, page1.Pagination.NextCursor)
	resp = doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list page 2")

	var page2 types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &page2)

	if len(page2.Data) != 2 {
		t.Fatalf("page 2: got %d, want 2", len(page2.Data))
	}
	if page2.Pagination.HasMore {
		t.Error("page 2: HasMore should be false")
	}

	// No overlap.
	seen := make(map[types.ID]bool)
	for _, s := range page1.Data {
		seen[s.ID] = true
	}
	for _, s := range page2.Data {
		if seen[s.ID] {
			t.Errorf("duplicate work unit %v across pages", s.ID)
		}
	}
}

func TestHandlerListWorkUnitsProjectNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units", ts.URL, fakeID)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Get Tests ---

func TestHandlerGetWorkUnit(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}
	generateWorkUnits(t, ts, p.ID, paramSpace)

	// List to get a work unit ID.
	listURL := fmt.Sprintf("%s/api/v1/leafs/%s/work-units", ts.URL, p.ID)
	resp := doRequest(t, "GET", listURL, nil)
	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) == 0 {
		t.Fatal("expected at least 1 work unit")
	}
	wuID := listResp.Data[0].ID

	// Get the work unit.
	getURL := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/%s", ts.URL, p.ID, wuID)
	resp = doRequest(t, "GET", getURL, nil)
	requireStatus(t, resp, http.StatusOK, "get work unit")

	var wu workunit.WorkUnit
	decodeJSON(t, resp, &wu)

	if wu.ID != wuID {
		t.Errorf("ID = %v, want %v", wu.ID, wuID)
	}
	if wu.LeafID != p.ID {
		t.Errorf("ProjectID = %v, want %v", wu.LeafID, p.ID)
	}
	if wu.State != workunit.WorkUnitStateQueued {
		t.Errorf("State = %q, want QUEUED", wu.State)
	}
	if wu.Parameters == nil {
		t.Error("Parameters should be set")
	}
	if wu.CodeArtifactRef == "" {
		t.Error("CodeArtifactRef should be set")
	}
}

func TestHandlerGetWorkUnitNotFound(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	fakeWUID := types.NewID()
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/%s", ts.URL, p.ID, fakeWUID)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerGetWorkUnitWrongProject(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p1 := createProjectInState(t, ts, pool, leaf.StateConfiguring)
	p2 := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": []interface{}{1.0},
	}
	generateWorkUnits(t, ts, p1.ID, paramSpace)

	// Get a work unit ID from p1.
	listURL := fmt.Sprintf("%s/api/v1/leafs/%s/work-units", ts.URL, p1.ID)
	resp := doRequest(t, "GET", listURL, nil)
	var listResp types.ListResponse[workunit.WorkUnitSummary]
	decodeJSON(t, resp, &listResp)

	wuID := listResp.Data[0].ID

	// Try to get it via p2 — should return 404.
	getURL := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/%s", ts.URL, p2.ID, wuID)
	resp = doRequest(t, "GET", getURL, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong project, got %d", resp.StatusCode)
	}
}

func TestHandlerGetWorkUnitInvalidID(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/not-a-uuid", ts.URL, p.ID)
	resp := doRequest(t, "GET", url, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// --- Error Response Shape ---

func TestHandlerWorkUnitErrorShape(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	fakeWUID := types.NewID()
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/%s", ts.URL, p.ID, fakeWUID)
	resp := doRequest(t, "GET", url, nil)

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &errResp)

	if errResp.Error.Code != "NOT_FOUND" {
		t.Errorf("error.code = %q, want NOT_FOUND", errResp.Error.Code)
	}
	if errResp.Error.Message == "" {
		t.Error("error.message should not be empty")
	}
}

// --- Edge Cases ---

func TestHandlerGenerateInvalidBody(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerGenerateInvalidParameterSpace(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createProjectInState(t, ts, pool, leaf.StateConfiguring)

	paramSpace := map[string]interface{}{
		"x": "not-a-list-or-range",
	}
	req := workunit.GenerateRequest{
		ParameterSpace: paramSpace,
	}
	url := fmt.Sprintf("%s/api/v1/leafs/%s/work-units/generate", ts.URL, p.ID)
	resp := doRequest(t, "POST", url, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}
