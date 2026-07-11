package result

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// mockResultRepo is a test implementation of result.Repository.
type mockResultRepo struct {
	results    []*Result
	pagination types.PaginationResponse
	err        error

	// capturedFilters stores the filters passed to ListByLeaf for assertion.
	capturedFilters *ResultFilters
}

func (m *mockResultRepo) Create(_ context.Context, _ *Result) error { return nil }
func (m *mockResultRepo) GetByID(_ context.Context, _ types.ID) (*Result, error) {
	return nil, nil
}
func (m *mockResultRepo) ListByWorkUnit(_ context.Context, _ types.ID) ([]*Result, error) {
	return nil, nil
}
func (m *mockResultRepo) ListByVolunteer(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockResultRepo) ListByLeaf(_ context.Context, _ types.ID, filters ResultFilters, _ types.PaginationRequest) ([]*Result, types.PaginationResponse, error) {
	m.capturedFilters = &filters
	if m.err != nil {
		return nil, types.PaginationResponse{}, m.err
	}
	return m.results, m.pagination, nil
}
func (m *mockResultRepo) CountByWorkUnit(_ context.Context, _ types.ID) (int, error) {
	return 0, nil
}
func (m *mockResultRepo) CountPendingByWorkUnit(_ context.Context, _ types.ID) (int, error) {
	return 0, nil
}
func (m *mockResultRepo) UpdateValidationStatus(_ context.Context, _ types.ID, _ ValidationStatus) error {
	return nil
}
func (m *mockResultRepo) BatchUpdateValidationStatus(_ context.Context, _ []types.ID, _ ValidationStatus) error {
	return nil
}

// mockLeafRepo is a test implementation of leaf.Repository.
type mockLeafRepo struct {
	proj *leaf.Leaf
	err  error
}

func (m *mockLeafRepo) Create(_ context.Context, _ *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.proj != nil {
		return m.proj, nil
	}
	return &leaf.Leaf{ID: id, Name: "Test Project"}, nil
}
func (m *mockLeafRepo) GetBySlug(_ context.Context, _ string, _ *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) GetBySlugPublic(_ context.Context, _ string) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) List(_ context.Context, _ leaf.LeafListFilters, _ types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockLeafRepo) Update(_ context.Context, _ *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) Delete(_ context.Context, _ types.ID) error        { return nil }

func setupTestHandler(resultRepo *mockResultRepo, leafRepo *mockLeafRepo) (*httptest.Server, func()) {
	handler := NewResultHandler(resultRepo, leafRepo, slog.Default())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/results", handler.HandleListByLeaf)
	ts := httptest.NewServer(mux)
	return ts, ts.Close
}

func TestHandleListByProject_NoFilters(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      types.NewID(),
			OutputChecksum:   "abc123",
			ValidationStatus: ValidationAgreed,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      types.NewID(),
			OutputChecksum:   "def456",
			ValidationStatus: ValidationPending,
			SubmittedAt:      now.Add(-time.Hour),
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	ts, cleanup := setupTestHandler(
		&mockResultRepo{results: results},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[*Result]
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}

	if len(listResp.Data) != 2 {
		t.Errorf("expected 2 results, got %d", len(listResp.Data))
	}
}

