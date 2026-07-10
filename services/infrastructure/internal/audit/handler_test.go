package audit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- test doubles -----------------------------------------------------------------------

type registerCall struct {
	volunteerID types.ID
	label       string
	note        string
}

// fakeRunnersRepo is an in-memory RunnersRepository double recording calls.
type fakeRunnersRepo struct {
	runner *Runner
	list   []*Runner
	err    error

	registerCalls   []registerCall
	deactivateCalls []types.ID
}

func (f *fakeRunnersRepo) Register(_ context.Context, volunteerID types.ID, label, note string) (*Runner, error) {
	f.registerCalls = append(f.registerCalls, registerCall{volunteerID, label, note})
	if f.err != nil {
		return nil, f.err
	}
	return f.runner, nil
}

func (f *fakeRunnersRepo) Deactivate(_ context.Context, volunteerID types.ID) error {
	f.deactivateCalls = append(f.deactivateCalls, volunteerID)
	return f.err
}

func (f *fakeRunnersRepo) List(context.Context) ([]*Runner, error) {
	return f.list, f.err
}

func (f *fakeRunnersRepo) GetActiveByVolunteerID(context.Context, types.ID) (*Runner, error) {
	return f.runner, f.err
}

func (f *fakeRunnersRepo) ActiveRunnerSubjects(context.Context) ([]string, error) {
	return nil, f.err
}

// fakeAuditsRepo is an in-memory AuditsRepository double; the admin handler exercises List
// and FlaggedLeaves.
type fakeAuditsRepo struct {
	list       []*Audit
	flagged    []FlaggedLeaf
	err        error
	lastFilter ListFilter
}

func (f *fakeAuditsRepo) Enqueue(context.Context, *Audit) error { return f.err }
func (f *fakeAuditsRepo) Claim(context.Context, types.ID, string) (*Audit, error) {
	return nil, f.err
}
func (f *fakeAuditsRepo) GetByID(context.Context, types.ID) (*Audit, error) { return nil, f.err }
func (f *fakeAuditsRepo) CompleteVerdict(context.Context, types.ID, types.ID, Verdict, string, []byte, string, bool) error {
	return f.err
}
func (f *fakeAuditsRepo) CompleteInconclusive(context.Context, types.ID, types.ID, string) error {
	return f.err
}
func (f *fakeAuditsRepo) ReleaseFailure(context.Context, types.ID, types.ID, string) error {
	return f.err
}
func (f *fakeAuditsRepo) SweepLapsedLeases(context.Context) (int, int, error) { return 0, 0, f.err }
func (f *fakeAuditsRepo) SweepStaleQueued(context.Context) (int, error)       { return 0, f.err }
func (f *fakeAuditsRepo) Stats(context.Context) (Stats, error)                { return Stats{}, f.err }
func (f *fakeAuditsRepo) List(_ context.Context, filter ListFilter) ([]*Audit, error) {
	f.lastFilter = filter
	return f.list, f.err
}
func (f *fakeAuditsRepo) EnqueueConfirmation(context.Context, types.ID) (*Audit, error) {
	return nil, f.err
}
func (f *fakeAuditsRepo) GetRunnerOutput(context.Context, types.ID) ([]byte, error) {
	return nil, f.err
}
func (f *fakeAuditsRepo) ListActionableRoots(context.Context, int) ([]*Audit, error) {
	return nil, f.err
}
func (f *fakeAuditsRepo) ConfirmationsForRoot(context.Context, types.ID) ([]*Audit, error) {
	return nil, f.err
}
func (f *fakeAuditsRepo) SetEnforcementState(context.Context, types.ID, EnforcementState) (bool, error) {
	return false, f.err
}
func (f *fakeAuditsRepo) ClaimRepair(context.Context, types.ID, types.ID) (bool, error) {
	return false, f.err
}
func (f *fakeAuditsRepo) FlaggedLeaves(context.Context) ([]FlaggedLeaf, error) {
	return f.flagged, f.err
}

