package workunit

import (
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// validTransitions defines the 12 valid state transitions for a work unit.
var validTransitions = map[WorkUnitState][]WorkUnitState{
	WorkUnitStateCreated:   {WorkUnitStateQueued},
	WorkUnitStateQueued:    {WorkUnitStateAssigned},
	WorkUnitStateAssigned:  {WorkUnitStateRunning, WorkUnitStateCompleted, WorkUnitStateExpired},
	WorkUnitStateRunning:   {WorkUnitStateCompleted, WorkUnitStateExpired},
	WorkUnitStateCompleted: {WorkUnitStateValidated, WorkUnitStateRejected},
	WorkUnitStateRejected:  {WorkUnitStateQueued, WorkUnitStateFailed},
	WorkUnitStateExpired:   {WorkUnitStateQueued, WorkUnitStateFailed},
	// WorkUnitStateValidated and WorkUnitStateFailed are terminal — no outbound transitions.
}

// ValidateTransition checks whether a state transition is allowed.
// Returns nil if the transition is valid, apierror.Conflict if invalid.
func ValidateTransition(from, to WorkUnitState) error {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return apierror.Conflict(
		fmt.Sprintf("invalid work unit state transition from %s to %s", from, to),
		map[string]string{
			"code": "INVALID_STATE_TRANSITION",
			"from": string(from),
			"to":   string(to),
		},
	)
}

// IsTerminalState returns true for VALIDATED and FAILED — no transitions out.
func IsTerminalState(state WorkUnitState) bool {
	return state == WorkUnitStateValidated || state == WorkUnitStateFailed
}

// TransitionToQueued handles REJECTED/EXPIRED → QUEUED business logic.
// Checks that reassignment_count < max_reassignments, increments the count,
// clears assignment fields, and sets priority to HIGH.
// Returns apierror.Conflict if max reassignments exceeded.
func TransitionToQueued(wu *WorkUnit) error {
	if wu.ReassignmentCount >= wu.MaxReassignments {
		return apierror.Conflict(
			fmt.Sprintf("max reassignments exceeded (%d/%d)", wu.ReassignmentCount, wu.MaxReassignments),
			map[string]string{
				"code":              "MAX_REASSIGNMENTS_EXCEEDED",
				"reassignment_count": fmt.Sprintf("%d", wu.ReassignmentCount),
				"max_reassignments":  fmt.Sprintf("%d", wu.MaxReassignments),
			},
		)
	}

	wu.State = WorkUnitStateQueued
	wu.ReassignmentCount++
	wu.Priority = WorkUnitPriorityHigh
	wu.AssignedVolunteerID = nil
	wu.AssignedAt = nil
	wu.StartedAt = nil
	wu.CompletedAt = nil
	wu.ValidatedAt = nil
	wu.LastHeartbeatAt = nil
	return nil
}

// TransitionToFailed sets state to FAILED and flags the work unit for review.
func TransitionToFailed(wu *WorkUnit) {
	wu.State = WorkUnitStateFailed
	wu.FlaggedForReview = true
}
