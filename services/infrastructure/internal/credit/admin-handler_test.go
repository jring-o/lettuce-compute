package credit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- test doubles -----------------------------------------------------------------------

type clawbackCall struct {
	entryID   types.ID
	magnitude *float64
	reason    string
	note      string
	createdBy string
}

// fakeAdjRepo is an in-memory AdjustmentsRepository double recording calls.
type fakeAdjRepo struct {
	adj  *Adjustment
	err  error
	list []*Adjustment

	clawbackCalls []clawbackCall
	lastLimit     int
	lastOffset    int
}

func (f *fakeAdjRepo) Clawback(_ context.Context, entryID types.ID, magnitude *float64, reason, note, createdBy string) (*Adjustment, error) {
	f.clawbackCalls = append(f.clawbackCalls, clawbackCall{entryID, magnitude, reason, note, createdBy})
	if f.err != nil {
		return nil, f.err
	}
	return f.adj, nil
}

func (f *fakeAdjRepo) ListByVolunteer(_ context.Context, _ types.ID, limit, offset int) ([]*Adjustment, error) {
	f.lastLimit, f.lastOffset = limit, offset
	return f.list, f.err
}

func (f *fakeAdjRepo) SumForEntry(_ context.Context, _ types.ID) (float64, error) {
	return 0, f.err
}

func (f *fakeAdjRepo) ClawbackForAudit(_ context.Context, _, _ types.ID, _ string) (*Adjustment, error) {
	return nil, f.err
}

func (f *fakeAdjRepo) ListUnmaturedEntryIDs(_ context.Context, _ types.ID, _ int) ([]types.ID, error) {
	return nil, f.err
}

// fakeLedgerRepo is an in-memory Repository double; only GetByResultID is exercised here.
type fakeLedgerRepo struct {
	entry    *LedgerEntry
	getErr   error
	getCalls []types.ID
}

func (f *fakeLedgerRepo) Create(context.Context, *LedgerEntry) error { return nil }

func (f *fakeLedgerRepo) GetByResultID(_ context.Context, resultID types.ID) (*LedgerEntry, error) {
	f.getCalls = append(f.getCalls, resultID)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.entry, nil
}

func (f *fakeLedgerRepo) SumByVolunteerProject(context.Context, types.ID, types.ID) (float64, error) {
	return 0, nil
}

func (f *fakeLedgerRepo) CountByVolunteerPerProject(context.Context, types.ID) (map[types.ID]int, error) {
	return nil, nil
}

