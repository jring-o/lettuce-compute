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
	// HRClass is the requesting volunteer's hardware class (CPU vendor + OS + arch). When
	// set, a unit already pinned to a DIFFERENT class (homogeneous redundancy) is excluded;
	// an unpinned unit (hr_class IS NULL) is eligible regardless. Empty = no HR filtering.
	HRClass string
	// HostID is the requesting MACHINE's effective host id (TODO #19), nil when the
	// volunteer reported no host (per-account fallback). It is stamped on the copy row
	// for per-machine attribution; the per-machine in-flight cap is enforced on
	// COALESCE(host_id, volunteer_id) so it equals this value (= VolunteerID when nil).
	// Distinctness keys on the trust SUBJECT (see TrustSubject), never on this.
	HostID *types.ID
	// TrustSubject is the requester's account-level trust subject
	// (trust.SubjectForVolunteer): the bound DID while the binding is live (OK or
	// STALE), else the per-keypair sentinel "vol:<volunteer-uuid>". Two volunteer
	// rows sharing a live DID are ONE principal, so the dispatch distinctness
	// exclusions (self-held copy, already-contributed) compare subjects, not
	// volunteer ids. Consumed ONLY by the in-memory hot-path predicate
	// (eligibleLocked); the SQL gates deliberately recompute the subject fresh from
	// the volunteers table inside each statement — the DB is always current, while
	// this snapshot can go stale across a mid-process bind/revoke (harmless: the SQL
	// landing writes then refuse the copy and the hand-out is voided). Empty means
	// "not resolved" — the predicate falls back to the sentinel of VolunteerID.
	TrustSubject string
	// BenchmarkFPOPS is the requesting host's measured CPU throughput (floating-point
	// ops per second), reported at registration and stored in hardware_capabilities.
	// It drives the feasibility-at-dispatch check: a unit whose estimated runtime
	// (leaf rsc_fpops_est / BenchmarkFPOPS) exceeds its deadline is not handed to this
	// host, so a too-slow machine never burns the whole deadline window on a run that
	// would be killed at the timeout. 0 = no benchmark reported -> the check is skipped
	// for this requester (cannot estimate, so never refuse work on a guess).
	BenchmarkFPOPS float64
}

