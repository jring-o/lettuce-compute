//go:build integration

package leaf

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
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setupHandlerServer creates an httptest.Server with the LeafHandler wired up.
// Returns the server, the pool (for test data setup), and a cleanup function.
func setupHandlerServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, func()) {
	t.Helper()

	pool, poolCleanup := setupTestDB(t)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	repo := NewPgxRepository(pool)
	handler := NewLeafHandler(repo, pool, logger)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	// Protected routes are now registered by the router with auth wrappers.
	// In integration tests, register them directly without auth.
	mux.HandleFunc("POST /api/v1/leafs", handler.HandleCreate)
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", handler.HandleUpdate)
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}", handler.HandleDelete)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", handler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/pause", handler.HandlePause)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/resume", handler.HandleResume)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/archive", handler.HandleArchive)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", handler.HandleConfigure)

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

func validCreateRequest(creatorID *types.ID) CreateLeafRequest {
	return CreateLeafRequest{
		Name:         "Test Handler Leaf",
		Description:  "A test leaf for handler integration testing purposes",
		ResearchArea: []string{"physics", "ml-ai"},
		TaskPattern:  PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   VisibilityPublic,
		CreatorID:    creatorID,
	}
}

// --- Create Tests ---

func TestHandlerCreate(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcreate1")

	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}

	var p Leaf
	decodeJSON(t, resp, &p)

	if types.IsNilID(p.ID) {
		t.Error("ID should be set")
	}
	if p.Slug == "" {
		t.Error("slug should be auto-generated")
	}
	if p.State != StateDraft {
		t.Errorf("state = %q, want DRAFT", p.State)
	}
	if p.Name != req.Name {
		t.Errorf("name = %q, want %q", p.Name, req.Name)
	}
}

func TestHandlerCreateInvalidName(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcreate2")

	req := validCreateRequest(&userID)
	req.Name = "ab" // too short
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerCreateInvalidTaskPattern(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcreate3")

	req := validCreateRequest(&userID)
	req.TaskPattern = "INVALID_PATTERN"
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerCreateInvalidBody(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/leafs", bytes.NewReader([]byte("not json")))
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

func TestHandlerCreateDefaultVisibility(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcreate4")

	req := validCreateRequest(&userID)
	req.Visibility = "" // should default to PUBLIC
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}

	var p Leaf
	decodeJSON(t, resp, &p)
	if p.Visibility != VisibilityPublic {
		t.Errorf("visibility = %q, want PUBLIC", p.Visibility)
	}
}

// --- Get Tests ---

func TestHandlerGet(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hget1")

	// Create a leaf first.
	req := validCreateRequest(&userID)
	createResp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, createResp, &created)

	// Get it.
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+created.ID.String(), nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var got Leaf
	decodeJSON(t, resp, &got)
	if got.ID != created.ID {
		t.Errorf("ID = %v, want %v", got.ID, created.ID)
	}
	if got.Name != created.Name {
		t.Errorf("Name = %q, want %q", got.Name, created.Name)
	}
}

func TestHandlerGetNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+fakeID.String(), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerGetInvalidID(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	// Non-UUID strings are now treated as slugs (Bug 3 fix).
	// A nonexistent slug returns 404 (not 400).
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/not-a-uuid", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 (nonexistent slug), got %d", resp.StatusCode)
	}
}

func TestHandlerGetBySlug(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hgetslug1")

	// Create a leaf first.
	req := validCreateRequest(&userID)
	createResp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, createResp, &created)

	if created.Slug == "" {
		t.Fatal("created leaf should have a slug")
	}

	// Get by slug.
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+created.Slug, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var got Leaf
	decodeJSON(t, resp, &got)
	if got.ID != created.ID {
		t.Errorf("ID = %v, want %v", got.ID, created.ID)
	}
	if got.Name != created.Name {
		t.Errorf("Name = %q, want %q", got.Name, created.Name)
	}
	if got.Slug != created.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, created.Slug)
	}
}

func TestHandlerGetBySlugNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/nonexistent-slug-xyz", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- List Tests ---

