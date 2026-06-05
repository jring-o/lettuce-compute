package aggregation

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// mockLeafRepo implements leaf.Repository for tests.
type mockLeafRepo struct {
	leafs map[types.ID]*leaf.Leaf
}

func (m *mockLeafRepo) Create(_ context.Context, p *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	if p, ok := m.leafs[id]; ok {
		return p, nil
	}
	return nil, &notFoundErr{id: id}
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
func (m *mockLeafRepo) Delete(_ context.Context, _ types.ID) error         { return nil }

type notFoundErr struct{ id types.ID }

func (e *notFoundErr) Error() string { return "not found: " + e.id.String() }

// mockResultRepo implements result.Repository for tests.
type mockResultRepo struct {
	results []*result.Result
}

func (m *mockResultRepo) Create(_ context.Context, _ *result.Result) error            { return nil }
func (m *mockResultRepo) GetByID(_ context.Context, _ types.ID) (*result.Result, error) { return nil, nil }
func (m *mockResultRepo) ListByWorkUnit(_ context.Context, _ types.ID) ([]*result.Result, error) {
	return nil, nil
}
func (m *mockResultRepo) ListByVolunteer(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockResultRepo) ListByLeaf(_ context.Context, _ types.ID, filters result.ResultFilters, _ types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	var matched []*result.Result
	for _, r := range m.results {
		if filters.ValidationStatus != nil && r.ValidationStatus != *filters.ValidationStatus {
			continue
		}
		if filters.WorkUnitID != nil && r.WorkUnitID != *filters.WorkUnitID {
			continue
		}
		matched = append(matched, r)
	}
	return matched, types.PaginationResponse{}, nil
}
func (m *mockResultRepo) CountByWorkUnit(_ context.Context, _ types.ID) (int, error)        { return 0, nil }
func (m *mockResultRepo) CountPendingByWorkUnit(_ context.Context, _ types.ID) (int, error)  { return 0, nil }
func (m *mockResultRepo) UpdateValidationStatus(_ context.Context, _ types.ID, _ result.ValidationStatus) error {
	return nil
}
func (m *mockResultRepo) BatchUpdateValidationStatus(_ context.Context, _ []types.ID, _ result.ValidationStatus) error {
	return nil
}

// mockWURepo implements workunit.WorkUnitRepository for tests.
type mockWURepo struct {
	workUnits []*workunit.WorkUnit
}

func (m *mockWURepo) Create(_ context.Context, _ *workunit.WorkUnit) error { return nil }
func (m *mockWURepo) GetByID(_ context.Context, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) List(_ context.Context, filters workunit.WorkUnitListFilters, _ types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	var matched []*workunit.WorkUnit
	for _, wu := range m.workUnits {
		if filters.LeafID != nil && wu.LeafID != *filters.LeafID {
			continue
		}
		if filters.State != nil && wu.State != *filters.State {
			continue
		}
		matched = append(matched, wu)
	}
	return matched, types.PaginationResponse{}, nil
}
func (m *mockWURepo) UpdateState(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) BulkCreate(_ context.Context, _ []*workunit.WorkUnit) error { return nil }
func (m *mockWURepo) BulkTransitionByBatch(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) FindNextAssignable(_ context.Context, _ workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) ReserveNextAssignable(_ context.Context, _ workunit.AssignmentOptions, _ time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) StampReservation(_ context.Context, _, _ types.ID, _ time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) ClearReservation(_ context.Context, _, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) Assign(_ context.Context, _ types.ID, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindExpiredWorkUnits(_ context.Context, _ int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindLapsedReservations(_ context.Context, _ int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindDispatchableBatch(_ context.Context, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClaimDispatchableBatch(_ context.Context, _ types.ID, _ time.Duration, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) FlushReservations(_ context.Context, _ []workunit.FlushReservation, _ types.ID, _ time.Duration) ([]types.ID, error) {
	return nil, nil
}
func (m *mockWURepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWURepo) TransitionToExpired(_ context.Context, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) Reassign(_ context.Context, _ types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (m *mockWURepo) CountByLeafAndState(_ context.Context, _ types.ID, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) MarkSpotCheck(_ context.Context, _ types.ID) error  { return nil }
func (m *mockWURepo) ClearSpotCheck(_ context.Context, _ types.ID) error { return nil }
func (m *mockWURepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}

func setupTestHandler(t *testing.T) (*AggregationHandler, types.ID) {
	t.Helper()

	leafID := types.NewID()
	wuID := types.NewID()

	leafRepo := &mockLeafRepo{
		leafs: map[types.ID]*leaf.Leaf{
			leafID: {
				ID:          leafID,
				TaskPattern: leaf.PatternParameterSweep,
				DataConfig: leaf.DataConfig{
					AggregationFormat: "JSON",
				},
			},
		},
	}

	wuRepo := &mockWURepo{
		workUnits: []*workunit.WorkUnit{
			{
				ID:         wuID,
				LeafID:  leafID,
				State:      workunit.WorkUnitStateValidated,
				Parameters: json.RawMessage(`{"x":1}`),
				CreatedAt:  time.Now(),
			},
		},
	}

	resultRepo := &mockResultRepo{
		results: []*result.Result{
			{
				ID:               types.NewID(),
				WorkUnitID:       wuID,
				OutputData:       json.RawMessage(`{"result":42}`),
				ValidationStatus: result.ValidationAgreed,
			},
		},
	}

	logger := slog.Default()
	engine := NewEngine(resultRepo, wuRepo, leafRepo, logger)
	handler := NewAggregationHandler(engine, logger)

	return handler, leafID
}

func TestHandlerPostAggregate(t *testing.T) {
	handler, leafID := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing data field")
	}
	if data["status"] != "complete" {
		t.Errorf("status = %v, want complete", data["status"])
	}
}

func TestHandlerGetAggregate_NoCache(t *testing.T) {
	handler, leafID := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	req := httptest.NewRequest("GET", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404 (no cache)", w.Code)
	}
}

func TestHandlerGetAggregate_WithCache(t *testing.T) {
	handler, leafID := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	// First POST to populate cache.
	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", w.Code)
	}

	// Then GET should return cached result.
	req = httptest.NewRequest("GET", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", w.Code)
	}
}

func TestHandlerPostAggregate_FormatOverride(t *testing.T) {
	handler, leafID := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	body := `{"format":"csv","force":true}`
	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST csv status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].(map[string]interface{})
	if data["format"] != "csv" {
		t.Errorf("format = %v, want csv", data["format"])
	}
}

func TestHandlerPostAggregate_InvalidFormat(t *testing.T) {
	handler, leafID := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	body := `{"format":"parquet"}`
	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid format status = %d, want 400", w.Code)
	}
}

func TestHandlerPostAggregate_NotFound(t *testing.T) {
	handler, _ := setupTestHandler(t)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	fakeID := types.NewID()
	req := httptest.NewRequest("POST", "/api/v1/leafs/"+fakeID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("not found status = %d, want 404", w.Code)
	}
}

func TestHandlerPostAggregate_PartialStatus(t *testing.T) {
	// 3 work units total, but only 1 validated with agreed result → partial.
	leafID := types.NewID()
	wuIDValidated := types.NewID()
	wuIDQueued1 := types.NewID()
	wuIDQueued2 := types.NewID()

	leafRepo := &mockLeafRepo{
		leafs: map[types.ID]*leaf.Leaf{
			leafID: {
				ID:          leafID,
				TaskPattern: leaf.PatternParameterSweep,
				DataConfig:  leaf.DataConfig{AggregationFormat: "JSON"},
			},
		},
	}

	wuRepo := &mockWURepo{
		workUnits: []*workunit.WorkUnit{
			{ID: wuIDValidated, LeafID: leafID, State: workunit.WorkUnitStateValidated, Parameters: json.RawMessage(`{"x":1}`), CreatedAt: time.Now()},
			{ID: wuIDQueued1, LeafID: leafID, State: workunit.WorkUnitStateQueued, Parameters: json.RawMessage(`{"x":2}`), CreatedAt: time.Now()},
			{ID: wuIDQueued2, LeafID: leafID, State: workunit.WorkUnitStateQueued, Parameters: json.RawMessage(`{"x":3}`), CreatedAt: time.Now()},
		},
	}

	resultRepo := &mockResultRepo{
		results: []*result.Result{
			{ID: types.NewID(), WorkUnitID: wuIDValidated, OutputData: json.RawMessage(`{"result":10}`), ValidationStatus: result.ValidationAgreed},
		},
	}

	logger := slog.Default()
	engine := NewEngine(resultRepo, wuRepo, leafRepo, logger)
	handler := NewAggregationHandler(engine, logger)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].(map[string]interface{})

	if data["status"] != "partial" {
		t.Errorf("status = %v, want partial", data["status"])
	}
	if data["work_units_aggregated"] != float64(1) {
		t.Errorf("work_units_aggregated = %v, want 1", data["work_units_aggregated"])
	}
	if data["work_units_total"] != float64(3) {
		t.Errorf("work_units_total = %v, want 3", data["work_units_total"])
	}
}

func TestHandlerPostAggregate_CustomNoAggregation(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	leafRepo := &mockLeafRepo{
		leafs: map[types.ID]*leaf.Leaf{
			leafID: {
				ID:          leafID,
				TaskPattern: leaf.PatternCustom,
				DataConfig: leaf.DataConfig{
					AggregationFormat: "JSON",
					AggregationConfig: map[string]any{}, // no reducer
				},
			},
		},
	}

	wuRepo := &mockWURepo{
		workUnits: []*workunit.WorkUnit{
			{ID: wuID, LeafID: leafID, State: workunit.WorkUnitStateValidated, Parameters: json.RawMessage(`{}`), CreatedAt: time.Now()},
		},
	}

	resultRepo := &mockResultRepo{
		results: []*result.Result{
			{ID: types.NewID(), WorkUnitID: wuID, OutputData: json.RawMessage(`{"x":1}`), ValidationStatus: result.ValidationAgreed},
		},
	}

	logger := slog.Default()
	engine := NewEngine(resultRepo, wuRepo, leafRepo, logger)
	handler := NewAggregationHandler(engine, logger)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", handler.HandleAggregate)

	req := httptest.NewRequest("POST", "/api/v1/leafs/"+leafID.String()+"/aggregate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].(map[string]interface{})

	if data["status"] != "no_aggregation" {
		t.Errorf("status = %v, want no_aggregation", data["status"])
	}
}
