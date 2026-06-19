package workunit

import (
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// validTransitions defines the valid state transitions for a work unit.
//
// Per-copy dispatch (migration 00006): a work unit's state is now a pure AGGREGATE
// over its copies. Run-start/timeout live on the per-copy rows, so a unit no longer
// transitions through ASSIGNED/RUNNING/EXPIRED itself — it sits QUEUED while up to
// redundancy copies run in parallel, then goes COMPLETED once enough results
// accumulate (or FAILED when it hits the dead-letter ceiling with redundancy unmet).
// The ASSIGNED/RUNNING/EXPIRED transitions are retained (inert) for backward
// compatibility with any legacy/manual paths and historical fixtures.
var validTransitions = map[WorkUnitState][]WorkUnitState{
	WorkUnitStateCreated:   {WorkUnitStateQueued},
	WorkUnitStateQueued:    {WorkUnitStateAssigned, WorkUnitStateCompleted, WorkUnitStateFailed},
	WorkUnitStateAssigned:  {WorkUnitStateRunning, WorkUnitStateCompleted, WorkUnitStateExpired, WorkUnitStateQueued},
	WorkUnitStateRunning:   {WorkUnitStateCompleted, WorkUnitStateExpired, WorkUnitStateQueued},
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

// TransitionToQueued handles REJECTED/EXPIRED → QUEUED business logic. It returns a
// unit to the dispatchable queue for further corroboration, bumps reassignment_count
// (kept for observability), clears the denormalized assignment fields, and raises
// priority so the requeued unit is picked up promptly.
//
// Property 6 (uncapped requeue): there is NO per-reassignment cap here. A unit is
// redispatched as many times as needed for the work to get done; the only ceiling is
// the dead-letter (max_total_copies), enforced where copies time out, not here.
func TransitionToQueued(wu *WorkUnit) error {
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
