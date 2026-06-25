package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Mock repositories for browser volunteer tests ---
// Prefixed with "bv" to avoid conflict with existing mocks in server_test.go / auth_test.go.

type bvMockVolunteerRepo struct {
	volunteers map[string]*volunteer.Volunteer // keyed by base64(pubkey)
}

func newBVMockVolunteerRepo() *bvMockVolunteerRepo {
	return &bvMockVolunteerRepo{volunteers: make(map[string]*volunteer.Volunteer)}
}

func (m *bvMockVolunteerRepo) Create(_ context.Context, v *volunteer.Volunteer) error {
	v.ID = types.NewID()
	v.CreatedAt = time.Now().UTC()
	v.RegisteredAt = v.CreatedAt
	key := base64.StdEncoding.EncodeToString(v.PublicKey)
	m.volunteers[key] = v
	return nil
}

func (m *bvMockVolunteerRepo) GetByPublicKey(_ context.Context, publicKey []byte) (*volunteer.Volunteer, error) {
	key := base64.StdEncoding.EncodeToString(publicKey)
	if v, ok := m.volunteers[key]; ok {
		return v, nil
	}
	return nil, apierror.NotFound("volunteer", "by-pubkey")
}

func (m *bvMockVolunteerRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Volunteer, error) {
	for _, v := range m.volunteers {
		if v.ID == id {
			return v, nil
		}
	}
	return nil, apierror.NotFound("volunteer", id.String())
}

func (m *bvMockVolunteerRepo) GetByUserID(context.Context, types.ID) (*volunteer.Volunteer, error) {
	return nil, apierror.NotFound("volunteer", "by-user")
}
func (m *bvMockVolunteerRepo) Update(context.Context, *volunteer.Volunteer) error { return nil }
func (m *bvMockVolunteerRepo) UpdateLastSeen(context.Context, types.ID) error     { return nil }
func (m *bvMockVolunteerRepo) SetActive(context.Context, types.ID, bool) error    { return nil }
func (m *bvMockVolunteerRepo) IncrementWorkUnitsCompleted(context.Context, types.ID) error {
	return nil
}
func (m *bvMockVolunteerRepo) IncrementWorkUnitsRejected(context.Context, types.ID) error {
	return nil
}
func (m *bvMockVolunteerRepo) List(context.Context, volunteer.VolunteerListFilters, types.PaginationRequest) ([]*volunteer.Volunteer, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockVolunteerRepo) MarkInactiveOlderThan(context.Context, time.Duration) (int, error) {
	return 0, nil
}

type bvMockWURepo struct {
	wus []*workunit.WorkUnit
}

