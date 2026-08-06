package workunit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TB-39 unit halves: the state chart's operator-revive edge and the HTTP handler's
// scoping/wiring. The transactional surgery itself is pinned by the integration tests
// (tb39_revive_deadlettered_test.go).

// TestTB39_StateChartAllowsOperatorReviveEdge: FAILED was "fully terminal" — no outbound
// edge at all — which is one of the three walls the filing documents (with the requeue
// refusal and the budget gate). The chart now carries the single deliberate
// FAILED → QUEUED operator edge, while FAILED stays terminal to the TRANSITIONER
// (IsTerminalState — the VALIDATED divergence discipline) and every other exit stays
// refused.
func TestTB39_StateChartAllowsOperatorReviveEdge(t *testing.T) {
	if err := ValidateTransition(WorkUnitStateFailed, WorkUnitStateQueued); err != nil {
		t.Fatalf("FAILED -> QUEUED refused (%v) — the dead-letter parking has no review verb (TB-39)", err)
	}
	// The edge is surgical: no other way out of FAILED.
	for _, to := range []WorkUnitState{
		WorkUnitStateAssigned, WorkUnitStateRunning, WorkUnitStateCompleted,
		WorkUnitStateValidated, WorkUnitStateRejected, WorkUnitStateExpired,
	} {
		if err := ValidateTransition(WorkUnitStateFailed, to); err == nil {
			t.Errorf("FAILED -> %s allowed, want refused", to)
		}
	}
	// And the transitioner still never re-decides a FAILED unit.
	if !IsTerminalState(WorkUnitStateFailed) {
		t.Error("IsTerminalState(FAILED) = false — the revive edge is operator-only, not a transitioner edge")
	}
}

// stubReviver records the ReviveDeadLettered call the handler makes.
type stubReviver struct {
	gotID     types.ID
	gotRefund bool
	gotTotal  int
	gotError  int
	out       *ReviveOutcome
	err       error
}

func (s *stubReviver) ReviveDeadLettered(_ context.Context, id types.ID, refund bool, freshTotal, freshError int) (*ReviveOutcome, error) {
	s.gotID, s.gotRefund, s.gotTotal, s.gotError = id, refund, freshTotal, freshError
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

func newReviveRequest(t *testing.T, leafID, wuID types.ID, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/leafs/"+leafID.String()+"/work-units/"+wuID.String()+"/revive", rdr)
	req.SetPathValue("leaf_id", leafID.String())
	req.SetPathValue("work_unit_id", wuID.String())
	return httptest.NewRecorder(), req
}

func newReviveHandler(repo WorkUnitRepository, rev Reviver, resolve func(context.Context, *WorkUnit) (int, int, error)) *WorkUnitHandler {
	h := NewWorkUnitHandler(repo, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetReviver(rev, resolve)
	return h
}

// TestHandleRevive_FailedUnitRevived: the happy path — 200, the outcome's counts in the
// body, and the PB-9 dispatch invalidation fired so the revived unit re-stages NOW.
func TestHandleRevive_FailedUnitRevived(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateFailed, FlaggedForReview: true}, nil
		},
	}
	rev := &stubReviver{out: &ReviveOutcome{ReclassifiedGivebacks: 2, ResurrectedResults: 1}}
	inv := &recordingInvalidator{}
	h := newReviveHandler(repo, rev, nil)
	h.SetDispatchInvalidator(inv)

	rec, req := newReviveRequest(t, leafID, wuID, "")
	h.HandleRevive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rev.gotID != wuID || rev.gotRefund {
		t.Errorf("reviver called with (id=%s refund=%v), want (%s, false)", rev.gotID, rev.gotRefund, wuID)
	}
	if len(inv.calls) != 1 || inv.calls[0] != wuID {
		t.Errorf("invalidator calls = %v, want exactly [%s] (PB-9)", inv.calls, wuID)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["revived"] != true || body["state"] != string(WorkUnitStateQueued) {
		t.Errorf("body = %v, want revived=true state=QUEUED", body)
	}
	if body["reclassified_givebacks"] != float64(2) || body["resurrected_results"] != float64(1) {
		t.Errorf("body counts = %v, want reclassified 2 / resurrected 1", body)
	}
}

// TestHandleRevive_RefundResolvesLeafBudgets: the override resolves the fresh ceilings
// (the enforcement demoter's resolution, injected) and passes them through.
func TestHandleRevive_RefundResolvesLeafBudgets(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateFailed}, nil
		},
	}
	rev := &stubReviver{out: &ReviveOutcome{Refunded: true}}
	h := newReviveHandler(repo, rev, func(_ context.Context, wu *WorkUnit) (int, int, error) {
		return 9, 3, nil
	})

	rec, req := newReviveRequest(t, leafID, wuID, `{"refund_real_failures": true}`)
	h.HandleRevive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !rev.gotRefund || rev.gotTotal != 9 || rev.gotError != 3 {
		t.Errorf("reviver called with (refund=%v total=%d error=%d), want (true, 9, 3)",
			rev.gotRefund, rev.gotTotal, rev.gotError)
	}
}

// TestHandleRevive_WrongLeafReturns404: a unit is only revivable through its own leaf's
// path (the handleGet/handleRequeue scoping rule).
func TestHandleRevive_WrongLeafReturns404(t *testing.T) {
	pathLeafID := types.NewID()
	otherLeafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: otherLeafID, State: WorkUnitStateFailed}, nil
		},
	}
	rev := &stubReviver{out: &ReviveOutcome{}}
	h := newReviveHandler(repo, rev, nil)

	rec, req := newReviveRequest(t, pathLeafID, wuID, "")
	h.HandleRevive(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if rev.gotID != (types.ID{}) {
		t.Error("reviver must not be called for a wrong-leaf unit")
	}
}

// TestHandleRevive_RefusalPassesThrough: the repository's refusals (wrong state, budget
// still exhausted) surface as their own 409s, operands intact.
func TestHandleRevive_RefusalPassesThrough(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateFailed}, nil
		},
	}
	rev := &stubReviver{err: apierror.Conflict("work unit's copy budget still reads exhausted",
		map[string]string{"code": "REVIVE_BUDGET_EXHAUSTED"})}
	h := newReviveHandler(repo, rev, nil)

	rec, req := newReviveRequest(t, leafID, wuID, "")
	h.HandleRevive(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "REVIVE_BUDGET_EXHAUSTED") {
		t.Errorf("refusal body must carry the code; got %s", rec.Body.String())
	}
}
