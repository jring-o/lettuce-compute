package workunit

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// AssignmentOptions configures the work unit assignment query.
type AssignmentOptions struct {
	VolunteerID             types.ID
	LeafIDs                 []types.ID // empty = any matching leaf
	BlockedLeafIDs          []types.ID
	MaxCPUCores             int
	MaxMemoryMB             int
	MaxDiskMB               int64
	HasGPU                  bool
	MaxGPUVRAMMB            int
	AvailableRuntimes       []string
	GPUVendors              []string // ["NVIDIA", "AMD"] — vendors of volunteer's GPUs
	GPUComputeCapabilities  []string // ["8.6", "gfx1030"] — compute capabilities
	MaxInflightPerVolunteer int      // server-enforced cap on concurrent assigned WUs
}

// WorkUnitRepository defines the data-access interface for work units.
type WorkUnitRepository interface {
	Create(ctx context.Context, wu *WorkUnit) error
	GetByID(ctx context.Context, id types.ID) (*WorkUnit, error)
	List(ctx context.Context, filters WorkUnitListFilters, page types.PaginationRequest) ([]*WorkUnit, types.PaginationResponse, error)
	UpdateState(ctx context.Context, id types.ID, from, to WorkUnitState) (*WorkUnit, error)
	BulkCreate(ctx context.Context, wus []*WorkUnit) error

	// BulkTransitionByBatch transitions all work units in a batch from one state to another
	// in a single UPDATE. Returns the number of rows affected.
	BulkTransitionByBatch(ctx context.Context, batchID types.ID, from, to WorkUnitState) (int64, error)

	// FindNextAssignable finds the highest-priority QUEUED work unit from active leafs
	// that matches the volunteer's capabilities and has fewer active assignments than
	// the leaf's redundancy_factor. Returns nil, nil if no work available.
	FindNextAssignable(ctx context.Context, opts AssignmentOptions) (*WorkUnit, error)

	// Assign transitions a work unit from QUEUED to ASSIGNED and sets assignment metadata.
	// Returns the updated work unit. Fails if work unit is not in QUEUED state.
	Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*WorkUnit, error)

	// UpdateHeartbeat updates last_heartbeat_at for a work unit.
	UpdateHeartbeat(ctx context.Context, id types.ID) error

	// CountByLeafAndState returns the count of work units for a leaf in a given state.
	CountByLeafAndState(ctx context.Context, leafID types.ID, state WorkUnitState) (int64, error)

	// FindExpiredWorkUnits returns ASSIGNED or RUNNING work units past their deadline.
	FindExpiredWorkUnits(ctx context.Context, limit int) ([]*WorkUnit, error)

	// FindAbandonedWorkUnits returns ASSIGNED or RUNNING work units with stale heartbeats.
	// A work unit is abandoned if: NOW() - last_heartbeat_at > heartbeat_interval * missed_threshold.
	// ASSIGNED units are included so orphans on no_deadline leafs (assigned but never
	// run) are reclaimed; PREPARING heartbeats keep live pulls/queued units fresh.
	FindAbandonedWorkUnits(ctx context.Context, limit int) ([]*WorkUnit, error)

	// TransitionToExpired moves a work unit to EXPIRED state.
	TransitionToExpired(ctx context.Context, id types.ID) (*WorkUnit, error)

	// Reassign transitions an EXPIRED or REJECTED work unit back to QUEUED
	// with incremented reassignment_count and HIGH priority. Clears assignment fields.
	// If reassignment_count >= max_reassignments, transitions to FAILED and sets flagged_for_review.
	// Returns the updated work unit and whether it was re-queued (true) or failed (false).
	Reassign(ctx context.Context, id types.ID) (wu *WorkUnit, requeued bool, err error)

	// MarkSpotCheck sets spot_check = true for a work unit.
	MarkSpotCheck(ctx context.Context, id types.ID) error

	// ClearSpotCheck sets spot_check = false, allowing single-result validation.
	ClearSpotCheck(ctx context.Context, id types.ID) error

	// FindRunningWithStaleCheckpoints returns running work units with checkpointing enabled
	// whose last checkpoint is older than 2× the configured checkpoint interval.
	FindRunningWithStaleCheckpoints(ctx context.Context, limit int) ([]StaleCheckpointInfo, error)
}

// BatchRepository defines the data-access interface for batches.
type BatchRepository interface {
	Create(ctx context.Context, b *Batch) error
	GetByID(ctx context.Context, id types.ID) (*Batch, error)
	ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*Batch, types.PaginationResponse, error)
	IncrementCompleted(ctx context.Context, batchID types.ID) error
}