func (m *bvMockWURepo) Create(context.Context, *workunit.WorkUnit) error       { return nil }
func (m *bvMockWURepo) BulkCreate(context.Context, []*workunit.WorkUnit) error { return nil }
func (m *bvMockWURepo) GetByID(_ context.Context, id types.ID) (*workunit.WorkUnit, error) {
	for _, wu := range m.wus {
		if wu.ID == id {
			return wu, nil
		}
	}
	return nil, apierror.NotFound("work_unit", id.String())
}
func (m *bvMockWURepo) List(context.Context, workunit.WorkUnitListFilters, types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockWURepo) UpdateState(context.Context, types.ID, workunit.WorkUnitState, workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *bvMockWURepo) BulkTransitionByBatch(context.Context, types.ID, workunit.WorkUnitState, workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *bvMockWURepo) FindNextAssignable(context.Context, workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	if len(m.wus) == 0 {
		return nil, nil
	}
	return m.wus[0], nil
}
func (m *bvMockWURepo) ReserveNextAssignable(context.Context, workunit.AssignmentOptions, time.Duration) (*workunit.WorkUnit, error) {
	if len(m.wus) == 0 {
		return nil, nil
	}
	return m.wus[0], nil
}
func (m *bvMockWURepo) ReserveCopy(_ context.Context, _, _ types.ID, _ *types.ID, _ time.Time, _ int) (*workunit.Copy, error) {
	return nil, nil
}
func (m *bvMockWURepo) Assign(_ context.Context, wuID types.ID, volID types.ID) (*workunit.WorkUnit, error) {
	for _, wu := range m.wus {
		if wu.ID == wuID {
			wu.AssignedVolunteerID = &volID
			wu.State = workunit.WorkUnitStateAssigned
			return wu, nil
		}
	}
	return nil, apierror.NotFound("work_unit", wuID.String())
}
func (m *bvMockWURepo) CountByLeafAndState(context.Context, types.ID, workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *bvMockWURepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}
func (m *bvMockWURepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *bvMockWURepo) FindDispatchableBatch(context.Context, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *bvMockWURepo) ClaimDispatchableBatch(context.Context, types.ID, time.Duration, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *bvMockWURepo) ClearExpiredDispatchClaims(context.Context) (int64, error) {
	return 0, nil
}
func (m *bvMockWURepo) ReleaseStaleBufferedCopies(context.Context, types.ID, []types.ID, time.Time) ([]types.ID, error) {
	return nil, nil
}
func (m *bvMockWURepo) FlushReservations(context.Context, []workunit.FlushReservation, types.ID, time.Duration) ([]workunit.FlushedCopy, error) {
	return nil, nil
}
func (m *bvMockWURepo) CountActiveByVolunteer(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *bvMockWURepo) CountActiveByHost(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *bvMockWURepo) CloseCopy(context.Context, types.ID, string) error { return nil }
func (m *bvMockWURepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID) error {
	return nil
}
func (m *bvMockWURepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}
func (m *bvMockWURepo) CountLiveCopies(context.Context, types.ID) (int, error)  { return 0, nil }
func (m *bvMockWURepo) CountTotalCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (m *bvMockWURepo) CountErrorCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (m *bvMockWURepo) MarkCompleted(context.Context, types.ID) error           { return nil }
func (m *bvMockWURepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	return false, nil
}
func (m *bvMockWURepo) Reassign(context.Context, types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (m *bvMockWURepo) MarkSpotCheck(context.Context, types.ID) error  { return nil }
func (m *bvMockWURepo) ClearSpotCheck(context.Context, types.ID) error { return nil }
func (m *bvMockWURepo) EnsureWorkUnitHRClass(_ context.Context, _ types.ID, class string) (string, error) {
	return class, nil
}
func (m *bvMockWURepo) FindRunningWithStaleCheckpoints(context.Context, int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}

type bvMockLeafRepo struct {
	leafs map[types.ID]*leaf.Leaf
}

func newBVMockLeafRepo() *bvMockLeafRepo {
	return &bvMockLeafRepo{leafs: make(map[types.ID]*leaf.Leaf)}
}

func (m *bvMockLeafRepo) Create(context.Context, *leaf.Leaf) error { return nil }
func (m *bvMockLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	if l, ok := m.leafs[id]; ok {
		return l, nil
	}
	return nil, apierror.NotFound("leaf", id.String())
}
func (m *bvMockLeafRepo) GetBySlug(context.Context, string, *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *bvMockLeafRepo) GetBySlugPublic(context.Context, string) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *bvMockLeafRepo) List(context.Context, leaf.LeafListFilters, types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockLeafRepo) Update(context.Context, *leaf.Leaf) error { return nil }
func (m *bvMockLeafRepo) Delete(context.Context, types.ID) error   { return nil }

type bvMockAssignRepo struct {
	entries []*assignment.AssignmentHistoryEntry
}

func (m *bvMockAssignRepo) Create(_ context.Context, e *assignment.AssignmentHistoryEntry) error {
	e.ID = types.NewID()
	m.entries = append(m.entries, e)
	return nil
}
func (m *bvMockAssignRepo) GetByID(context.Context, types.ID) (*assignment.AssignmentHistoryEntry, error) {
	return nil, nil
}
func (m *bvMockAssignRepo) ListByWorkUnit(context.Context, types.ID) ([]*assignment.AssignmentHistoryEntry, error) {
	return nil, nil
}
func (m *bvMockAssignRepo) ListByVolunteer(context.Context, types.ID, types.PaginationRequest) ([]*assignment.AssignmentHistoryEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockAssignRepo) CountActiveByWorkUnit(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *bvMockAssignRepo) UpdateOutcome(context.Context, types.ID, assignment.AssignmentOutcome, *types.ID) error {
	return nil
}
func (m *bvMockAssignRepo) FindActiveByWorkUnitAndVolunteer(_ context.Context, wuID, volID types.ID) (*assignment.AssignmentHistoryEntry, error) {
	for _, e := range m.entries {
		if e.WorkUnitID == wuID && e.VolunteerID == volID && e.Outcome == nil {
			return e, nil
		}
	}
	return nil, apierror.NotFound("assignment", wuID.String())
}
func (m *bvMockAssignRepo) FindLatestByWorkUnitAndVolunteer(_ context.Context, wuID, volID types.ID) (*assignment.AssignmentHistoryEntry, error) {
	var latest *assignment.AssignmentHistoryEntry
	for _, e := range m.entries {
		if e.WorkUnitID == wuID && e.VolunteerID == volID {
			latest = e
		}
	}
	if latest == nil {
		return nil, apierror.NotFound("assignment", wuID.String())
	}
	return latest, nil
}

