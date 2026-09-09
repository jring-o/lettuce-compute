package workunit

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Copy is one dispatched copy (attempt) of a work unit — the per-copy record that
// makes redundancy real: one independent instance of the work, run by one volunteer.
// It is a row of work_unit_assignment_history with the per-copy lease + deadline columns added in
// migration 00006. A work unit with redundancy N can have up to N live copies at
// once, each held by a DISTINCT volunteer, each with its own lease and deadline.
//
// Lifecycle (derived from the columns, see CopyState):
//   RESERVED : Outcome == nil, StartedAt == nil  (buffered in a volunteer's work
//              buffer; held until ReservedUntil, then reclaimed if never started)
//   RUNNING  : Outcome == nil, StartedAt != nil  (run-started; deadline clock =
//              StartedAt + DeadlineSeconds)
//   closed   : Outcome != nil  (COMPLETED / EXPIRED / ABANDONED / REJECTED)
type Copy struct {
	ID          types.ID
	WorkUnitID  types.ID
	VolunteerID types.ID
	// HostID attributes the copy to the MACHINE that holds it (TODO #19); nil = a volunteer
	// that reported no host (the per-account fallback). Carried so the per-host reliability
	// signal (TODO #54) can charge a timed-out / abandoned copy to the machine that wasted
	// it (keyed on COALESCE(host_id, volunteer_id)).
	HostID          *types.ID
	AssignedAt      time.Time
	ReservedUntil   *time.Time
	StartedAt       *time.Time
	DeadlineSeconds int
	Outcome         *string
	OutcomeAt       *time.Time
	ResultID        *types.ID
}

// ClosedCopy reports what CloseCopyByVolunteer actually wrote: the outcome the
// single write point honored (it downgrades a RETURNED request to ABANDONED when the
// copy had started, TB-35) — the one fact the SQL cooldown gate benches on
// (cooldownGuardSQL: a RETURNED give-back for its short re-offer throttle, any
// EXPIRED or ABANDONED copy for ~one deadline, TB-81), so the close's caller can
// mirror the gate in memory without re-reading the row (TB-40) — and the machine
// the copy was charged to (host_id; nil = the account reported no host), so an
// ABANDONED close can be recorded against that machine's reliability the way the
// deadline sweep records a timeout (TB-81).
type ClosedCopy struct {
	Outcome string
	HostID  *types.ID
}

// CopyState is the lifecycle phase of a copy.
type CopyState string

const (
	CopyStateReserved CopyState = "RESERVED"
	CopyStateRunning  CopyState = "RUNNING"
	CopyStateClosed   CopyState = "CLOSED"
)

// State returns the copy's lifecycle phase.
func (c *Copy) State() CopyState {
	if c.Outcome != nil {
		return CopyStateClosed
	}
	if c.StartedAt != nil {
		return CopyStateRunning
	}
	return CopyStateReserved
}

// copyColumns is the column list for SELECTs over copy rows.
const copyColumns = `id, work_unit_id, volunteer_id, host_id, assigned_at,
	reserved_until, started_at, deadline_seconds, outcome, outcome_at, result_id`
