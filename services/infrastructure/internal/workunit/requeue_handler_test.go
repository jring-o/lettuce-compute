package workunit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// stubRequeueRepo implements just the WorkUnitRepository methods handleRequeue
// uses in the per-copy model: GetByID, ExpireLiveCopies (abandon a unit's live
// copies), and Reassign. The embedded interface is nil, so any unexpected call
// panics — keeping the test honest about which methods the handler touches.
type stubRequeueRepo struct {
	WorkUnitRepository
	getByID       func(ctx context.Context, id types.ID) (*WorkUnit, error)
	reassign      func(ctx context.Context, id types.ID) (*WorkUnit, bool, error)
	expireCalls   int
	expireOutcome string
}

func (s *stubRequeueRepo) GetByID(ctx context.Context, id types.ID) (*WorkUnit, error) {
	return s.getByID(ctx, id)
}

// ExpireLiveCopies records the operator-requeue abandon of a unit's live copies
// (the per-copy replacement for closing the prior volunteer's active assignment row).
func (s *stubRequeueRepo) ExpireLiveCopies(_ context.Context, _ types.ID, outcome string) (int, error) {
	s.expireCalls++
	s.expireOutcome = outcome
	return 0, nil
}

func (s *stubRequeueRepo) Reassign(ctx context.Context, id types.ID) (*WorkUnit, bool, error) {
	return s.reassign(ctx, id)
}

func newRequeueRequest(t *testing.T, leafID, wuID types.ID) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/leafs/"+leafID.String()+"/work-units/"+wuID.String()+"/requeue", nil)
	req.SetPathValue("leaf_id", leafID.String())
	req.SetPathValue("work_unit_id", wuID.String())
	return httptest.NewRecorder(), req
}

func newRequeueHandler(repo WorkUnitRepository) *WorkUnitHandler {
	return NewWorkUnitHandler(repo, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestHandleRequeue_QueuedUnitAbandonsLiveCopies verifies the per-copy requeue of
// a QUEUED unit: it abandons the unit's in-flight copies and returns 200
// requeued=true while the unit stays QUEUED (units no longer reach ASSIGNED/RUNNING,
// so a fresh set of copies simply dispatches once the old ones are closed).
func TestHandleRequeue_QueuedUnitAbandonsLiveCopies(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			t.Fatal("Reassign must not be called for a QUEUED unit")
			return nil, false, nil
		},
	}

	rec, req := newRequeueRequest(t, leafID, wuID)
	newRequeueHandler(repo).HandleRequeue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.expireCalls != 1 {
		t.Errorf("ExpireLiveCopies called %d times, want 1", repo.expireCalls)
	}
	if repo.expireOutcome != string(assignment.OutcomeAbandoned) {
		t.Errorf("expire outcome = %q, want ABANDONED", repo.expireOutcome)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["requeued"] != true {
		t.Errorf("requeued = %v, want true", body["requeued"])
	}
	if body["state"] != string(WorkUnitStateQueued) {
		t.Errorf("state = %v, want QUEUED", body["state"])
	}
}

// TestHandleRequeue_ClosesLiveCopies verifies that requeuing a QUEUED unit closes
// (abandons) its live copies directly in work_unit_assignment_history — the
// per-copy replacement for closing the prior volunteer's open assignment-history
// row — so those volunteers are freed and stop counting toward redundancy.
func TestHandleRequeue_ClosesLiveCopies(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	volID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued, AssignedVolunteerID: &volID}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			t.Fatal("Reassign must not be called for a QUEUED unit")
			return nil, false, nil
		},
	}

	rec, req := newRequeueRequest(t, leafID, wuID)
	newRequeueHandler(repo).HandleRequeue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.expireCalls != 1 {
		t.Fatalf("ExpireLiveCopies called %d times, want 1", repo.expireCalls)
	}
	if repo.expireOutcome != string(assignment.OutcomeAbandoned) {
		t.Errorf("expire outcome = %q, want ABANDONED", repo.expireOutcome)
	}
}

