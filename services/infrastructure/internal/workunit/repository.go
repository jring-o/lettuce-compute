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

	// FlushReservations writes a batch of dispatch-cache reservations in one
	// multi-row UPDATE using the per-row optimistic reservation guard, returning the
	// set of work_unit_ids whose reservation actually landed (ids not returned are
	// conflicts the cache must void). The same statement RENEWS this head's dispatch
	// claim (dispatch_claim_expires_at = NOW() + claimLease) on any unit still claimed
	// by headID, so a held-but-unflushed unit's claim never expires under it. A
	// headID of uuid.Nil disables claim renewal (single-replica / pre-Layer-3 paths).
	FlushReservations(ctx context.Context, recs []FlushReservation, headID types.ID, claimLease time.Duration) ([]types.ID, error)

	// CountActiveByVolunteer returns the authoritative per-volunteer inflight count
	// (active history rows + live reservations) keyed by volunteer id, used to
	// reconcile the dispatch cache's in-memory inflight counters.
	CountActiveByVolunteer(ctx context.Context) (map[types.ID]int, error)

	// ReserveNextAssignable finds the next assignable QUEUED work unit (same
	// predicates as FindNextAssignable, including the per-volunteer inflight cap
	// counting live reservations) and stamps a lease on it (reserved_until,
	// reserved_volunteer_id), keeping state='QUEUED'. Used by the batch-fill path
	// to lease buffered work without starting the deadline/heartbeat clock.
	// Returns nil, nil if no work available.
	ReserveNextAssignable(ctx context.Context, opts AssignmentOptions, lease time.Duration) (*WorkUnit, error)

	// Assign transitions a work unit from QUEUED to ASSIGNED and sets assignment metadata.
	// Returns the updated work unit. Fails if work unit is not in QUEUED state.
	Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*WorkUnit, error)

	// CountByLeafAndState returns the count of work units for a leaf in a given state.
	CountByLeafAndState(ctx context.Context, leafID types.ID, state WorkUnitState) (int64, error)

	// FindExpiredWorkUnits returns ASSIGNED or RUNNING work units past their deadline.
	FindExpiredWorkUnits(ctx context.Context, limit int) ([]*WorkUnit, error)

	// FindLapsedReservations returns still-QUEUED work units whose buffer lease has
	// lapsed (reserved_until < NOW()), i.e. a buffered (reserved) unit whose holder
	// vanished before run-start. The caller clears each reservation (ClearReservation),
	// leaving the unit QUEUED and immediately re-stageable — no expire/reassign is
	// needed. Closes the #22 lapsed-lease reclaim gap.
	FindLapsedReservations(ctx context.Context, limit int) ([]*WorkUnit, error)

	// TransitionToExpired moves a work unit to EXPIRED state.
	TransitionToExpired(ctx context.Context, id types.ID) (*WorkUnit, error)

	// Reassign transitions an EXPIRED or REJECTED work unit back to QUEUED
	// with incremented reassignment_count and HIGH priority. Clears assignment fields.
	// If reassignment_count >= max_reassignments, transitions to FAILED and sets flagged_for_review.
	// Returns the updated work unit and whether it was re-queued (true) or failed (false).
	Reassign(ctx context.Context, id types.ID) (wu *WorkUnit, requeued bool, err error)

	// MarkSpotCheck sets spot_check = true for a work unit.
	MarkSpotCheck(ctx context.Context, id types.ID) error

	// StampReservation sets reserved_until / reserved_volunteer_id on a still-QUEUED
	// work unit (used by the batch spot-check branch to hide the unit from the same
	// volunteer's subsequent iterations). Returns the updated WorkUnit.
	StampReservation(ctx context.Context, id, volunteerID types.ID, lease time.Duration) (*WorkUnit, error)

	// ClearReservation drops the reservation columns on a still-QUEUED unit
	// reserved to volunteerID, leaving it QUEUED so it is immediately
	// re-reservable. Used when a volunteer abandons a buffered (reserved,
	// un-started) unit. Returns the updated WorkUnit, or apierror.Conflict if no
	// matching reserved QUEUED unit exists.
	ClearReservation(ctx context.Context, id, volunteerID types.ID) (*WorkUnit, error)

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
