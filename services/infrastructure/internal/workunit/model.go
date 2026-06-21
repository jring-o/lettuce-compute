package workunit

import (
	"encoding/json"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// WorkUnitState represents the lifecycle state of a work unit.
type WorkUnitState string

const (
	WorkUnitStateCreated   WorkUnitState = "CREATED"
	WorkUnitStateQueued    WorkUnitState = "QUEUED"
	WorkUnitStateAssigned  WorkUnitState = "ASSIGNED"
	WorkUnitStateRunning   WorkUnitState = "RUNNING"
	WorkUnitStateCompleted WorkUnitState = "COMPLETED"
	WorkUnitStateValidated WorkUnitState = "VALIDATED"
	WorkUnitStateRejected  WorkUnitState = "REJECTED"
	WorkUnitStateExpired   WorkUnitState = "EXPIRED"
	WorkUnitStateFailed    WorkUnitState = "FAILED"
)

// WorkUnitPriority controls assignment ordering.
type WorkUnitPriority string

const (
	WorkUnitPriorityNormal   WorkUnitPriority = "NORMAL"
	WorkUnitPriorityHigh     WorkUnitPriority = "HIGH"
	WorkUnitPriorityCritical WorkUnitPriority = "CRITICAL"
)

// WorkUnit is the atom of distributed computation — a single piece of work
// that a volunteer executes, with inputs, parameters, and a deadline.
type WorkUnit struct {
	ID                       types.ID         `json:"id"`
	LeafID                   types.ID         `json:"leaf_id"`
	BatchID                  *types.ID        `json:"batch_id"`
	State                    WorkUnitState    `json:"state"`
	Priority                 WorkUnitPriority `json:"priority"`
	InputData                json.RawMessage  `json:"input_data,omitempty"`
	InputDataRef             *string          `json:"input_data_ref,omitempty"`
	CodeArtifactRef          string           `json:"code_artifact_ref"`
	Parameters               json.RawMessage  `json:"parameters,omitempty"`
	EstimatedDurationSeconds *int             `json:"estimated_duration_seconds,omitempty"`
	DeadlineSeconds          int              `json:"deadline_seconds"`
	OutputSpec               json.RawMessage  `json:"output_spec,omitempty"`
	AssignedVolunteerID      *types.ID        `json:"assigned_volunteer_id,omitempty"`
	AssignedAt               *time.Time       `json:"assigned_at,omitempty"`
	StartedAt                *time.Time       `json:"started_at,omitempty"`
	CompletedAt              *time.Time       `json:"completed_at,omitempty"`
	ValidatedAt              *time.Time       `json:"validated_at,omitempty"`
	ReassignmentCount        int              `json:"reassignment_count"`
	MaxReassignments         int              `json:"max_reassignments"`
	// MaxTotalCopies is the head-owned dead-letter ceiling (property 6): the total
	// number of copies (dispatch attempts) ever created for this unit before, if its
	// redundancy is still unmet and no copy is live, it is parked FAILED. 0 = derive a
	// default from the leaf's redundancy_factor (EffectiveMaxTotalCopies). Floor only,
	// no upper cap — a timed-out copy is otherwise redispatched without per-attempt cap.
	MaxTotalCopies           int              `json:"max_total_copies"`
	// TargetCopies / MinQuorum / MaxErrorCopies / MaxSuccessCopies are the explicit
	// redundancy knobs (TODO #50, migration 00010), stamped per-unit at generation like
	// MaxTotalCopies. 0 means "derive from the leaf's validation_config exactly as today":
	//   TargetCopies      0 -> redundancy_factor   (how many copies to dispatch)
	//   MinQuorum         0 -> redundancy_factor   (how many agreeing results validate)
	//   MaxErrorCopies    0 -> unlimited           (only MaxTotalCopies bounds errors)
	//   MaxSuccessCopies  0 -> TargetCopies         (dispatch stops at target)
	// The Effective* helpers below resolve the 0 sentinel; all redundancy arithmetic in
	// the head reads these via the transition.RedundancyPolicy, never re-derives them.
	TargetCopies             int              `json:"target_copies"`
	MinQuorum                int              `json:"min_quorum"`
	MaxErrorCopies           int              `json:"max_error_copies"`
	MaxSuccessCopies         int              `json:"max_success_copies"`
	LastHeartbeatAt          *time.Time       `json:"last_heartbeat_at,omitempty"`
	FlaggedForReview         bool             `json:"flagged_for_review"`
	SpotCheck                bool             `json:"spot_check"`
	// HRClass is the Homogeneous-Redundancy hardware class this unit is pinned to. It is
	// NULL until the first copy is dispatched; when the leaf enables homogeneous_redundancy
	// the first hand-out stamps it (first-writer-wins) with the holder's class (CPU vendor +
	// OS + arch), and every later copy is then restricted to volunteers of that same class so
	// redundant results are bit-comparable even for non-portably-deterministic engines.
	// NULL = unpinned / HR disabled (no class restriction).
	HRClass                  *string          `json:"hr_class,omitempty"`
	LastCheckpointAt         *time.Time       `json:"last_checkpoint_at,omitempty"`
	LastCheckpointSequence   int              `json:"last_checkpoint_sequence"`
	// ReservedUntil / ReservedVolunteerID are TRANSIENT, not DB columns (the single
	// per-unit reservation columns were retired in migration 00006 in favor of
	// per-copy rows). HandOut populates them on the unit copy it returns so the proto
	// assignment can echo reserved_until_unix (the buffered copy's lease window). They
	// are never scanned from or written to work_units. Nil except on a hand-out echo.
	ReservedUntil            *time.Time       `json:"reserved_until,omitempty"`
	ReservedVolunteerID      *types.ID        `json:"reserved_volunteer_id,omitempty"`
	CreatedAt                time.Time        `json:"created_at"`
	UpdatedAt                time.Time        `json:"updated_at"`
}

// EffectiveMaxTotalCopies returns the dead-letter ceiling for this unit: the
// configured MaxTotalCopies if positive, else a derived default of redundancy + a
// retry margin so honest timeouts redispatch freely while a hopeless (poison) unit
// still eventually parks. redundancy is the leaf's effective redundancy_factor.
func (wu *WorkUnit) EffectiveMaxTotalCopies(redundancy int) int {
	if wu.MaxTotalCopies > 0 {
		return wu.MaxTotalCopies
	}
	if redundancy < 1 {
		redundancy = 1
	}
	// redundancy + 6: the redundancy target plus a retry margin.
	return redundancy + defaultCopyRetryMargin
}

// defaultCopyRetryMargin is how many timed-out/failed copies above the redundancy
// target a unit tolerates before dead-lettering, when MaxTotalCopies is unset.
const defaultCopyRetryMargin = 6

// Batch groups work units within a leaf for progress tracking.
type Batch struct {
	ID                 types.ID  `json:"id"`
	LeafID             types.ID  `json:"leaf_id"`
	SequenceNumber     int       `json:"sequence_number"`
	TotalWorkUnits     int       `json:"total_work_units"`
	CompletedWorkUnits int       `json:"completed_work_units"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// StaleCheckpointInfo holds data about a running work unit with a stale checkpoint.
type StaleCheckpointInfo struct {
	WorkUnitID               types.ID
	LastCheckpointAt         time.Time
	CheckpointIntervalSeconds int
	AgeSeconds               int64
}

// WorkUnitListFilters controls filtering for List queries.
type WorkUnitListFilters struct {
	LeafID              *types.ID         `json:"leaf_id,omitempty"`
	BatchID             *types.ID         `json:"batch_id,omitempty"`
	State               *WorkUnitState    `json:"state,omitempty"`
	Priority            *WorkUnitPriority `json:"priority,omitempty"`
	AssignedVolunteerID *types.ID         `json:"assigned_volunteer_id,omitempty"`
	FlaggedForReview    *bool             `json:"flagged_for_review,omitempty"`
}