func testAdminHandler(runners RunnersRepository, audits AuditsRepository) *AdminHandler {
	return NewAdminHandler(runners, audits, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func adminContext() context.Context {
	return WithCaller(context.Background(), Caller{IsAdmin: true})
}

// --- authorization (F2): non-admin → 403 on every route ---------------------------------

func TestAdminRoutes_NonAdminForbidden(t *testing.T) {
	volID := types.NewID().String()
	routes := []struct {
		name   string
		invoke func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context)
	}{
		{"register", func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners",
				strings.NewReader(`{"volunteer_id":"`+volID+`","label":"box-1"}`)).WithContext(ctx)
			h.HandleRegisterRunner(rec, req)
		}},
		{"deactivate", func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners/deactivate",
				strings.NewReader(`{"volunteer_id":"`+volID+`"}`)).WithContext(ctx)
			h.HandleDeactivateRunner(rec, req)
		}},
		{"list-runners", func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/runners", nil).WithContext(ctx)
			h.HandleListRunners(rec, req)
		}},
		{"list-audits", func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/results", nil).WithContext(ctx)
			h.HandleListAudits(rec, req)
		}},
		{"flagged-leaves", func(h *AdminHandler, rec *httptest.ResponseRecorder, ctx context.Context) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/flagged-leaves", nil).WithContext(ctx)
			h.HandleFlaggedLeaves(rec, req)
		}},
	}

	// Both a missing caller (fails closed) and an explicit non-admin caller must 403.
	ctxs := []context.Context{
		context.Background(),
		WithCaller(context.Background(), Caller{IsAdmin: false}),
	}

	for _, rt := range routes {
		for _, ctx := range ctxs {
			runners := &fakeRunnersRepo{}
			audits := &fakeAuditsRepo{}
			h := testAdminHandler(runners, audits)
			rec := httptest.NewRecorder()
			rt.invoke(h, rec, ctx)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", rt.name, rec.Code)
			}
			if len(runners.registerCalls) != 0 || len(runners.deactivateCalls) != 0 {
				t.Errorf("%s: repo must not be called for a non-admin caller", rt.name)
			}
		}
	}
}

// --- register: validation ---------------------------------------------------------------

func TestHandleRegisterRunner_Validation(t *testing.T) {
	id := types.NewID().String()
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"bad volunteer id", `{"volunteer_id":"not-a-uuid","label":"x"}`},
		{"missing volunteer id", `{"label":"x"}`},
		{"missing label", `{"volunteer_id":"` + id + `"}`},
		{"blank label", `{"volunteer_id":"` + id + `","label":"   "}`},
		{"label too long", `{"volunteer_id":"` + id + `","label":"` + strings.Repeat("a", 129) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runners := &fakeRunnersRepo{}
			h := testAdminHandler(runners, &fakeAuditsRepo{})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners",
				strings.NewReader(tc.body)).WithContext(adminContext())
			rec := httptest.NewRecorder()
			h.HandleRegisterRunner(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if len(runners.registerCalls) != 0 {
				t.Errorf("Register must not be called on invalid input, got %d calls", len(runners.registerCalls))
			}
		})
	}
}

func TestHandleRegisterRunner_UnknownVolunteer(t *testing.T) {
	runners := &fakeRunnersRepo{err: ErrUnknownVolunteer}
	h := testAdminHandler(runners, &fakeAuditsRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners",
		strings.NewReader(`{"volunteer_id":"`+types.NewID().String()+`","label":"box-1"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleRegisterRunner(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown volunteer", rec.Code)
	}
}

func TestHandleRegisterRunner_Success(t *testing.T) {
	volID := types.NewID()
	runner := &Runner{ID: types.NewID(), VolunteerID: volID, Label: "box-1", Active: true}
	runners := &fakeRunnersRepo{runner: runner}
	h := testAdminHandler(runners, &fakeAuditsRepo{})

	// Leading/trailing whitespace must be trimmed before reaching the repo.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners",
		strings.NewReader(`{"volunteer_id":"`+volID.String()+`","label":"  box-1  ","note":"  head runner  "}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleRegisterRunner(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(runners.registerCalls) != 1 {
		t.Fatalf("Register calls = %d, want 1", len(runners.registerCalls))
	}
	call := runners.registerCalls[0]
	if call.volunteerID != volID {
		t.Errorf("volunteerID = %v, want %v", call.volunteerID, volID)
	}
	if call.label != "box-1" || call.note != "head runner" {
		t.Errorf("label/note = %q/%q, want trimmed box-1/head runner", call.label, call.note)
	}
	var got Runner
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != runner.ID || got.VolunteerID != volID {
		t.Errorf("response = %+v, want the registered row", got)
	}
}

// --- deactivate -------------------------------------------------------------------------

func TestHandleDeactivateRunner_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"bad volunteer id", `{"volunteer_id":"nope"}`},
		{"missing volunteer id", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runners := &fakeRunnersRepo{}
			h := testAdminHandler(runners, &fakeAuditsRepo{})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners/deactivate",
				strings.NewReader(tc.body)).WithContext(adminContext())
			rec := httptest.NewRecorder()
			h.HandleDeactivateRunner(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if len(runners.deactivateCalls) != 0 {
				t.Errorf("Deactivate must not be called on invalid input")
			}
		})
	}
}

