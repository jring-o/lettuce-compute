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
		// Per-copy model: a unit is a pure aggregate over its copies and can go
		// QUEUED → COMPLETED / FAILED directly (enough results accumulated, or the
		// dead-letter ceiling hit) without passing through ASSIGNED/RUNNING.
		{"QUEUED → COMPLETED", WorkUnitStateQueued, WorkUnitStateCompleted},
		{"QUEUED → FAILED", WorkUnitStateQueued, WorkUnitStateFailed},
		{"ASSIGNED → RUNNING", WorkUnitStateAssigned, WorkUnitStateRunning},
		{"ASSIGNED → COMPLETED", WorkUnitStateAssigned, WorkUnitStateCompleted},
		{"ASSIGNED → EXPIRED", WorkUnitStateAssigned, WorkUnitStateExpired},
		{"RUNNING → COMPLETED", WorkUnitStateRunning, WorkUnitStateCompleted},
		{"RUNNING → EXPIRED", WorkUnitStateRunning, WorkUnitStateExpired},
		{"COMPLETED → VALIDATED", WorkUnitStateCompleted, WorkUnitStateValidated},
		{"COMPLETED → REJECTED", WorkUnitStateCompleted, WorkUnitStateRejected},
		// The recovery REOPEN edge: a COMPLETED unit whose straggler copies all died
		// without submitting is demoted back to QUEUED (plain flip, no requeue business
		// logic) so dispatch can supply the missing corroborators. Taken only by the
		// transitioner's reopen arm.
		{"COMPLETED → QUEUED", WorkUnitStateCompleted, WorkUnitStateQueued},
		{"REJECTED → QUEUED", WorkUnitStateRejected, WorkUnitStateQueued},
		{"REJECTED → FAILED", WorkUnitStateRejected, WorkUnitStateFailed},
		{"EXPIRED → QUEUED", WorkUnitStateExpired, WorkUnitStateQueued},
		{"EXPIRED → FAILED", WorkUnitStateExpired, WorkUnitStateFailed},
		// The single audit-enforcement demotion edge (design doc §9.7 Q2-C): a VALIDATED
		// unit whose accepted output was refuted with nothing repairable is demoted so it
		// can requeue and revalidate honestly. Taken only by the enforcement pass.
		{"VALIDATED → REJECTED", WorkUnitStateValidated, WorkUnitStateRejected},
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
		// VALIDATED has exactly ONE outbound edge (→ REJECTED, above); every other exit
		// stays refused (design doc §9.10 viii).
		{"VALIDATED → QUEUED", WorkUnitStateValidated, WorkUnitStateQueued},
		{"VALIDATED → COMPLETED", WorkUnitStateValidated, WorkUnitStateCompleted},
		{"FAILED → QUEUED", WorkUnitStateFailed, WorkUnitStateQueued},
		{"CREATED → RUNNING", WorkUnitStateCreated, WorkUnitStateRunning},
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

	// Property 6 (uncapped requeue): TransitionToQueued no longer caps on
	// max_reassignments. A unit at or beyond its old cap still requeues; the only
	// terminal stop is the dead-letter ceiling (DeadLetterIfExhausted), enforced
	// where copies time out, not here.
	t.Run("uncapped: requeues even at/over max_reassignments", func(t *testing.T) {
		wu := &WorkUnit{
			State:             WorkUnitStateExpired,
			ReassignmentCount: 3,
			MaxReassignments:  3,
		}

		if err := TransitionToQueued(wu); err != nil {
			t.Fatalf("expected no error (requeue is uncapped), got %v", err)
		}
		if wu.State != WorkUnitStateQueued {
			t.Errorf("State = %s, want QUEUED", wu.State)
		}
		if wu.ReassignmentCount != 4 {
			t.Errorf("ReassignmentCount = %d, want 4 (bumped past old cap)", wu.ReassignmentCount)
		}
	})

	t.Run("uncapped: requeues at exactly old max", func(t *testing.T) {
		wu := &WorkUnit{
			State:             WorkUnitStateRejected,
			ReassignmentCount: 1,
			MaxReassignments:  1,
		}

		if err := TransitionToQueued(wu); err != nil {
			t.Fatalf("expected no error when count equals max (uncapped), got %v", err)
		}
		if wu.State != WorkUnitStateQueued {
			t.Errorf("State = %s, want QUEUED", wu.State)
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
