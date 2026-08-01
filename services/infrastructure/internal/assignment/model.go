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
	// OutcomeReturned closes a copy the volunteer gave back UN-RUN because its work
	// buffer could not use it (the TB-32 arrival-time give-back, flagged on the wire as
	// unrun_giveback and verified un-started head-side — TB-35). Like SUPERSEDED it is
	// non-punitive, and it carries no information about the UNIT either, so it counts
	// toward NEITHER the dead-letter copy budget nor the error-copy cap (a batch tail
	// bounced by full buffers must never dead-letter a healthy unit). Its only teeth are
	// a short per-(unit, volunteer) re-offer cooldown so the same machine is not handed
	// the same unit right back.
	OutcomeReturned AssignmentOutcome = "RETURNED"
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
