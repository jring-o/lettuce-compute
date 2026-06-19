package workunit

import (
	"context"
	"time"

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

	// FindDispatchableBatch bulk-selects up to `limit` QUEUED, dispatch-eligible
	// (non-WASM, redundancy/reservation-eligible) work units for the in-memory
	// dispatch cache, excluding any id the cache already holds (excludeIDs). It keeps
	// the global no-double-hand guards in SQL (FOR UPDATE SKIP LOCKED, redundancy,
	// reservation) but is volunteer-agnostic — the per-requester predicates are
	// re-checked in memory at hand-out. When leafIDs is non-empty the select is scoped
	// to those leafs (the on-demand leaf-scoped refill that prevents one leaf from
	// monopolizing the ready pool and starving a leaf-filtered requester); nil/empty
	// leafIDs selects across all ACTIVE non-WASM leafs.
	FindDispatchableBatch(ctx context.Context, limit int, excludeIDs []types.ID, leafIDs []types.ID) ([]DispatchCandidate, error)

	// ClaimDispatchableBatch is the Layer-3 (horizontal scale-out) claim-on-refill
	// counterpart of FindDispatchableBatch: it selects the same LIMIT-N batch but
	// ATOMICALLY stamps a per-head dispatch claim (dispatch_claimed_by = headID,
	// dispatch_claim_expires_at = NOW() + lease) on each staged unit, so a unit one
	// replica stages is invisible to every other replica's refill (its claim is live
	// and owned by another head). The unit stays QUEUED. Closes the cross-replica
	// double-hand while keeping the per-request hand-out hot path DB-free (the claim
	// cost is amortized at bulk-refill).
	ClaimDispatchableBatch(ctx context.Context, headID types.ID, lease time.Duration, limit int, excludeIDs []types.ID, leafIDs []types.ID) ([]DispatchCandidate, error)

	// ClearExpiredDispatchClaims NULLs the dispatch-claim columns on every unit whose
	// claim has expired. HYGIENE ONLY (an expired claim is already re-claimable by
	// any refill): run from the leader-gated fault monitor. Returns rows cleared.
	ClearExpiredDispatchClaims(ctx context.Context) (int64, error)

	// FlushReservations materializes a batch of dispatch-cache in-memory holds as
	// per-copy RESERVED rows (one work_unit_assignment_history row each), returning the
	// (work_unit, volunteer) pairs whose copy actually landed (pairs not returned are
	// conflicts the cache must void). The same call RENEWS this head's dispatch claim
	// on the batch's units when headID is non-zero. Two rows for the SAME unit but
	// DISTINCT volunteers both land when redundancy allows — the parallel-copy case.
	FlushReservations(ctx context.Context, recs []FlushReservation, headID types.ID, claimLease time.Duration) ([]FlushedCopy, error)

	// CountActiveByVolunteer returns the authoritative per-volunteer inflight count
	// (live copies) keyed by volunteer id, used to reconcile the dispatch cache's
	// in-memory inflight counters.
	CountActiveByVolunteer(ctx context.Context) (map[types.ID]int, error)

	// ReserveNextAssignable finds the next assignable QUEUED work unit (same
	// predicates as FindNextAssignable) and inserts a RESERVED copy held until the
	// unit's deadline, keeping the unit QUEUED. Used by the non-cache batch-fill path
	// to lease buffered work without starting the deadline clock. Returns nil, nil if
	// no work available. The returned unit carries transient ReservedUntil/ReservedVolunteerID
	// for the proto echo.
	ReserveNextAssignable(ctx context.Context, opts AssignmentOptions, lease time.Duration) (*WorkUnit, error)

	// ReserveCopy inserts a RESERVED copy for (workUnitID, volunteerID) held until
	// reservedUntil, snapshotting deadlineSeconds. Returns apierror.Conflict if the
	// volunteer already holds a live copy or the unit is not QUEUED.
	ReserveCopy(ctx context.Context, workUnitID, volunteerID types.ID, reservedUntil time.Time, deadlineSeconds int) (*Copy, error)

	// Assign run-starts a volunteer's reserved copy (started_at = NOW), starting the
	// per-copy deadline clock. The WORK UNIT stays QUEUED so its other redundancy
	// copies keep dispatching in parallel. Fails if the volunteer has no live
	// un-started copy.
	Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*WorkUnit, error)

	// CountByLeafAndState returns the count of work units for a leaf in a given state.
	CountByLeafAndState(ctx context.Context, leafID types.ID, state WorkUnitState) (int64, error)

	// FindExpiredCopies returns LIVE copies past their deadline — RUNNING copies past
	// started_at + deadline_seconds, or RESERVED (buffered) copies past reserved_until.
	FindExpiredCopies(ctx context.Context, limit int) ([]*Copy, error)

	// FindStuckSpotCheckUnits returns QUEUED spot-check units that sat over an hour
	// without a second corroborator (the caller clears spot_check to accept the single
	// result).
	FindStuckSpotCheckUnits(ctx context.Context, limit int) ([]*WorkUnit, error)

	// CloseCopy closes a copy by id with the given outcome (EXPIRED/ABANDONED), idempotently.
	CloseCopy(ctx context.Context, copyID types.ID, outcome string) error

	// CloseCopyByVolunteer closes a volunteer's live copy of a unit with the given
	// outcome (submit/abandon). Returns apierror.Conflict if no live copy exists.
	CloseCopyByVolunteer(ctx context.Context, workUnitID, volunteerID types.ID, outcome string, resultID *types.ID) error

	// ExpireLiveCopies closes ALL live copies of a unit with the given outcome
	// (operator manual-requeue). Returns how many were closed.
	ExpireLiveCopies(ctx context.Context, workUnitID types.ID, outcome string) (int, error)

	// CountLiveCopies returns the number of live (RESERVED + RUNNING) copies of a unit.
	CountLiveCopies(ctx context.Context, workUnitID types.ID) (int, error)

	// CountTotalCopies returns the total copies ever created for a unit (dead-letter probe).
	CountTotalCopies(ctx context.Context, workUnitID types.ID) (int, error)

	// DeadLetterIfExhausted parks a unit FAILED + flagged-for-review when it is QUEUED
	// with no live copy, redundancy unmet, and total copies >= its dead-letter ceiling
	// (max_total_copies / derived default). The only cap on requeue (property 6).
	// Returns whether the unit was failed.
	DeadLetterIfExhausted(ctx context.Context, workUnitID types.ID) (bool, error)

	// Reassign returns an EXPIRED or REJECTED work unit to QUEUED for further
	// corroboration (uncapped — property 6). Always requeued=true on success.
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