type bvMockResultRepo struct {
	results []*result.Result
}

func (m *bvMockResultRepo) Create(_ context.Context, r *result.Result) error {
	r.ID = types.NewID()
	m.results = append(m.results, r)
	return nil
}
func (m *bvMockResultRepo) GetByID(context.Context, types.ID) (*result.Result, error) {
	return nil, nil
}
func (m *bvMockResultRepo) ListByWorkUnit(context.Context, types.ID) ([]*result.Result, error) {
	return nil, nil
}
func (m *bvMockResultRepo) ListByVolunteer(context.Context, types.ID, types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockResultRepo) ListByLeaf(context.Context, types.ID, result.ResultFilters, types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *bvMockResultRepo) CountByWorkUnit(context.Context, types.ID) (int, error) { return 0, nil }
func (m *bvMockResultRepo) CountPendingByWorkUnit(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *bvMockResultRepo) UpdateValidationStatus(context.Context, types.ID, result.ValidationStatus) error {
	return nil
}
func (m *bvMockResultRepo) BatchUpdateValidationStatus(context.Context, []types.ID, result.ValidationStatus) error {
	return nil
}

// --- Helper to build deps ---

func testBrowserDeps() (*browserVolunteerDeps, *bvMockVolunteerRepo, *bvMockWURepo, *bvMockLeafRepo, *bvMockAssignRepo, *bvMockResultRepo) {
	volRepo := newBVMockVolunteerRepo()
	wuRepo := &bvMockWURepo{}
	leafRepo := newBVMockLeafRepo()
	assignRepo := &bvMockAssignRepo{}
	resultRepo := &bvMockResultRepo{}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	deps := &browserVolunteerDeps{
		pool:                    nil,
		volunteerRepo:           volRepo,
		wuRepo:                  wuRepo,
		leafRepo:                leafRepo,
		assignRepo:              assignRepo,
		resultRepo:              resultRepo,
		validationEngine:        nil,
		logger:                  logger,
		headName:                "test-head",
		maxInflightPerVolunteer: 5,
	}

	return deps, volRepo, wuRepo, leafRepo, assignRepo, resultRepo
}

// --- Register tests ---

func TestBrowserRegister_ValidRequest(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserRegister(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pubKey)

	body := `{"public_key":"` + pubB64 + `","display_name":"Test Volunteer","hardware":{"cpu_cores":8,"memory_mb":16384,"has_gpu":true,"gpu_vendors":["WEBGPU"],"available_runtimes":["WASM"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp browserRegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.VolunteerID == "" {
		t.Error("expected non-empty volunteer_id")
	}
}

func TestBrowserRegister_DuplicatePublicKey(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserRegister(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pubKey)

	body := `{"public_key":"` + pubB64 + `","hardware":{"cpu_cores":4,"memory_mb":8192,"available_runtimes":["WASM"]}}`

	// First registration.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 for first registration, got %d", rec1.Code)
	}

	// Second registration — same key.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate registration, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp browserRegisterResponse
	json.NewDecoder(rec2.Body).Decode(&resp)
	if resp.VolunteerID == "" {
		t.Error("expected existing volunteer_id in 409 response")
	}
}

func TestBrowserRegister_MissingPublicKey(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserRegister(deps)

	body := `{"display_name":"No Key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing public_key, got %d", rec.Code)
	}
}

// --- Request-work test (unauthenticated) ---