func TestHandleDeactivateRunner_NotRegistered(t *testing.T) {
	runners := &fakeRunnersRepo{err: ErrNotRegistered}
	h := testAdminHandler(runners, &fakeAuditsRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners/deactivate",
		strings.NewReader(`{"volunteer_id":"`+types.NewID().String()+`"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleDeactivateRunner(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unregistered volunteer", rec.Code)
	}
}

func TestHandleDeactivateRunner_Success(t *testing.T) {
	volID := types.NewID()
	runners := &fakeRunnersRepo{}
	h := testAdminHandler(runners, &fakeAuditsRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit/runners/deactivate",
		strings.NewReader(`{"volunteer_id":"`+volID.String()+`"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleDeactivateRunner(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(runners.deactivateCalls) != 1 || runners.deactivateCalls[0] != volID {
		t.Fatalf("Deactivate calls = %v, want [%v]", runners.deactivateCalls, volID)
	}
}

// --- list runners -----------------------------------------------------------------------

func TestHandleListRunners_Success(t *testing.T) {
	runners := &fakeRunnersRepo{list: []*Runner{
		{ID: types.NewID(), VolunteerID: types.NewID(), Label: "box-1", Active: true},
	}}
	h := testAdminHandler(runners, &fakeAuditsRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/runners", nil).WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleListRunners(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []*Runner `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(resp.Data))
	}
}

// --- list audits: filter validation + wiring --------------------------------------------

func TestHandleListAudits_InvalidParams(t *testing.T) {
	leaf := types.NewID().String()
	for _, target := range []string{
		"/api/v1/admin/audit/results?status=BOGUS",
		"/api/v1/admin/audit/results?verdict=BOGUS",
		"/api/v1/admin/audit/results?leaf_id=not-a-uuid",
		"/api/v1/admin/audit/results?limit=0",
		"/api/v1/admin/audit/results?limit=-1",
		"/api/v1/admin/audit/results?limit=abc",
		"/api/v1/admin/audit/results?enforcement_state=BOGUS",
		"/api/v1/admin/audit/results?leaf_id=" + leaf + "&status=nope",
	} {
		audits := &fakeAuditsRepo{}
		h := testAdminHandler(&fakeRunnersRepo{}, audits)
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(adminContext())
		rec := httptest.NewRecorder()
		h.HandleListAudits(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}

func TestHandleListAudits_Success(t *testing.T) {
	leafID := types.NewID()
	audits := &fakeAuditsRepo{list: []*Audit{
		{ID: types.NewID(), LeafID: leafID, Status: StatusCompleted},
	}}
	h := testAdminHandler(&fakeRunnersRepo{}, audits)

	target := "/api/v1/admin/audit/results?status=COMPLETED&verdict=MISMATCH&enforcement_state=ENFORCED&leaf_id=" +
		leafID.String() + "&limit=99999"
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleListAudits(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// Enum + id filters parsed through; the over-cap limit is clamped.
	if audits.lastFilter.Status != StatusCompleted || audits.lastFilter.Verdict != VerdictMismatch {
		t.Errorf("filter status/verdict = %v/%v, want COMPLETED/MISMATCH",
			audits.lastFilter.Status, audits.lastFilter.Verdict)
	}
	if audits.lastFilter.EnforcementState != EnforcementEnforced {
		t.Errorf("filter enforcement_state = %v, want ENFORCED", audits.lastFilter.EnforcementState)
	}
	if audits.lastFilter.LeafID == nil || *audits.lastFilter.LeafID != leafID {
		t.Errorf("filter leaf_id = %v, want %v", audits.lastFilter.LeafID, leafID)
	}
	if audits.lastFilter.Limit != maxAuditsListLimit {
		t.Errorf("filter limit = %d, want clamped %d", audits.lastFilter.Limit, maxAuditsListLimit)
	}
	var resp struct {
		Data []*Audit `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(resp.Data))
	}
}

// --- flagged leaves ---------------------------------------------------------------------

func TestHandleFlaggedLeaves_Success(t *testing.T) {
	leafID := types.NewID()
	ownerID := types.NewID()
	audits := &fakeAuditsRepo{flagged: []FlaggedLeaf{
		{LeafID: leafID, OwnerID: &ownerID, EnforcedCount: 2, ContradictedCount: 1, StalledCount: 0},
	}}
	h := testAdminHandler(&fakeRunnersRepo{}, audits)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/flagged-leaves", nil).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleFlaggedLeaves(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []FlaggedLeaf `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(resp.Data))
	}
	got := resp.Data[0]
	if got.LeafID != leafID || got.OwnerID == nil || *got.OwnerID != ownerID {
		t.Errorf("leaf/owner = %v/%v, want %v/%v", got.LeafID, got.OwnerID, leafID, ownerID)
	}
	if got.EnforcedCount != 2 || got.ContradictedCount != 1 || got.StalledCount != 0 {
		t.Errorf("counts = %d/%d/%d, want 2/1/0",
			got.EnforcedCount, got.ContradictedCount, got.StalledCount)
	}
}

// HandleFlaggedLeaves returns an empty (non-null) array when nothing is flagged.
func TestHandleFlaggedLeaves_Empty(t *testing.T) {
	h := testAdminHandler(&fakeRunnersRepo{}, &fakeAuditsRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit/flagged-leaves", nil).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleFlaggedLeaves(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("empty flagged-leaves body = %s, want data:[]", rec.Body.String())
	}
}