// TestHandleRequeue_ExpiredUnitReassigned verifies an EXPIRED unit abandons any
// live copies (best-effort) and is then reassigned back to QUEUED.
func TestHandleRequeue_ExpiredUnitReassigned(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateExpired}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued}, true, nil
		},
	}

	rec, req := newRequeueRequest(t, leafID, wuID)
	newRequeueHandler(repo).HandleRequeue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.expireCalls != 1 {
		t.Errorf("ExpireLiveCopies called %d times, want 1", repo.expireCalls)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["requeued"] != true {
		t.Errorf("requeued = %v, want true", body["requeued"])
	}
	if body["state"] != string(WorkUnitStateQueued) {
		t.Errorf("state = %v, want QUEUED", body["state"])
	}
}

// TestHandleRequeue_WrongLeafReturns404 verifies a work unit that belongs to a
// different leaf is not requeuable through this leaf's path.
func TestHandleRequeue_WrongLeafReturns404(t *testing.T) {
	pathLeafID := types.NewID()
	otherLeafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: otherLeafID, State: WorkUnitStateQueued}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			t.Fatal("must not reassign a work unit from another leaf")
			return nil, false, nil
		},
	}

	rec, req := newRequeueRequest(t, pathLeafID, wuID)
	newRequeueHandler(repo).HandleRequeue(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if repo.expireCalls != 0 {
		t.Errorf("ExpireLiveCopies must not be called for a wrong-leaf unit, got %d", repo.expireCalls)
	}
}

// TestHandleRequeue_TerminalStateRejected verifies a COMPLETED unit cannot be
// requeued (it is neither QUEUED nor EXPIRED/REJECTED).
func TestHandleRequeue_TerminalStateRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateCompleted}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			t.Fatal("must not reassign a terminal unit")
			return nil, false, nil
		},
	}

	rec, req := newRequeueRequest(t, leafID, wuID)
	newRequeueHandler(repo).HandleRequeue(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if repo.expireCalls != 0 {
		t.Errorf("ExpireLiveCopies must not be called for a terminal unit, got %d", repo.expireCalls)
	}
}

// recordingInvalidator captures InvalidateWorkUnit calls (the PB-9 requeue →
// dispatch-cache invalidation seam).
type recordingInvalidator struct {
	calls []types.ID
}

func (r *recordingInvalidator) InvalidateWorkUnit(id types.ID) { r.calls = append(r.calls, id) }

// TestHandleRequeue_InvalidatesDispatchState (PB-9): the operator requeue must drop
// the unit's in-memory dispatch state via the wired invalidator — pre-fix the HTTP
// handler had no path to the dispatch cache at all, so a requeue 200 OK changed
// nothing about dispatch: the staged candidate kept its stale refill-time bench and
// contributor snapshots and the in-memory holds kept the unit excluded from refill.
func TestHandleRequeue_InvalidatesDispatchState(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	// QUEUED arm.
	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued}, nil
		},
	}
	inv := &recordingInvalidator{}
	h := newRequeueHandler(repo)
	h.SetDispatchInvalidator(inv)
	rec, req := newRequeueRequest(t, leafID, wuID)
	h.HandleRequeue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("QUEUED requeue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != wuID {
		t.Fatalf("QUEUED requeue invalidator calls = %v, want exactly [%s]", inv.calls, wuID)
	}

	// EXPIRED arm.
	repo = &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateExpired}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued}, true, nil
		},
	}
	inv = &recordingInvalidator{}
	h = newRequeueHandler(repo)
	h.SetDispatchInvalidator(inv)
	rec, req = newRequeueRequest(t, leafID, wuID)
	h.HandleRequeue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EXPIRED requeue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != wuID {
		t.Fatalf("EXPIRED requeue invalidator calls = %v, want exactly [%s]", inv.calls, wuID)
	}
}
