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
	LastHeartbeatAt          *time.Time       `json:"last_heartbeat_at,omitempty"`
	FlaggedForReview         bool             `json:"flagged_for_review"`
	SpotCheck                bool             `json:"spot_check"`
	LastCheckpointAt         *time.Time       `json:"last_checkpoint_at,omitempty"`
	LastCheckpointSequence   int              `json:"last_checkpoint_sequence"`
	// ReservedUntil / ReservedVolunteerID model a lightweight lease on a buffered
	// (still-QUEUED) work unit: while reserved_until > NOW() the unit is hidden
	// from other volunteers by FindNextAssignable's reservation guard, without the
	// reclaim monitors treating it as assigned. Nil when not reserved.
	ReservedUntil            *time.Time       `json:"reserved_until,omitempty"`
	ReservedVolunteerID      *types.ID        `json:"reserved_volunteer_id,omitempty"`
	CreatedAt                time.Time        `json:"created_at"`
	UpdatedAt                time.Time        `json:"updated_at"`
}

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
