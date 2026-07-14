package workunit

import (
	"fmt"
	"time"

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
	// COMPLETED -> QUEUED is the recovery REOPEN edge: a unit parked COMPLETED whose
	// straggler copies all died without submitting has dispatch headroom no dispatcher
	// can use (dispatch requires QUEUED), so the transitioner demotes it back to QUEUED
	// — a plain state flip that touches no results (the PENDING rows keep holding their
	// redundancy slots) — and dispatch supplies exactly the missing corroborators. No
	// requeue business logic runs on this edge (nothing was adjudicated).
	// COMPLETED -> FAILED mirrors DeadLetterIfExhausted's widened state guard (the raw
	// SQL dead-letter can now park a COMPLETED unit whose version-filtered pending set
	// sits below quorum with the budget spent); the chart stays honest about every edge
	// the database can take, exactly as QUEUED -> FAILED always was.
	WorkUnitStateCompleted: {WorkUnitStateValidated, WorkUnitStateRejected, WorkUnitStateQueued, WorkUnitStateFailed},
	WorkUnitStateRejected:  {WorkUnitStateQueued, WorkUnitStateFailed},
	WorkUnitStateExpired:   {WorkUnitStateQueued, WorkUnitStateFailed},
	// VALIDATED has exactly ONE outbound edge: the audit-enforcement demotion (design doc
	// §9.7 Q2-C) — a unit whose accepted output was refuted by trusted re-execution and
	// nothing was repairable is demoted so it can requeue and revalidate honestly. NOTE
	// this deliberately diverges from IsTerminalState, which still reports VALIDATED as
	// terminal: "terminal" means "the TRANSITIONER never re-decides it" (the
	// decideAndApply guard), not "no edge exists". Only the enforcement pass takes this
	// edge. WorkUnitStateFailed remains fully terminal.
	WorkUnitStateValidated: {WorkUnitStateRejected},
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

// IsTerminalState returns true for VALIDATED and FAILED. "Terminal" here means the
// TRANSITIONER never re-decides the unit (transition.decideAndApply no-ops on it) — NOT
// "no edge exists": validTransitions carries the single VALIDATED→REJECTED
// audit-enforcement demotion edge (design doc §9.7), taken only by the enforcement pass,
// never by the transitioner. Keep the two in deliberate divergence; gating new behavior
// on IsTerminalState alone must account for the enforcement edge.
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

// TransitionToValidated marks the unit VALIDATED (terminal) and stamps
// validated_at. completed_at is stamped earlier by MarkCompleted at the
// COMPLETED transition; this is the only place validated_at is set, so stats/
// health that read it (and the credit-ledger join) get a real timestamp rather
// than NULL.
func TransitionToValidated(wu *WorkUnit) {
	wu.State = WorkUnitStateValidated
	now := time.Now().UTC()
	wu.ValidatedAt = &now
}