// FeasibleByDeadline reports whether a host with benchmark FP-ops/sec can be
// expected to finish a unit of a leaf whose per-unit estimate is rscFpopsEst
// before deadlineSeconds. It is the single source of truth for the
// feasibility-at-dispatch rule, mirrored verbatim in the dispatch SQL
// (FindNextAssignable / FlushReservations / ReserveCopy):
//
//	estimated_seconds = rscFpopsEst / benchmark   (no host-specific correction yet)
//	feasible          = estimated_seconds <= deadlineSeconds
//
// It returns true (feasible) whenever it cannot estimate — no benchmark, no leaf
// estimate, or no deadline — so an un-benchmarked host or an un-estimated leaf is
// never refused work on a guess; only a definite over-run is excluded.
func FeasibleByDeadline(rscFpopsEst, benchmarkFPOPS float64, deadlineSeconds int) bool {
	if rscFpopsEst <= 0 || benchmarkFPOPS <= 0 || deadlineSeconds <= 0 {
		return true
	}
	return rscFpopsEst/benchmarkFPOPS <= float64(deadlineSeconds)
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

	// CountActiveByHost returns the authoritative per-MACHINE inflight count (live
	// copies) keyed by effective host id — COALESCE(host_id, volunteer_id), so a copy
	// from a volunteer that reported no host (NULL host_id) counts under its account id,
	// exactly matching the dispatch cache's effective-host-id keying (= volunteer id in
	// the fallback). Used to reconcile the per-host in-flight counters (TODO #19).
	CountActiveByHost(ctx context.Context) (map[types.ID]int, error)

	// ReleaseStaleBufferedCopies closes a MACHINE's buffered (RESERVED, not-yet-
	// run-started) live copies that the machine no longer holds in its client buffer
	// (TODO #19): hostID is the reporting host's effective id, matched on
	// COALESCE(host_id, volunteer_id) so only THAT machine's copies are reaped (host A's
	// report never releases host B's buffer) — and = the account id for a no-host copy.
	// heldWorkUnitIDs is the set the machine reports it still has; any of its buffered
	// copies for a unit NOT in that set, and older than olderThan (a grace window so a copy
	// handed out moments ago is not reaped before the machine's next report includes it),
	// is closed ABANDONED. The work unit stays QUEUED, so it redispatches immediately, and
	// the freed copy stops counting against the host's inflight cap. RUNNING copies
	// (started_at set) are never touched here — they ride their deadline. An empty
	// heldWorkUnitIDs means the machine holds nothing, so all its grace-aged buffered
	// copies are released. Returns the work-unit ids whose copies were released.
	ReleaseStaleBufferedCopies(ctx context.Context, hostID types.ID, heldWorkUnitIDs []types.ID, olderThan time.Time) ([]types.ID, error)

	// ReserveNextAssignable finds the next assignable QUEUED work unit (same
	// predicates as FindNextAssignable) and inserts a RESERVED copy held until the
	// unit's deadline, keeping the unit QUEUED. Used by the non-cache batch-fill path
	// to lease buffered work without starting the deadline clock. Returns nil, nil if
	// no work available. The returned unit carries transient ReservedUntil/ReservedVolunteerID
	// for the proto echo.
	ReserveNextAssignable(ctx context.Context, opts AssignmentOptions, lease time.Duration) (*WorkUnit, error)

	// ReserveCopy inserts a RESERVED copy for (workUnitID, volunteerID) held until
	// reservedUntil, snapshotting deadlineSeconds. hostID attributes the copy to the
	// machine (nil = no host reported). Returns apierror.Conflict if the volunteer
	// already holds a live copy or the unit is not QUEUED.
	ReserveCopy(ctx context.Context, workUnitID, volunteerID types.ID, hostID *types.ID, reservedUntil time.Time, deadlineSeconds int) (*Copy, error)

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

	// CountProbationLiveCopies returns the live copies whose HOLDER's CURRENT effective standing
	// is not OK (BG-24b) — the subset excluded from redundancy coverage so a unit forces full
	// replication around a probation account instead of counting its copy.
	CountProbationLiveCopies(ctx context.Context, workUnitID types.ID) (int, error)

	// CountTotalCopies returns the total copies ever created for a unit (dead-letter probe).
	CountTotalCopies(ctx context.Context, workUnitID types.ID) (int, error)

	// CountErrorCopies returns the unit's wasted-work tally (EXPIRED/ABANDONED copies +
	// DISAGREED results) — the max_error_copies cap probe (TODO #50).
	CountErrorCopies(ctx context.Context, workUnitID types.ID) (int, error)

	// MarkCompleted transitions a unit QUEUED/ASSIGNED/RUNNING -> COMPLETED (the pre-validation
	// state once a quorum's worth of results is in). Idempotent. Owned by the transitioner.
	MarkCompleted(ctx context.Context, id types.ID) error

	// DeadLetterIfExhausted parks a unit FAILED + flagged-for-review when it is QUEUED
	// with no live copy, redundancy unmet, and total copies >= its dead-letter ceiling
	// (max_total_copies / derived default). The only cap on requeue (property 6).
	// Returns whether the unit was failed.
	DeadLetterIfExhausted(ctx context.Context, workUnitID types.ID) (bool, error)

	// Reassign returns an EXPIRED or REJECTED work unit to QUEUED for further
	// corroboration (uncapped — property 6). Always requeued=true on success.
	Reassign(ctx context.Context, id types.ID) (wu *WorkUnit, requeued bool, err error)

	// EnsureWorkUnitHRClass stamps the homogeneous-redundancy hardware class on a unit
	// first-writer-wins (SET hr_class = COALESCE(hr_class, $2)) and returns the effective
	// class. Idempotent: once the first copy pins a class, later callers get that class
	// back regardless of their own. Used at first hand-out so every subsequent copy of the
	// unit is restricted to the same class.
	EnsureWorkUnitHRClass(ctx context.Context, workUnitID types.ID, class string) (string, error)

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
