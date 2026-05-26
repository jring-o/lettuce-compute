package workunit

import (
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestValidateTransition(t *testing.T) {
	validCases := []struct {
		name string
		from WorkUnitState
		to   WorkUnitState
	}{
		{"CREATED → QUEUED", WorkUnitStateCreated, WorkUnitStateQueued},
		{"QUEUED → ASSIGNED", WorkUnitStateQueued, WorkUnitStateAssigned},
		{"ASSIGNED → RUNNING", WorkUnitStateAssigned, WorkUnitStateRunning},
		{"ASSIGNED → COMPLETED", WorkUnitStateAssigned, WorkUnitStateCompleted},
		{"ASSIGNED → EXPIRED", WorkUnitStateAssigned, WorkUnitStateExpired},
		{"RUNNING → COMPLETED", WorkUnitStateRunning, WorkUnitStateCompleted},
		{"RUNNING → EXPIRED", WorkUnitStateRunning, WorkUnitStateExpired},
		{"COMPLETED → VALIDATED", WorkUnitStateCompleted, WorkUnitStateValidated},
		{"COMPLETED → REJECTED", WorkUnitStateCompleted, WorkUnitStateRejected},
		{"REJECTED → QUEUED", WorkUnitStateRejected, WorkUnitStateQueued},
		{"REJECTED → FAILED", WorkUnitStateRejected, WorkUnitStateFailed},
		{"EXPIRED → QUEUED", WorkUnitStateExpired, WorkUnitStateQueued},
		{"EXPIRED → FAILED", WorkUnitStateExpired, WorkUnitStateFailed},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTransition(tc.from, tc.to); err != nil {
				t.Errorf("expected valid transition, got error: %v", err)
			}
		})
	}

	invalidCases := []struct {
		name string
		from WorkUnitState
		to   WorkUnitState
	}{
		{"QUEUED → COMPLETED", WorkUnitStateQueued, WorkUnitStateCompleted},
		{"VALIDATED → QUEUED", WorkUnitStateValidated, WorkUnitStateQueued},
		{"FAILED → QUEUED", WorkUnitStateFailed, WorkUnitStateQueued},
		{"CREATED → RUNNING", WorkUnitStateCreated, WorkUnitStateRunning},
		{"RUNNING → QUEUED", WorkUnitStateRunning, WorkUnitStateQueued},
		{"COMPLETED → QUEUED", WorkUnitStateCompleted, WorkUnitStateQueued},
		{"VALIDATED → FAILED", WorkUnitStateValidated, WorkUnitStateFailed},
		{"FAILED → VALIDATED", WorkUnitStateFailed, WorkUnitStateValidated},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransition(tc.from, tc.to)
			if err == nil {
				t.Error("expected error for invalid transition")
			}
			apiErr, ok := err.(*apierror.APIError)
			if !ok {
				t.Fatalf("expected *apierror.APIError, got %T", err)
			}
			if apiErr.HTTPStatus != 409 {
				t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
			}
		})
	}
}

func TestIsTerminalState(t *testing.T) {
	terminalCases := []struct {
		state    WorkUnitState
		terminal bool
	}{
		{WorkUnitStateCreated, false},
		{WorkUnitStateQueued, false},
		{WorkUnitStateAssigned, false},
		{WorkUnitStateRunning, false},
		{WorkUnitStateCompleted, false},
		{WorkUnitStateValidated, true},
		{WorkUnitStateRejected, false},
		{WorkUnitStateExpired, false},
		{WorkUnitStateFailed, true},
	}

	for _, tc := range terminalCases {
		t.Run(string(tc.state), func(t *testing.T) {
			got := IsTerminalState(tc.state)
			if got != tc.terminal {
				t.Errorf("IsTerminalState(%s) = %v, want %v", tc.state, got, tc.terminal)
			}
		})
	}
}

func TestTransitionToQueued(t *testing.T) {
	now := time.Now()
	volID := types.NewID()

	t.Run("success: increments count and clears fields", func(t *testing.T) {
		wu := &WorkUnit{
			State:               WorkUnitStateRejected,
			Priority:            WorkUnitPriorityNormal,
			ReassignmentCount:   1,
			MaxReassignments:    3,
			AssignedVolunteerID: &volID,
			AssignedAt:          &now,
			StartedAt:           &now,
			CompletedAt:         &now,
			LastHeartbeatAt:     &now,
		}

		err := TransitionToQueued(wu)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wu.State != WorkUnitStateQueued {
			t.Errorf("State = %s, want QUEUED", wu.State)
		}
		if wu.ReassignmentCount != 2 {
			t.Errorf("ReassignmentCount = %d, want 2", wu.ReassignmentCount)
		}
		if wu.Priority != WorkUnitPriorityHigh {
			t.Errorf("Priority = %s, want HIGH", wu.Priority)
		}
		if wu.AssignedVolunteerID != nil {
			t.Error("AssignedVolunteerID should be nil")
		}
		if wu.AssignedAt != nil {
			t.Error("AssignedAt should be nil")
		}
		if wu.StartedAt != nil {
			t.Error("StartedAt should be nil")
		}
		if wu.CompletedAt != nil {
			t.Error("CompletedAt should be nil")
		}
		if wu.LastHeartbeatAt != nil {
			t.Error("LastHeartbeatAt should be nil")
		}
	})

	t.Run("error: max reassignments exceeded", func(t *testing.T) {
		wu := &WorkUnit{
			State:             WorkUnitStateExpired,
			ReassignmentCount: 3,
			MaxReassignments:  3,
		}

		err := TransitionToQueued(wu)
		if err == nil {
			t.Fatal("expected error when max reassignments exceeded")
		}
		apiErr, ok := err.(*apierror.APIError)
		if !ok {
			t.Fatalf("expected *apierror.APIError, got %T", err)
		}
		if apiErr.HTTPStatus != 409 {
			t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
		}
	})

	t.Run("error: exactly at max", func(t *testing.T) {
		wu := &WorkUnit{
			State:             WorkUnitStateRejected,
			ReassignmentCount: 1,
			MaxReassignments:  1,
		}

		err := TransitionToQueued(wu)
		if err == nil {
			t.Fatal("expected error when reassignment_count equals max_reassignments")
		}
	})
}

func TestTransitionToFailed(t *testing.T) {
	wu := &WorkUnit{
		State:            WorkUnitStateRejected,
		FlaggedForReview: false,
	}

	TransitionToFailed(wu)

	if wu.State != WorkUnitStateFailed {
		t.Errorf("State = %s, want FAILED", wu.State)
	}
	if !wu.FlaggedForReview {
		t.Error("FlaggedForReview should be true")
	}
}