func TestBrowserRequestWork_Unauthenticated(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := ed25519AuthRequired(handleBrowserRequestWork(deps))

	body := `{"max_memory_mb":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/request-work", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// --- Pre-transaction validation tests ---
// Note: request-work, submit-result, and heartbeat handlers use pool.Begin() for transactions,
// so full flow tests require a real database (integration tests). These unit tests verify
// the handler logic up to the point of transaction creation.

func TestBrowserRequestWork_VolunteerNotRegistered(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserRequestWork(deps)

	// Use a pubkey that hasn't been registered.
	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"max_memory_mb":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/request-work", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unregistered volunteer, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrowserRequestWork_InvalidLeafID(t *testing.T) {
	deps, volRepo, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserRequestWork(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	volRepo.Create(context.Background(), &volunteer.Volunteer{
		PublicKey:         pubKey,
		AvailableRuntimes: []string{"WASM"},
		IsActive:          true,
		SchedulingMode:    volunteer.ScheduleAlways,
		HardwareCapabilities: volunteer.HardwareCapabilities{
			CPUCores: 4, MaxCPUCores: 4, MemoryTotalMB: 8192, MaxMemoryMB: 8192,
		},
	})

	body := `{"leaf_ids":["not-a-uuid"],"max_memory_mb":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/request-work", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid leaf_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Submit-result pre-transaction tests ---

func TestBrowserSubmitResult_MissingAuth(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserSubmitResult(deps)

	body := `{"work_unit_id":"00000000-0000-0000-0000-000000000001"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestBrowserSubmitResult_MissingWorkUnitID(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserSubmitResult(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"output_data":"dGVzdA==","output_checksum":"abc"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrowserSubmitResult_InvalidChecksum(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserSubmitResult(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	body := `{"work_unit_id":"00000000-0000-0000-0000-000000000001","output_data":"dGVzdA==","output_checksum":"not-a-valid-sha256"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid checksum, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrowserSubmitResult_ChecksumMismatch(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserSubmitResult(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	// "dGVzdA==" is base64 for "test", but we provide a wrong checksum.
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"
	body := `{"work_unit_id":"00000000-0000-0000-0000-000000000001","output_data":"dGVzdA==","output_checksum":"` + wrongChecksum + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for checksum mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBrowserSubmitResult_VolunteerNotRegistered(t *testing.T) {
	deps, _, _, _, _, _ := testBrowserDeps()
	handler := handleBrowserSubmitResult(deps)

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	// Valid checksum for "test" (base64 "dGVzdA==").
	validChecksum := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	body := `{"work_unit_id":"00000000-0000-0000-0000-000000000001","output_data":"dGVzdA==","output_checksum":"` + validChecksum + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	ctx := ContextWithEd25519PubKey(req.Context(), pubKey)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unregistered volunteer, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Browser REST heartbeat tests removed: the browser heartbeat endpoint is gone
// (browser/WASM units run-start at assignment time; liveness is deadline-based).

// --- CORS middleware tests ---

func TestCORSMiddleware_EmptyOriginsDisabled(t *testing.T) {
	// Empty origins ⇒ cross-origin sharing disabled (fail-closed): NO
	// Access-Control-Allow-Origin and NO credentials header.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner, "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("expected no Allow-Origin when origins empty, got %q", origin)
	}
	if cred := rec.Header().Get("Access-Control-Allow-Credentials"); cred != "" {
		t.Errorf("expected no Allow-Credentials when origins empty, got %q", cred)
	}
	if maxAge := rec.Header().Get("Access-Control-Max-Age"); maxAge != "3600" {
		t.Errorf("expected Max-Age 3600, got %q", maxAge)
	}
}

func TestCORSMiddleware_ExplicitWildcardNoCredentials(t *testing.T) {
	// Explicit "*" ⇒ wildcard origin and NEVER a credentials header.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner, "*", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("expected wildcard origin, got %q", origin)
	}
	if cred := rec.Header().Get("Access-Control-Allow-Credentials"); cred != "" {
		t.Errorf("wildcard must never set credentials, got %q", cred)
	}
}

func TestCORSMiddleware_SpecificOriginMatches(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner, "https://dashboard.example.com,https://localhost:3000", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "https://dashboard.example.com" {
		t.Errorf("expected matching origin, got %q", origin)
	}
	if cred := rec.Header().Get("Access-Control-Allow-Credentials"); cred != "true" {
		t.Errorf("expected credentials true, got %q", cred)
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Errorf("expected Vary: Origin, got %q", vary)
	}
}

func TestCORSMiddleware_SpecificOriginNoMatch(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner, "https://dashboard.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("expected no origin for non-matching request, got %q", origin)
	}
}

func TestCORSMiddleware_PreflightOptions(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner, "https://dashboard.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/volunteers/register", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for OPTIONS, got %d", rec.Code)
	}
	if h := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(h, "Authorization") {
		t.Errorf("expected Authorization in Allow-Headers, got %q", h)
	}
}

// --- Auth middleware pass-through for Ed25519 ---

func TestAuthMiddleware_PassesEd25519Scheme(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	handler := authMiddleware(inner, nil, "test-admin-key", logger)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Authorization", "Ed25519 abc:def:123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("expected Ed25519 auth header to pass through authMiddleware as anonymous")
	}
}
