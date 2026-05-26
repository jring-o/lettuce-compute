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
)

// AssignmentHistoryEntry records a single assignment of a work unit to a volunteer.
type AssignmentHistoryEntry struct {
	ID          types.ID          `json:"id"`
	WorkUnitID  types.ID          `json:"work_unit_id"`
	VolunteerID types.ID          `json:"volunteer_id"`
	AssignedAt  time.Time         `json:"assigned_at"`
	Outcome     *AssignmentOutcome `json:"outcome,omitempty"`
	OutcomeAt   *time.Time        `json:"outcome_at,omitempty"`
	ResultID    *types.ID         `json:"result_id,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}
