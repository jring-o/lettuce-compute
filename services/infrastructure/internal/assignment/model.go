package assignment

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// AssignmentOutcome represents the outcome of a work unit assignment.
type AssignmentOutcome string

const (
	OutcomeCompleted AssignmentOutcome = "COMPLETED"
	OutcomeExpired   AssignmentOutcome = "EXPIRED"
	OutcomeAbandoned AssignmentOutcome = "ABANDONED"
	OutcomeRejected  AssignmentOutcome = "REJECTED"
	// OutcomeSuperseded closes an extra in-flight copy NON-PUNITIVELY when the unit validated
	// at quorum before the copy finished (target_copies > min_quorum, TODO #50). Unlike
	// EXPIRED/ABANDONED it carries no bad reliability signal — the work was superseded, not
	// failed. Never written for a target == quorum leaf (no extras exist at validation).
	OutcomeSuperseded AssignmentOutcome = "SUPERSEDED"
)

// AssignmentHistoryEntry records a single assignment of a work unit to a volunteer.
type AssignmentHistoryEntry struct {
	ID          types.ID           `json:"id"`
	WorkUnitID  types.ID           `json:"work_unit_id"`
	VolunteerID types.ID           `json:"volunteer_id"`
	AssignedAt  time.Time          `json:"assigned_at"`
	Outcome     *AssignmentOutcome `json:"outcome,omitempty"`
	OutcomeAt   *time.Time         `json:"outcome_at,omitempty"`
	ResultID    *types.ID          `json:"result_id,omitempty"`
	// HostID attributes the copy to the MACHINE that produced it (TODO #19). nil = a
	// volunteer that reported no host (per-account fallback). Stamped at reservation;
	// the result row copies it so per-machine attribution is queryable end-to-end.
	HostID    *types.ID `json:"host_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
