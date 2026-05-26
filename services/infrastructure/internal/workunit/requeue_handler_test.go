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
// uses. The embedded interface is nil, so any unexpected call panics — keeping
// the test honest about which methods the handler touches.
type stubRequeueRepo struct {
	WorkUnitRepository
	getByID    func(ctx context.Context, id types.ID) (*WorkUnit, error)
	expire     func(ctx context.Context, id types.ID) (*WorkUnit, error)
	reassign   func(ctx context.Context, id types.ID) (*WorkUnit, bool, error)
	expireCals int
}

func (s *stubRequeueRepo) GetByID(ctx context.Context, id types.ID) (*WorkUnit, error) {
	return s.getByID(ctx, id)
}

func (s *stubRequeueRepo) TransitionToExpired(ctx context.Context, id types.ID) (*WorkUnit, error) {
	s.expireCals++
	return s.expire(ctx, id)
}

func (s *stubRequeueRepo) Reassign(ctx context.Context, id types.ID) (*WorkUnit, bool, error) {
	return s.reassign(ctx, id)
}

// stubAssignRepo implements just the assignment.Repository methods handleRequeue
// uses. The embedded interface is nil, so any unexpected call panics.
type stubAssignRepo struct {
	assignment.Repository
	active         *assignment.AssignmentHistoryEntry
	findErr        error
	updateCalls    int
	updatedID      types.ID
	updatedOutcome assignment.AssignmentOutcome
}

func (s *stubAssignRepo) FindActiveByWorkUnitAndVolunteer(_ context.Context, _, _ types.ID) (*assignment.AssignmentHistoryEntry, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.active, nil
}

func (s *stubAssignRepo) UpdateOutcome(_ context.Context, id types.ID, outcome assignment.AssignmentOutcome, _ *types.ID) error {
	s.updateCalls++
	s.updatedID = id
	s.updatedOutcome = outcome
	return nil
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
	return NewWorkUnitHandler(repo, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestHandleRequeue_AssignedUnitIsExpiredThenReassigned verifies a stuck ASSIGNED
// unit is moved EXPIRED → QUEUED via the existing transition path.
func TestHandleRequeue_AssignedUnitIsExpiredThenReassigned(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateAssigned}, nil
		},
		expire: func(_ context.Context, id types.ID) (*WorkUnit, error) {
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
	if repo.expireCals != 1 {
		t.Errorf("TransitionToExpired called %d times, want 1", repo.expireCals)
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

// TestHandleRequeue_ClosesActiveAssignment verifies that requeuing an ASSIGNED
// unit closes the prior volunteer's open assignment-history row (outcome EXPIRED),
// so FindNextAssignable no longer permanently excludes that volunteer.
func TestHandleRequeue_ClosesActiveAssignment(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	volID := types.NewID()
	assignID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateAssigned, AssignedVolunteerID: &volID}, nil
		},
		expire: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateExpired, AssignedVolunteerID: &volID}, nil
		},
		reassign: func(_ context.Context, id types.ID) (*WorkUnit, bool, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateQueued}, true, nil
		},
	}
	assignRepo := &stubAssignRepo{
		active: &assignment.AssignmentHistoryEntry{ID: assignID, WorkUnitID: wuID, VolunteerID: volID},
	}

	h := newRequeueHandler(repo)
	h.SetAssignmentRepo(assignRepo)

	rec, req := newRequeueRequest(t, leafID, wuID)
	h.HandleRequeue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if assignRepo.updateCalls != 1 {
		t.Fatalf("UpdateOutcome called %d times, want 1", assignRepo.updateCalls)
	}
	if assignRepo.updatedID != assignID {
		t.Errorf("closed assignment id = %v, want %v", assignRepo.updatedID, assignID)
	}
	if assignRepo.updatedOutcome != assignment.OutcomeExpired {
		t.Errorf("outcome = %q, want EXPIRED", assignRepo.updatedOutcome)
	}
}

// TestHandleRequeue_ExpiredUnitSkipsExpireTransition verifies an already-EXPIRED
// unit is reassigned directly without a redundant TransitionToExpired.
func TestHandleRequeue_ExpiredUnitSkipsExpireTransition(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateExpired}, nil
		},
		expire: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			t.Fatal("TransitionToExpired should not be called for an already-EXPIRED unit")
			return nil, nil
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
	if repo.expireCals != 0 {
		t.Errorf("TransitionToExpired called %d times, want 0", repo.expireCals)
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
			return &WorkUnit{ID: wuID, LeafID: otherLeafID, State: WorkUnitStateAssigned}, nil
		},
		expire: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			t.Fatal("must not transition a work unit from another leaf")
			return nil, nil
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
}

// TestHandleRequeue_TerminalStateRejected verifies a COMPLETED unit cannot be
// requeued.
func TestHandleRequeue_TerminalStateRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	repo := &stubRequeueRepo{
		getByID: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			return &WorkUnit{ID: wuID, LeafID: leafID, State: WorkUnitStateCompleted}, nil
		},
		expire: func(_ context.Context, id types.ID) (*WorkUnit, error) {
			t.Fatal("must not expire a terminal unit")
			return nil, nil
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
}