func TestHandlerList(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hlist1")

	// Create 3 public leafs.
	for i := 0; i < 3; i++ {
		req := validCreateRequest(&userID)
		req.Name = fmt.Sprintf("List Test Leaf %d", i)
		resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
		resp.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}

	// List with no filters — should return PUBLIC leafs.
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[LeafSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) < 3 {
		t.Errorf("expected at least 3 leafs, got %d", len(listResp.Data))
	}

	// Verify summaries have expected v0.2 defaults.
	for _, s := range listResp.Data {
		if s.ID == types.NilID() {
			t.Error("summary ID should not be nil")
		}
		if s.Name == "" {
			t.Error("summary name should not be empty")
		}
		if s.ActiveVolunteers != 0 {
			t.Errorf("active_volunteers = %d, want 0 for v0.2", s.ActiveVolunteers)
		}
		if s.ProgressPct != nil {
			t.Errorf("progress_pct should be nil for v0.2, got %v", *s.ProgressPct)
		}
	}
}

func TestHandlerListWithStateFilter(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hlist2")

	// Create a leaf (starts as DRAFT).
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	resp.Body.Close()

	// List with state=DRAFT and creator_id (to see all states).
	url := fmt.Sprintf("%s/api/v1/leafs?state=DRAFT&creator_id=%s", ts.URL, userID.String())
	resp = doRequest(t, "GET", url, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[LeafSummary]
	decodeJSON(t, resp, &listResp)

	for _, s := range listResp.Data {
		if s.State != StateDraft {
			t.Errorf("expected DRAFT, got %q", s.State)
		}
	}
}

func TestHandlerListWithSearch(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hlist3")

	req := validCreateRequest(&userID)
	req.Name = "Unique Quantum Handler Search Test"
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Search for it.
	url := fmt.Sprintf("%s/api/v1/leafs?search=Unique+Quantum+Handler&creator_id=%s", ts.URL, userID.String())
	resp = doRequest(t, "GET", url, nil)

	var listResp types.ListResponse[LeafSummary]
	decodeJSON(t, resp, &listResp)

	found := false
	for _, s := range listResp.Data {
		if s.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Error("search did not find the leaf")
	}
}

func TestHandlerListPagination(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hlist4")

	// Create 5 leafs.
	for i := 0; i < 5; i++ {
		req := validCreateRequest(&userID)
		req.Name = fmt.Sprintf("Paginated Leaf %d", i)
		resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
		resp.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}

	// Page 1: limit=3.
	url := fmt.Sprintf("%s/api/v1/leafs?limit=3&creator_id=%s", ts.URL, userID.String())
	resp := doRequest(t, "GET", url, nil)

	var page1 types.ListResponse[LeafSummary]
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
	url = fmt.Sprintf("%s/api/v1/leafs?limit=3&creator_id=%s&cursor=%s",
		ts.URL, userID.String(), page1.Pagination.NextCursor)
	resp = doRequest(t, "GET", url, nil)

	var page2 types.ListResponse[LeafSummary]
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
			t.Errorf("duplicate leaf %v across pages", s.ID)
		}
	}
}

func TestHandlerListSortOrder(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hlist5")

	names := []string{"Zeta", "Alpha", "Mu"}
	for _, name := range names {
		req := validCreateRequest(&userID)
		req.Name = name + " Sort Leaf"
		resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
		resp.Body.Close()
	}

	url := fmt.Sprintf("%s/api/v1/leafs?sort=name&order=asc&creator_id=%s", ts.URL, userID.String())
	resp := doRequest(t, "GET", url, nil)

	var listResp types.ListResponse[LeafSummary]
	decodeJSON(t, resp, &listResp)

	if len(listResp.Data) < 3 {
		t.Fatalf("expected at least 3 leafs, got %d", len(listResp.Data))
	}
	if listResp.Data[0].Name > listResp.Data[1].Name {
		t.Errorf("not ascending: %q > %q", listResp.Data[0].Name, listResp.Data[1].Name)
	}
}

func TestHandlerListInvalidSort(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs?sort=invalid", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerListInvalidLimit(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs?limit=999", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// --- Update Tests ---

func TestHandlerUpdate(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hupd1")

	// Create.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Update name only.
	newName := "Updated Handler Leaf Name"
	updateReq := UpdateLeafRequest{
		Name: &newName,
	}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var updated Leaf
	decodeJSON(t, resp, &updated)
	if updated.Name != newName {
		t.Errorf("name = %q, want %q", updated.Name, newName)
	}
	// Other fields should be unchanged.
	if updated.Description != created.Description {
		t.Errorf("description changed unexpectedly")
	}
	if updated.Slug != created.Slug {
		t.Errorf("slug changed unexpectedly")
	}
}

func TestHandlerUpdateConfig(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hupd2")

	// Create.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Update execution config.
	execCfg := ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY",
		MaxMemoryMB:     8192,
		MaxDiskMB:       20480,
		MaxCPUSeconds:   172800,
	}
	updateReq := UpdateLeafRequest{
		ExecutionConfig: &execCfg,
	}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var updated Leaf
	decodeJSON(t, resp, &updated)
	if updated.ExecutionConfig.MaxMemoryMB != 8192 {
		t.Errorf("MaxMemoryMB = %d, want 8192", updated.ExecutionConfig.MaxMemoryMB)
	}
}

func TestHandlerUpdateNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	newName := "Nonexistent"
	updateReq := UpdateLeafRequest{Name: &newName}
	resp := doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+fakeID.String(), updateReq)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerUpdateImmutableWhileActive(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hupd3")

	// Create leaf via API.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Force state to ACTIVE in DB.
	_, err := pool.Exec(t.Context(), "UPDATE leafs SET state = 'ACTIVE' WHERE id = $1", created.ID)
	if err != nil {
		t.Fatalf("failed to set ACTIVE state: %v", err)
	}

	// Try to update execution_config.runtime while ACTIVE — should get 409.
	execCfg := ExecutionConfig{
		Runtime:       "CONTAINER", // different from zero-value empty string
		GPUType:       "ANY",
		MaxMemoryMB:   4096,
		MaxDiskMB:     10240,
		MaxCPUSeconds: 86400,
	}
	updateReq := UpdateLeafRequest{
		ExecutionConfig: &execCfg,
	}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Delete Tests ---

func TestHandlerDelete(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hdel1")

	// Create leaf (DRAFT state).
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Delete it.
	resp = doRequest(t, "DELETE", ts.URL+"/api/v1/leafs/"+created.ID.String(), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, body)
	}

	// Verify it's gone.
	resp2 := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+created.ID.String(), nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp2.StatusCode)
	}
}

func TestHandlerDeleteNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	resp := doRequest(t, "DELETE", ts.URL+"/api/v1/leafs/"+fakeID.String(), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDeleteActiveLeaf(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hdel2")

	// Create leaf.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Force state to ACTIVE.
	_, err := pool.Exec(t.Context(), "UPDATE leafs SET state = 'ACTIVE' WHERE id = $1", created.ID)
	if err != nil {
		t.Fatalf("failed to set ACTIVE state: %v", err)
	}

	// Delete should fail with 409.
	resp = doRequest(t, "DELETE", ts.URL+"/api/v1/leafs/"+created.ID.String(), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Error Response Shape ---

func TestHandlerErrorShape(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+fakeID.String(), nil)

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

// --- State Transition Tests ---

// createLeafInState creates a leaf via the API, sets its configs via PUT,
// and transitions it to the desired state by manipulating the DB directly.
// For CONFIGURING, it uses the /configure endpoint. For ACTIVE, it uses /configure + /activate.
func createLeafInState(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, state LeafState) Leaf {
	t.Helper()

	userID := createTestUser(t, pool, fmt.Sprintf("st-%s", time.Now().Format("150405000")))

	// Create via API (starts as DRAFT).
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	requireStatus(t, resp, http.StatusCreated, "createLeafInState: create")
	var p Leaf
	decodeJSON(t, resp, &p)

	if state == StateDraft {
		return p
	}

	leafURL := ts.URL + "/api/v1/leafs/" + p.ID.String()

	// Set all configs via PUT so activation can succeed.
	updateReq := fullConfigUpdate()
	resp = doRequest(t, "PUT", leafURL, updateReq)
	requireStatus(t, resp, http.StatusOK, "createLeafInState: update configs")
	decodeJSON(t, resp, &p)

	if state == StateConfiguring {
		resp = doRequest(t, "POST", leafURL+"/configure", nil)
		requireStatus(t, resp, http.StatusOK, "createLeafInState: configure")
		decodeJSON(t, resp, &p)
		return p
	}

	// For ACTIVE, PAUSED, COMPLETED, ARCHIVED: transition through CONFIGURING → ACTIVE.
	resp = doRequest(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "createLeafInState: configure")
	decodeJSON(t, resp, &p)
	resp = doRequest(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "createLeafInState: activate")
	decodeJSON(t, resp, &p)

	if state == StateActive {
		return p
	}

	if state == StatePaused {
		resp = doRequest(t, "POST", leafURL+"/pause", nil)
		requireStatus(t, resp, http.StatusOK, "createLeafInState: pause")
		decodeJSON(t, resp, &p)
		return p
	}

	if state == StateCompleted {
		_, err := pool.Exec(t.Context(), "UPDATE leafs SET state = 'COMPLETED' WHERE id = $1", p.ID)
		if err != nil {
			t.Fatalf("failed to set COMPLETED state: %v", err)
		}
		p.State = StateCompleted
		return p
	}

	if state == StateArchived {
		resp = doRequest(t, "POST", leafURL+"/pause", nil)
		requireStatus(t, resp, http.StatusOK, "createLeafInState: pause")
		decodeJSON(t, resp, &p)
		resp = doRequest(t, "POST", leafURL+"/archive", nil)
		requireStatus(t, resp, http.StatusOK, "createLeafInState: archive")
		decodeJSON(t, resp, &p)
		return p
	}

	t.Fatalf("unsupported state: %s", state)
	return p
}

func fullConfigUpdate() UpdateLeafRequest {
	execCfg := ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY",
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   86400,
	}
	valCfg := ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	ftCfg := FaultToleranceConfig{
		HeartbeatIntervalSeconds:  300,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
	}
	dataCfg := DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
	return UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	}
}

// --- Activate Tests ---

func TestHandlerActivateSuccess(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateConfiguring)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/activate", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var activated Leaf
	decodeJSON(t, resp, &activated)
	if activated.State != StateActive {
		t.Errorf("state = %q, want ACTIVE", activated.State)
	}
}