func (f *fakeLedgerRepo) ListByVolunteer(context.Context, types.ID, types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

func (f *fakeLedgerRepo) ListByLeaf(context.Context, types.ID, types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

func testAdminHandler(adj AdjustmentsRepository, ledger Repository) *AdminHandler {
	return NewAdminHandler(adj, ledger, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// adminContext returns a context carrying an admin caller.
func adminContext() context.Context {
	return WithCaller(context.Background(), Caller{IsAdmin: true})
}

func floatPtr(v float64) *float64 { return &v }

// --- clawback: authorization (audit F2) -------------------------------------------------

func TestHandleClawback_NonAdminForbidden(t *testing.T) {
	adj := &fakeAdjRepo{}
	ledger := &fakeLedgerRepo{}
	h := testAdminHandler(adj, ledger)

	// No caller injected at all (fails closed) and an explicit non-admin caller.
	for _, ctx := range []context.Context{
		context.Background(),
		WithCaller(context.Background(), Caller{IsAdmin: false}),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
			strings.NewReader(`{"ledger_entry_id":"`+types.NewID().String()+`","reason":"OPERATOR_CLAWBACK"}`)).
			WithContext(ctx)
		rec := httptest.NewRecorder()
		h.HandleClawback(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-admin HandleClawback: status = %d, want 403", rec.Code)
		}
	}
	if len(adj.clawbackCalls) != 0 {
		t.Errorf("Clawback should not be called for non-admin, got %d calls", len(adj.clawbackCalls))
	}
}

func TestHandleListAdjustments_NonAdminForbidden(t *testing.T) {
	adj := &fakeAdjRepo{}
	h := testAdminHandler(adj, &fakeLedgerRepo{})
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/credit/adjustments?volunteer_id="+types.NewID().String(), nil) // no caller
	rec := httptest.NewRecorder()
	h.HandleListAdjustments(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// --- clawback: input validation (audit F9) ----------------------------------------------

func TestHandleClawback_Validation(t *testing.T) {
	id := types.NewID().String()
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"both ids", `{"result_id":"` + id + `","ledger_entry_id":"` + id + `","reason":"x"}`},
		{"neither id", `{"reason":"x"}`},
		{"amount zero", `{"ledger_entry_id":"` + id + `","amount":0,"reason":"x"}`},
		{"amount negative", `{"ledger_entry_id":"` + id + `","amount":-1.5,"reason":"x"}`},
		{"amount NaN", `{"ledger_entry_id":"` + id + `","amount":NaN,"reason":"x"}`},
		{"missing reason", `{"ledger_entry_id":"` + id + `"}`},
		{"blank reason", `{"ledger_entry_id":"` + id + `","reason":"   "}`},
		{"reason too long", `{"ledger_entry_id":"` + id + `","reason":"` + strings.Repeat("a", 65) + `"}`},
		{"bad entry id", `{"ledger_entry_id":"not-a-uuid","reason":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adj := &fakeAdjRepo{}
			h := testAdminHandler(adj, &fakeLedgerRepo{})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
				strings.NewReader(tc.body)).WithContext(adminContext())
			rec := httptest.NewRecorder()
			h.HandleClawback(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if len(adj.clawbackCalls) != 0 {
				t.Errorf("Clawback should not be called on invalid input, got %d calls", len(adj.clawbackCalls))
			}
		})
	}
}

// --- clawback: repo error mapping -------------------------------------------------------

func TestHandleClawback_Exhausted(t *testing.T) {
	adj := &fakeAdjRepo{err: ErrAdjustmentExhausted}
	h := testAdminHandler(adj, &fakeLedgerRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+types.NewID().String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already fully adjusted") {
		t.Errorf("body = %s, want distinct exhausted message", rec.Body.String())
	}
}

func TestHandleClawback_Overshoot(t *testing.T) {
	adj := &fakeAdjRepo{err: ErrAdjustmentOvershoot}
	h := testAdminHandler(adj, &fakeLedgerRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+types.NewID().String()+`","amount":9999,"reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "exceeds the entry's remaining credit") {
		t.Errorf("body = %s, want distinct overshoot message", rec.Body.String())
	}
}

// The exhausted and overshoot conflicts must carry DISTINCT messages so a runbook can tell
// "nothing left to claw back" apart from "you asked for too much".
func TestHandleClawback_ConflictMessagesDistinct(t *testing.T) {
	entry := types.NewID().String()
	body := func() string {
		return `{"ledger_entry_id":"` + entry + `","reason":"OPERATOR_CLAWBACK"}`
	}

	recFor := func(err error) *httptest.ResponseRecorder {
		h := testAdminHandler(&fakeAdjRepo{err: err}, &fakeLedgerRepo{})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
			strings.NewReader(body())).WithContext(adminContext())
		rec := httptest.NewRecorder()
		h.HandleClawback(rec, req)
		return rec
	}

	if recFor(ErrAdjustmentExhausted).Body.String() == recFor(ErrAdjustmentOvershoot).Body.String() {
		t.Error("exhausted and overshoot conflicts must have distinct messages")
	}
}

// --- clawback: success ------------------------------------------------------------------

func TestHandleClawback_SuccessByEntryID(t *testing.T) {
	entryID := types.NewID()
	volID := types.NewID()
	adj := &fakeAdjRepo{adj: &Adjustment{
		ID:            types.NewID(),
		LedgerEntryID: entryID,
		VolunteerID:   volID,
		LeafID:        types.NewID(),
		Amount:        -1.5,
		Reason:        "OPERATOR_CLAWBACK",
		CreatedBy:     AdjustmentByOperator,
		CreatedAt:     time.Now().UTC(),
	}}
	h := testAdminHandler(adj, &fakeLedgerRepo{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+entryID.String()+`","amount":1.5,"reason":"OPERATOR_CLAWBACK","note":"chargeback"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(adj.clawbackCalls) != 1 {
		t.Fatalf("Clawback calls = %d, want 1", len(adj.clawbackCalls))
	}
	call := adj.clawbackCalls[0]
	if call.entryID != entryID {
		t.Errorf("entryID = %v, want %v", call.entryID, entryID)
	}
	if call.magnitude == nil || *call.magnitude != 1.5 {
		t.Errorf("magnitude = %v, want 1.5 (positive magnitude passed through)", call.magnitude)
	}
	if call.createdBy != AdjustmentByOperator {
		t.Errorf("createdBy = %q, want %q", call.createdBy, AdjustmentByOperator)
	}
	if call.reason != "OPERATOR_CLAWBACK" || call.note != "chargeback" {
		t.Errorf("reason/note = %q/%q, unexpected", call.reason, call.note)
	}

	var got Adjustment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LedgerEntryID != entryID || got.Amount != -1.5 {
		t.Errorf("response = %+v, want entry=%v amount=-1.5", got, entryID)
	}
}

// amount omitted => full remaining => magnitude nil reaches the repo.
func TestHandleClawback_FullRemainingDefault(t *testing.T) {
	entryID := types.NewID()
	adj := &fakeAdjRepo{adj: &Adjustment{
		ID: types.NewID(), LedgerEntryID: entryID, VolunteerID: types.NewID(),
		LeafID: types.NewID(), Amount: -3.0, Reason: "OPERATOR_CLAWBACK",
		CreatedBy: AdjustmentByOperator, CreatedAt: time.Now().UTC(),
	}}
	h := testAdminHandler(adj, &fakeLedgerRepo{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+entryID.String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(adj.clawbackCalls) != 1 || adj.clawbackCalls[0].magnitude != nil {
		t.Fatalf("magnitude = %v, want nil (full remaining)", adj.clawbackCalls[0].magnitude)
	}
}

// --- clawback: result_id resolution -----------------------------------------------------

func TestHandleClawback_ResultIDResolved(t *testing.T) {
	resultID := types.NewID()
	entryID := types.NewID()
	ledger := &fakeLedgerRepo{entry: &LedgerEntry{ID: entryID, ResultID: resultID}}
	adj := &fakeAdjRepo{adj: &Adjustment{
		ID: types.NewID(), LedgerEntryID: entryID, VolunteerID: types.NewID(),
		LeafID: types.NewID(), Amount: -1.0, Reason: "OPERATOR_CLAWBACK",
		CreatedBy: AdjustmentByOperator, CreatedAt: time.Now().UTC(),
	}}
	h := testAdminHandler(adj, ledger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"result_id":"`+resultID.String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(ledger.getCalls) != 1 || ledger.getCalls[0] != resultID {
		t.Fatalf("GetByResultID calls = %v, want [%v]", ledger.getCalls, resultID)
	}
	if len(adj.clawbackCalls) != 1 || adj.clawbackCalls[0].entryID != entryID {
		t.Fatalf("Clawback entryID = %v, want the resolved %v", adj.clawbackCalls, entryID)
	}
}

func TestHandleClawback_ResultIDNotFound(t *testing.T) {
	resultID := types.NewID()
	ledger := &fakeLedgerRepo{getErr: apierror.NotFound("credit_ledger_entry", resultID.String())}
	adj := &fakeAdjRepo{}
	h := testAdminHandler(adj, ledger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"result_id":"`+resultID.String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if len(adj.clawbackCalls) != 0 {
		t.Errorf("Clawback should not be called when the result has no ledger entry")
	}
}

// --- list -------------------------------------------------------------------------------

func TestHandleListAdjustments_MissingVolunteerID(t *testing.T) {
	h := testAdminHandler(&fakeAdjRepo{}, &fakeLedgerRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/credit/adjustments", nil).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleListAdjustments(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleListAdjustments_InvalidVolunteerID(t *testing.T) {
	h := testAdminHandler(&fakeAdjRepo{}, &fakeLedgerRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/credit/adjustments?volunteer_id=nope", nil).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleListAdjustments(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleListAdjustments_Success(t *testing.T) {
	volID := types.NewID()
	adj := &fakeAdjRepo{list: []*Adjustment{
		{ID: types.NewID(), LedgerEntryID: types.NewID(), VolunteerID: volID, Amount: -1.5, Reason: "OPERATOR_CLAWBACK"},
	}}
	h := testAdminHandler(adj, &fakeLedgerRepo{})

	// Default limit/offset.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/credit/adjustments?volunteer_id="+volID.String(), nil).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleListAdjustments(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if adj.lastLimit != defaultAdjustmentListLimit || adj.lastOffset != 0 {
		t.Errorf("limit/offset = %d/%d, want %d/0", adj.lastLimit, adj.lastOffset, defaultAdjustmentListLimit)
	}
	var resp struct {
		Data []*Adjustment `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(resp.Data))
	}

	// Over-cap limit is clamped; offset honored.
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/credit/adjustments?volunteer_id="+volID.String()+"&limit=99999&offset=7", nil).
		WithContext(adminContext())
	rec = httptest.NewRecorder()
	h.HandleListAdjustments(rec, req)
	if adj.lastLimit != maxAdjustmentListLimit || adj.lastOffset != 7 {
		t.Errorf("limit/offset = %d/%d, want %d/7", adj.lastLimit, adj.lastOffset, maxAdjustmentListLimit)
	}
}

func TestHandleListAdjustments_InvalidParams(t *testing.T) {
	volID := types.NewID().String()
	h := testAdminHandler(&fakeAdjRepo{}, &fakeLedgerRepo{})
	for _, target := range []string{
		"/api/v1/admin/credit/adjustments?volunteer_id=" + volID + "&limit=0",
		"/api/v1/admin/credit/adjustments?volunteer_id=" + volID + "&limit=-1",
		"/api/v1/admin/credit/adjustments?volunteer_id=" + volID + "&limit=abc",
		"/api/v1/admin/credit/adjustments?volunteer_id=" + volID + "&offset=-1",
		"/api/v1/admin/credit/adjustments?volunteer_id=" + volID + "&offset=xyz",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(adminContext())
		rec := httptest.NewRecorder()
		h.HandleListAdjustments(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}

// --- clawback: reason charset (audit F-M2) ----------------------------------------------

// A reason that is not an uppercase machine code is rejected 400 before any repository call —
// free text would leak Go-HTML-escaped bytes into the signed revocation payload. An
// OPERATOR_CLAWBACK-shaped code (uppercase, digits, underscore) is accepted.
func TestHandleClawback_ReasonCharset(t *testing.T) {
	entryID := types.NewID()
	okAdj := &Adjustment{
		ID: types.NewID(), LedgerEntryID: entryID, VolunteerID: types.NewID(),
		LeafID: types.NewID(), Amount: -1.0, Reason: "OPERATOR_CLAWBACK",
		CreatedBy: AdjustmentByOperator, CreatedAt: time.Now().UTC(),
	}
	cases := []struct {
		name     string
		reason   string
		wantCode int
	}{
		{"lowercase rejected", "operator_clawback", http.StatusBadRequest},
		{"ampersand rejected", "FRAUD&X", http.StatusBadRequest},
		{"angle bracket rejected", "FRAUD<X", http.StatusBadRequest},
		{"space rejected", "OPERATOR CLAWBACK", http.StatusBadRequest},
		{"65 chars rejected", strings.Repeat("A", 65), http.StatusBadRequest},
		{"machine code accepted", "OPERATOR_CLAWBACK", http.StatusCreated},
		{"digits underscore accepted", "SLASH_2", http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adj := &fakeAdjRepo{adj: okAdj}
			h := testAdminHandler(adj, &fakeLedgerRepo{})
			body := `{"ledger_entry_id":"` + entryID.String() + `","reason":"` + tc.reason + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
				strings.NewReader(body)).WithContext(adminContext())
			rec := httptest.NewRecorder()
			h.HandleClawback(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("reason %q: status = %d, want %d (body %s)", tc.reason, rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusBadRequest && len(adj.clawbackCalls) != 0 {
				t.Errorf("reason %q: Clawback must not be called on a rejected reason", tc.reason)
			}
		})
	}
}

// --- clawback: revocation emission ------------------------------------------------------

// fakeRevEmitter records EmitForAdjustment calls and optionally fails them.
type fakeRevEmitter struct {
	calls []types.ID
	err   error
}

func (f *fakeRevEmitter) EmitForAdjustment(_ context.Context, adjustmentID types.ID) error {
	f.calls = append(f.calls, adjustmentID)
	return f.err
}

func TestHandleClawback_EmitsRevocationOnSuccess(t *testing.T) {
	adjID := types.NewID()
	entryID := types.NewID()
	adj := &fakeAdjRepo{adj: &Adjustment{
		ID: adjID, LedgerEntryID: entryID, VolunteerID: types.NewID(),
		LeafID: types.NewID(), Amount: -1.0, Reason: "OPERATOR_CLAWBACK",
		CreatedBy: AdjustmentByOperator, CreatedAt: time.Now().UTC(),
	}}
	emitter := &fakeRevEmitter{}
	h := testAdminHandler(adj, &fakeLedgerRepo{}).WithRevocationEmitter(emitter)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+entryID.String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(emitter.calls) != 1 || emitter.calls[0] != adjID {
		t.Fatalf("EmitForAdjustment calls = %v, want [%v]", emitter.calls, adjID)
	}
}

// A revocation-emission failure never fails the committed clawback: the endpoint still returns
// 201 (the reconciliation sweep recovers the missing attestation).
func TestHandleClawback_EmissionFailureStill201(t *testing.T) {
	adjID := types.NewID()
	entryID := types.NewID()
	adj := &fakeAdjRepo{adj: &Adjustment{
		ID: adjID, LedgerEntryID: entryID, VolunteerID: types.NewID(),
		LeafID: types.NewID(), Amount: -1.0, Reason: "OPERATOR_CLAWBACK",
		CreatedBy: AdjustmentByOperator, CreatedAt: time.Now().UTC(),
	}}
	emitter := &fakeRevEmitter{err: errors.New("signer unavailable")}
	h := testAdminHandler(adj, &fakeLedgerRepo{}).WithRevocationEmitter(emitter)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit/adjustments",
		strings.NewReader(`{"ledger_entry_id":"`+entryID.String()+`","reason":"OPERATOR_CLAWBACK"}`)).
		WithContext(adminContext())
	rec := httptest.NewRecorder()
	h.HandleClawback(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 despite emission failure (body %s)", rec.Code, rec.Body.String())
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForAdjustment calls = %d, want 1", len(emitter.calls))
	}
}