func TestHandleListByProject_FilterByValidationStatus(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      types.NewID(),
			OutputChecksum:   "abc123",
			ValidationStatus: ValidationAgreed,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	ts, cleanup := setupTestHandler(
		&mockResultRepo{results: results},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?validation_status=AGREED")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[*Result]
	json.NewDecoder(resp.Body).Decode(&listResp)
	if len(listResp.Data) != 1 {
		t.Errorf("expected 1 result, got %d", len(listResp.Data))
	}
}

func TestHandleListByProject_FilterByAwaitingContentVerification(t *testing.T) {
	leafID := types.NewID()

	repo := &mockResultRepo{results: nil}
	ts, cleanup := setupTestHandler(repo, &mockLeafRepo{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?validation_status=AWAITING_CONTENT_VERIFICATION")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// The held status must reach the repository as a positive-form filter.
	if repo.capturedFilters == nil || repo.capturedFilters.ValidationStatus == nil {
		t.Fatal("expected ValidationStatus filter to be captured")
	}
	if *repo.capturedFilters.ValidationStatus != ValidationAwaitingContentVerification {
		t.Errorf("ValidationStatus = %v, want %v", *repo.capturedFilters.ValidationStatus, ValidationAwaitingContentVerification)
	}
}

func TestHandleListByProject_FilterByContentVerificationFailed(t *testing.T) {
	leafID := types.NewID()

	repo := &mockResultRepo{results: nil}
	ts, cleanup := setupTestHandler(repo, &mockLeafRepo{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?validation_status=CONTENT_VERIFICATION_FAILED")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	if repo.capturedFilters == nil || repo.capturedFilters.ValidationStatus == nil {
		t.Fatal("expected ValidationStatus filter to be captured")
	}
	if *repo.capturedFilters.ValidationStatus != ValidationContentVerificationFailed {
		t.Errorf("ValidationStatus = %v, want %v", *repo.capturedFilters.ValidationStatus, ValidationContentVerificationFailed)
	}
}

func TestHandleListByProject_FilterByWorkUnitID(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	wuID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       wuID,
			VolunteerID:      types.NewID(),
			OutputChecksum:   "abc123",
			ValidationStatus: ValidationPending,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	ts, cleanup := setupTestHandler(
		&mockResultRepo{results: results},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?work_unit_id=" + wuID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_Pagination(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      types.NewID(),
			OutputChecksum:   "abc",
			ValidationStatus: ValidationPending,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	ts, cleanup := setupTestHandler(
		&mockResultRepo{
			results:    results,
			pagination: types.PaginationResponse{HasMore: true, NextCursor: "next-cursor-abc"},
		},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[*Result]
	json.NewDecoder(resp.Body).Decode(&listResp)
	if !listResp.Pagination.HasMore {
		t.Error("expected has_more=true")
	}
	if listResp.Pagination.NextCursor != "next-cursor-abc" {
		t.Errorf("expected next_cursor=next-cursor-abc, got %s", listResp.Pagination.NextCursor)
	}
}

func TestHandleListByProject_ProjectNotFound(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{},
		&mockLeafRepo{err: apierror.NotFound("project", leafID.String())},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_InvalidProjectID(t *testing.T) {
	ts, cleanup := setupTestHandler(
		&mockResultRepo{},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/not-a-uuid/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_InvalidValidationStatus(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?validation_status=INVALID")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_InvalidWorkUnitID(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?work_unit_id=not-a-uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_RepoError(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{err: apierror.Internal("database error", nil)},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_EmptyResults(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{results: nil},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var listResp types.ListResponse[*Result]
	json.NewDecoder(resp.Body).Decode(&listResp)

	// Data should be empty array, not null.
	if listResp.Data == nil {
		t.Error("expected empty array, got nil")
	}
	if len(listResp.Data) != 0 {
		t.Errorf("expected 0 results, got %d", len(listResp.Data))
	}
}

// --- S109: volunteer_id filter tests ---

func TestHandleListByProject_FilterByVolunteerID(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	volID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      volID,
			OutputChecksum:   "abc123",
			ValidationStatus: ValidationAgreed,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	repo := &mockResultRepo{results: results}
	ts, cleanup := setupTestHandler(repo, &mockLeafRepo{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?volunteer_id=" + volID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify the filter was passed through to the repository.
	if repo.capturedFilters == nil {
		t.Fatal("expected filters to be captured")
	}
	if repo.capturedFilters.VolunteerID == nil {
		t.Fatal("expected VolunteerID filter to be set")
	}
	if *repo.capturedFilters.VolunteerID != volID {
		t.Errorf("VolunteerID = %v, want %v", *repo.capturedFilters.VolunteerID, volID)
	}
}

func TestHandleListByProject_InvalidVolunteerID(t *testing.T) {
	leafID := types.NewID()

	ts, cleanup := setupTestHandler(
		&mockResultRepo{},
		&mockLeafRepo{},
	)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results?volunteer_id=not-a-uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleListByProject_NoVolunteerID_BackwardCompat(t *testing.T) {
	now := time.Now().UTC()
	leafID := types.NewID()
	results := []*Result{
		{
			ID:               types.NewID(),
			WorkUnitID:       types.NewID(),
			VolunteerID:      types.NewID(),
			OutputChecksum:   "abc123",
			ValidationStatus: ValidationPending,
			SubmittedAt:      now,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}

	repo := &mockResultRepo{results: results}
	ts, cleanup := setupTestHandler(repo, &mockLeafRepo{})
	defer cleanup()

	// Call without volunteer_id — backward compatibility.
	resp, err := http.Get(ts.URL + "/api/v1/leafs/" + leafID.String() + "/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify that VolunteerID filter was NOT set.
	if repo.capturedFilters == nil {
		t.Fatal("expected filters to be captured")
	}
	if repo.capturedFilters.VolunteerID != nil {
		t.Errorf("expected VolunteerID filter to be nil, got %v", *repo.capturedFilters.VolunteerID)
	}
}