func TestHandlerActivateIncompleteConfig(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "act-inc")

	// Create leaf and move to CONFIGURING without setting configs.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var p Leaf
	decodeJSON(t, resp, &p)

	resp = doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/configure", nil)
	decodeJSON(t, resp, &p)

	// Try to activate — should fail with 400 CONFIGURATION_INCOMPLETE.
	resp = doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/activate", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&errResp)
	if errResp.Error.Code != "CONFIGURATION_INCOMPLETE" {
		t.Errorf("error.code = %q, want CONFIGURATION_INCOMPLETE", errResp.Error.Code)
	}
}

func TestHandlerActivateWrongState(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateDraft)

	// DRAFT → ACTIVE is not a valid transition.
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/activate", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandlerActivateNotFound(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	fakeID := types.NewID()
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+fakeID.String()+"/activate", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Pause Tests ---

func TestHandlerPauseSuccess(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateActive)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/pause", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var paused Leaf
	decodeJSON(t, resp, &paused)
	if paused.State != StatePaused {
		t.Errorf("state = %q, want PAUSED", paused.State)
	}
}

func TestHandlerPauseWrongState(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StatePaused)

	// PAUSED → PAUSED is not valid.
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/pause", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Resume Tests ---

func TestHandlerResumeSuccess(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StatePaused)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/resume", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var resumed Leaf
	decodeJSON(t, resp, &resumed)
	if resumed.State != StateActive {
		t.Errorf("state = %q, want ACTIVE", resumed.State)
	}
}

func TestHandlerResumeWrongState(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateActive)

	// ACTIVE → ACTIVE (resume) is not valid.
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/resume", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Archive Tests ---

func TestHandlerArchiveFromPaused(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StatePaused)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/archive", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var archived Leaf
	decodeJSON(t, resp, &archived)
	if archived.State != StateArchived {
		t.Errorf("state = %q, want ARCHIVED", archived.State)
	}
}

func TestHandlerArchiveFromDraft(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateDraft)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/archive", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var archived Leaf
	decodeJSON(t, resp, &archived)
	if archived.State != StateArchived {
		t.Errorf("state = %q, want ARCHIVED", archived.State)
	}
}

func TestHandlerArchiveFromActive(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateActive)

	// ACTIVE → ARCHIVED is not valid (must pause first).
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/archive", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Configure Tests ---

func TestHandlerConfigureFromDraft(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateDraft)

	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/configure", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var configured Leaf
	decodeJSON(t, resp, &configured)
	if configured.State != StateConfiguring {
		t.Errorf("state = %q, want CONFIGURING", configured.State)
	}
}

func TestHandlerConfigureFromActive(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateActive)

	// ACTIVE → CONFIGURING is valid (implicit pause).
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/configure", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var configured Leaf
	decodeJSON(t, resp, &configured)
	if configured.State != StateConfiguring {
		t.Errorf("state = %q, want CONFIGURING", configured.State)
	}
}

