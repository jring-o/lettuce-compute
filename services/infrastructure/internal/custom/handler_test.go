package custom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Mock repositories ---

type mockLeafRepo struct {
	proj *leaf.Leaf
	err  error
}

func (m *mockLeafRepo) Create(ctx context.Context, p *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) GetByID(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.proj == nil {
		return nil, apierror.NotFound("project", id.String())
	}
	return m.proj, nil
}
func (m *mockLeafRepo) GetBySlug(ctx context.Context, slug string, creatorID *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) GetBySlugPublic(ctx context.Context, slug string) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) List(ctx context.Context, filters leaf.LeafListFilters, page types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockLeafRepo) Update(ctx context.Context, p *leaf.Leaf) error { return nil }
func (m *mockLeafRepo) Delete(ctx context.Context, id types.ID) error        { return nil }

type mockWURepo struct {
	bulkCreated    []*workunit.WorkUnit
	bulkCreateErr  error
	transitionErr  error
	transitionRows int64
}

func (m *mockWURepo) Create(ctx context.Context, wu *workunit.WorkUnit) error { return nil }
func (m *mockWURepo) GetByID(ctx context.Context, id types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) List(ctx context.Context, filters workunit.WorkUnitListFilters, page types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockWURepo) UpdateState(ctx context.Context, id types.ID, from, to workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) BulkCreate(ctx context.Context, wus []*workunit.WorkUnit) error {
	m.bulkCreated = wus
	return m.bulkCreateErr
}
func (m *mockWURepo) BulkTransitionByBatch(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
	if m.transitionErr != nil {
		return 0, m.transitionErr
	}
	return m.transitionRows, nil
}
func (m *mockWURepo) FindNextAssignable(ctx context.Context, opts workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) ReserveNextAssignable(ctx context.Context, opts workunit.AssignmentOptions, lease time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) StampReservation(ctx context.Context, id, volunteerID types.ID, lease time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) ClearReservation(ctx context.Context, id, volunteerID types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) Assign(ctx context.Context, workUnitID, volunteerID types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindExpiredWorkUnits(ctx context.Context, limit int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindLapsedReservations(ctx context.Context, limit int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindDispatchableBatch(ctx context.Context, limit int, excludeIDs []types.ID, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClaimDispatchableBatch(_ context.Context, _ types.ID, _ time.Duration, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) FlushReservations(ctx context.Context, recs []workunit.FlushReservation, headID types.ID, claimLease time.Duration) ([]types.ID, error) {
	return nil, nil
}
func (m *mockWURepo) CountActiveByVolunteer(ctx context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWURepo) TransitionToExpired(ctx context.Context, id types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) Reassign(ctx context.Context, id types.ID) (*workunit.WorkUnit, bool, error) {
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

type mockBatchRepo struct {
	batches   []*workunit.Batch
	createErr error
}

func (m *mockBatchRepo) Create(ctx context.Context, b *workunit.Batch) error {
	if m.createErr != nil {
		return m.createErr
	}
	b.ID = types.NewID()
	m.batches = append(m.batches, b)
	return nil
}
func (m *mockBatchRepo) GetByID(ctx context.Context, id types.ID) (*workunit.Batch, error) {
	return nil, nil
}
func (m *mockBatchRepo) ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*workunit.Batch, types.PaginationResponse, error) {
	return m.batches, types.PaginationResponse{}, nil
}
func (m *mockBatchRepo) IncrementCompleted(ctx context.Context, batchID types.ID) error { return nil }

// --- Helpers ---

func makeCustomProject() *leaf.Leaf {
	img := "python:3.12"
	return &leaf.Leaf{
		ID:          types.NewID(),
		Name:        "Test Custom Project",
		State:       leaf.StateConfiguring,
		TaskPattern: leaf.PatternCustom,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime: "CONTAINER",
			Image:   &img,
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 3.0,
			MaxReassignments:   3,
		},
		DataConfig: leaf.DataConfig{
			MaxInputSizeBytes:  1048576, // 1 MB
			MaxOutputSizeBytes: 104857600,
		},
	}
}

func makeBulkRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/api/v1/leafs/{leaf_id}/work-units/bulk", bytes.NewReader(b))
}

func setPathValue(r *http.Request, key, value string) *http.Request {
	r.SetPathValue(key, value)
	return r
}

// --- Tests ---

func TestHandleBulkUpload(t *testing.T) {
	tests := []struct {
		name           string
		proj           *leaf.Leaf
		projErr        error
		body           any
		leafID      string
		wantStatus     int
		wantContains   string
		checkCreated   func(t *testing.T, wus []*workunit.WorkUnit)
	}{
		{
			name:      "happy path: 5 work units with mixed input types",
			proj:      makeCustomProject(),
			leafID: "", // will be filled from proj
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{InputData: json.RawMessage(`{"key":"val1"}`)},
					{InputDataRef: strPtr("https://example.com/data.json")},
					{Parameters: json.RawMessage(`{"algo":"bfs"}`)},
					{
						InputData:                json.RawMessage(`{"key":"val4"}`),
						Parameters:               json.RawMessage(`{"step":1}`),
						EstimatedDurationSeconds: intPtr(3600),
						OutputSpec:               json.RawMessage(`{"format":"json"}`),
					},
					{InputDataRef: strPtr("https://example.com/chunk2.json"), Parameters: json.RawMessage(`{"step":2}`)},
				},
			},
			wantStatus: http.StatusCreated,
			checkCreated: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				if len(wus) != 5 {
					t.Fatalf("expected 5 work units, got %d", len(wus))
				}
				// Check code_artifact_ref inherited from project
				for i, wu := range wus {
					if wu.CodeArtifactRef != "python:3.12" {
						t.Errorf("work unit %d: expected code_artifact_ref 'python:3.12', got %q", i, wu.CodeArtifactRef)
					}
					// deadline = 3600 * 3.0 = 10800
					if wu.DeadlineSeconds != 10800 {
						t.Errorf("work unit %d: expected deadline_seconds 10800, got %d", i, wu.DeadlineSeconds)
					}
				}
				// Check estimated_duration on unit 3
				if wus[3].EstimatedDurationSeconds == nil || *wus[3].EstimatedDurationSeconds != 3600 {
					t.Error("work unit 3: expected estimated_duration_seconds 3600")
				}
			},
		},
		{
			name: "max batch: 10000 work units",
			proj: makeCustomProject(),
			body: func() BulkUploadRequest {
				wus := make([]WorkUnitInput, 10000)
				for i := range wus {
					wus[i] = WorkUnitInput{Parameters: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}
				}
				return BulkUploadRequest{WorkUnits: wus}
			}(),
			wantStatus: http.StatusCreated,
		},
		{
			name: "over max: 10001 work units",
			proj: makeCustomProject(),
			body: func() BulkUploadRequest {
				wus := make([]WorkUnitInput, 10001)
				for i := range wus {
					wus[i] = WorkUnitInput{Parameters: json.RawMessage(`{"i":1}`)}
				}
				return BulkUploadRequest{WorkUnits: wus}
			}(),
			wantStatus:   http.StatusBadRequest,
			wantContains: "exceeds maximum",
		},
		{
			name: "empty array",
			proj: makeCustomProject(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{},
			},
			wantStatus:   http.StatusBadRequest,
			wantContains: "must not be empty",
		},
		{
			name: "work unit with no input (all nil)",
			proj: makeCustomProject(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{}, // no input_data, input_data_ref, or parameters
				},
			},
			wantStatus:   http.StatusBadRequest,
			wantContains: "at least one of",
		},
		{
			name: "input_data too large",
			proj: func() *leaf.Leaf {
				p := makeCustomProject()
				p.DataConfig.MaxInputSizeBytes = 10 // tiny limit
				return p
			}(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{InputData: json.RawMessage(`{"large":"data_that_exceeds_limit"}`)},
				},
			},
			wantStatus:   http.StatusBadRequest,
			wantContains: "exceeds max_input_size_bytes",
		},
		{
			name: "non-CUSTOM leaf",
			proj: func() *leaf.Leaf {
				p := makeCustomProject()
				p.TaskPattern = leaf.PatternParameterSweep
				return p
			}(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`)},
				},
			},
			wantStatus:   http.StatusForbidden,
			wantContains: "CUSTOM",
		},
		{
			name:         "leaf not found",
			proj:         nil,
			projErr:      apierror.NotFound("project", "some-id"),
			body:         BulkUploadRequest{WorkUnits: []WorkUnitInput{{Parameters: json.RawMessage(`{"key":"val"}`)}}},
			wantStatus:   http.StatusNotFound,
			wantContains: "not found",
		},
		{
			name: "leaf in DRAFT state",
			proj: func() *leaf.Leaf {
				p := makeCustomProject()
				p.State = leaf.StateDraft
				return p
			}(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`)},
				},
			},
			wantStatus:   http.StatusConflict,
			wantContains: "CONFIGURING or ACTIVE",
		},
		{
			name: "invalid leaf_id",
			proj: makeCustomProject(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`)},
				},
			},
			leafID:    "not-a-uuid",
			wantStatus:   http.StatusBadRequest,
			wantContains: "invalid leaf_id",
		},
		{
			name: "estimated_duration_seconds zero",
			proj: makeCustomProject(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`), EstimatedDurationSeconds: intPtr(0)},
				},
			},
			wantStatus:   http.StatusBadRequest,
			wantContains: "estimated_duration_seconds",
		},
		{
			name: "leaf in ACTIVE state succeeds",
			proj: func() *leaf.Leaf {
				p := makeCustomProject()
				p.State = leaf.StateActive
				return p
			}(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`)},
				},
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "leaf in COMPLETED state rejected",
			proj: func() *leaf.Leaf {
				p := makeCustomProject()
				p.State = leaf.StateCompleted
				return p
			}(),
			body: BulkUploadRequest{
				WorkUnits: []WorkUnitInput{
					{Parameters: json.RawMessage(`{"key":"val"}`)},
				},
			},
			wantStatus:   http.StatusConflict,
			wantContains: "CONFIGURING or ACTIVE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wuRepo := &mockWURepo{transitionRows: 5}
			batchRepo := &mockBatchRepo{}
			leafRepo := &mockLeafRepo{proj: tt.proj, err: tt.projErr}

			handler := NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, slog.Default())

			req := makeBulkRequest(t, tt.body)

			// Set leaf_id path parameter
			pid := tt.leafID
			if pid == "" && tt.proj != nil {
				pid = tt.proj.ID.String()
			} else if pid == "" {
				pid = types.NewID().String()
			}
			req = setPathValue(req, "leaf_id", pid)

			rr := httptest.NewRecorder()
			handler.HandleBulkUpload(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.wantStatus, rr.Code, rr.Body.String())
			}

			if tt.wantContains != "" && !strings.Contains(rr.Body.String(), tt.wantContains) {
				t.Errorf("expected response to contain %q, got: %s", tt.wantContains, rr.Body.String())
			}

			if tt.checkCreated != nil {
				tt.checkCreated(t, wuRepo.bulkCreated)
			}
		})
	}
}

func TestGenerate_ReturnsRedirectError(t *testing.T) {
	proj := makeCustomProject()

	_, err := Generate(context.Background(), proj, nil, 0, nil, nil)
	if err == nil {
		t.Fatal("expected error from Generate for custom pattern")
	}

	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}

	if apiErr.Code != "INVALID_PATTERN_FOR_GENERATE" {
		t.Errorf("expected code INVALID_PATTERN_FOR_GENERATE, got %q", apiErr.Code)
	}

	if apiErr.HTTPStatus != 400 {
		t.Errorf("expected HTTP 400, got %d", apiErr.HTTPStatus)
	}

	if !strings.Contains(apiErr.Message, "/work-units/bulk") {
		t.Errorf("expected message to mention /work-units/bulk, got: %s", apiErr.Message)
	}

	details, ok := apiErr.Details.(map[string]string)
	if !ok {
		t.Fatalf("expected details to be map[string]string, got %T", apiErr.Details)
	}
	if !strings.Contains(details["redirect_endpoint"], "/work-units/bulk") {
		t.Errorf("expected redirect_endpoint to contain /work-units/bulk, got: %s", details["redirect_endpoint"])
	}
}

func TestHandleBulkUpload_InvalidJSON(t *testing.T) {
	proj := makeCustomProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{}
	leafRepo := &mockLeafRepo{proj: proj}
	handler := NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/leafs/{leaf_id}/work-units/bulk",
		strings.NewReader("{invalid json"))
	req.SetPathValue("leaf_id", proj.ID.String())

	rr := httptest.NewRecorder()
	handler.HandleBulkUpload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleBulkUpload_RepoErrors(t *testing.T) {
	validBody := BulkUploadRequest{
		WorkUnits: []WorkUnitInput{
			{Parameters: json.RawMessage(`{"key":"val"}`)},
		},
	}

	t.Run("batch create error", func(t *testing.T) {
		proj := makeCustomProject()
		wuRepo := &mockWURepo{}
		batchRepo := &mockBatchRepo{createErr: fmt.Errorf("db connection lost")}
		leafRepo := &mockLeafRepo{proj: proj}
		handler := NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, slog.Default())

		b, _ := json.Marshal(validBody)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
		req.SetPathValue("leaf_id", proj.ID.String())

		rr := httptest.NewRecorder()
		handler.HandleBulkUpload(rr, req)

		if rr.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("bulk create error", func(t *testing.T) {
		proj := makeCustomProject()
		wuRepo := &mockWURepo{bulkCreateErr: fmt.Errorf("copy failed")}
		batchRepo := &mockBatchRepo{}
		leafRepo := &mockLeafRepo{proj: proj}
		handler := NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, slog.Default())

		b, _ := json.Marshal(validBody)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
		req.SetPathValue("leaf_id", proj.ID.String())

		rr := httptest.NewRecorder()
		handler.HandleBulkUpload(rr, req)

		if rr.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("transition error", func(t *testing.T) {
		proj := makeCustomProject()
		wuRepo := &mockWURepo{transitionErr: fmt.Errorf("update failed")}
		batchRepo := &mockBatchRepo{}
		leafRepo := &mockLeafRepo{proj: proj}
		handler := NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, slog.Default())

		b, _ := json.Marshal(validBody)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
		req.SetPathValue("leaf_id", proj.ID.String())

		rr := httptest.NewRecorder()
		handler.HandleBulkUpload(rr, req)

		if rr.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d; body: %s", rr.Code, rr.Body.String())
		}
	})
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