func TestHandlerConfigureFromArchived(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateArchived)

	// ARCHIVED → CONFIGURING is not valid.
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/"+p.ID.String()+"/configure", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

// --- Stats Cache Seconds Update Tests ---

func TestHandlerUpdateStatsCacheSeconds(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcache1")

	// Create leaf.
	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Default should be 0 (uses server default).
	if created.StatsCacheSeconds != 0 {
		t.Errorf("default stats_cache_seconds = %d, want 0", created.StatsCacheSeconds)
	}

	// Update to a valid value.
	cacheSeconds := 300
	updateReq := UpdateLeafRequest{StatsCacheSeconds: &cacheSeconds}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var updated Leaf
	decodeJSON(t, resp, &updated)
	if updated.StatsCacheSeconds != 300 {
		t.Errorf("stats_cache_seconds = %d, want 300", updated.StatsCacheSeconds)
	}
}

func TestHandlerUpdateStatsCacheSecondsTooLow(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcache2")

	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Try to set below minimum (5).
	tooLow := 2
	updateReq := UpdateLeafRequest{StatsCacheSeconds: &tooLow}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for stats_cache_seconds=2, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandlerUpdateStatsCacheSecondsTooHigh(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "hcache3")

	req := validCreateRequest(&userID)
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs", req)
	var created Leaf
	decodeJSON(t, resp, &created)

	// Try to set above maximum (3600).
	tooHigh := 7200
	updateReq := UpdateLeafRequest{StatsCacheSeconds: &tooHigh}
	resp = doRequest(t, "PUT", ts.URL+"/api/v1/leafs/"+created.ID.String(), updateReq)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for stats_cache_seconds=7200, got %d: %s", resp.StatusCode, body)
	}
}

// --- Coverage gap tests ---

func TestHandlerTransitionInvalidID(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	// Invalid UUID on a transition endpoint (covers shared handleTransition path).
	resp := doRequest(t, "POST", ts.URL+"/api/v1/leafs/not-a-uuid/activate", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerUpdateInvalidID(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	newName := "Updated"
	updateReq := UpdateLeafRequest{Name: &newName}
	resp := doRequest(t, "PUT", ts.URL+"/api/v1/leafs/not-a-uuid", updateReq)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerDeleteInvalidID(t *testing.T) {
	ts, _, cleanup := setupHandlerServer(t)
	defer cleanup()

	resp := doRequest(t, "DELETE", ts.URL+"/api/v1/leafs/not-a-uuid", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerDeleteWithCreditHistory(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	p := createLeafInState(t, ts, pool, StateDraft)

	// Create a volunteer so we can create credit history.
	volID := types.NewID()
	pubKey := []byte(fmt.Sprintf("handler-test-pubkey-%s!!", volID.String()[:10]))
	_, err := pool.Exec(t.Context(), `
		INSERT INTO volunteers (id, public_key, display_name)
		VALUES ($1, $2, $3)`,
		volID, pubKey, "Test Volunteer",
	)
	if err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// Create a work unit.
	wuID := types.NewID()
	_, err = pool.Exec(t.Context(), `
		INSERT INTO work_units (id, leaf_id, state, priority, code_artifact_ref, deadline_seconds)
		VALUES ($1, $2, 'COMPLETED', 'NORMAL', 'ref://test', 3600)`,
		wuID, p.ID,
	)
	if err != nil {
		t.Fatalf("create work unit: %v", err)
	}

	// Create a result.
	resultID := types.NewID()
	_, err = pool.Exec(t.Context(), `
		INSERT INTO results (id, work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status, submitted_at)
		VALUES ($1, $2, $3, '{"result": 42}', 'sha256:abc123', '{}', 'PENDING', NOW())`,
		resultID, wuID, volID,
	)
	if err != nil {
		t.Fatalf("create result: %v", err)
	}

	// Create credit ledger entry.
	_, err = pool.Exec(t.Context(), `
		INSERT INTO credit_ledger (id, volunteer_id, leaf_id, work_unit_id, result_id, credit_amount, granted_at)
		VALUES ($1, $2, $3, $4, $5, 1.0, NOW())`,
		types.NewID(), volID, p.ID, wuID, resultID,
	)
	if err != nil {
		t.Fatalf("create credit ledger entry: %v", err)
	}

	// Delete should fail with 409 — leaf has credit history.
	resp := doRequest(t, "DELETE", ts.URL+"/api/v1/leafs/"+p.ID.String(), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}
