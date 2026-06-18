package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Layer 2/3: in-process dispatch cache (per-replica, claim-on-refill) -------
//
// The dispatch cache takes Postgres OFF the RequestWorkUnit hot path. A background
// refiller bulk-fetches QUEUED, dispatch-eligible units into an in-memory ready
// pool; RequestWorkUnit serves reservations from that pool in memory (zero DB I/O
// on the hot path); a background flusher writes the reservations to Postgres
// asynchronously in batched multi-row UPDATEs.
//
// It is an in-memory MIRROR of the existing Layer-1 reservation-columns model, not
// a new ASSIGNED-at-handout model: a hand-out produces a reservation (the unit
// stays state='QUEUED'), exactly as ReserveNextAssignable did, so every Layer-1
// correctness property (no double-reserve, per-volunteer inflight cap, redundancy,
// spot-check, runtime/capability eligibility, blocked leafs, the reservation lease)
// is preserved. Run-start (QUEUED->ASSIGNED + active history row) is a separate
// explicit StartWork step.
//
// HORIZONTAL SCALE-OUT (Layer 3, claim-on-refill): each replica runs its OWN cache
// against the SHARED Postgres. The refill is NOT a plain SELECT but an atomic
// ClaimDispatchableBatch UPDATE that stamps a per-head dispatch claim
// (dispatch_claimed_by = this replica's instance id, dispatch_claim_expires_at = a
// short lease) on each staged unit. A unit one replica stages is invisible to every
// other replica's refill (its claim is live and owned by another head), so two
// replicas can NEVER double-hand the same QUEUED unit. The claim is amortized at
// bulk-refill, so the per-request hand-out hot path stays 100% in memory. A held
// unit's claim is renewed off the hot path by the async reservation flush. When no
// head id is configured (single-replica) the cache uses the claim-free Layer-2
// refill/flush — identical behavior, no DB column writes for claims.
//
// CRASH SAFETY: the cache is an optimization over the source-of-truth Postgres.
//   - An unflushed reservation lost at crash leaves the unit plain QUEUED in PG ->
//     immediately re-dispatchable. Its dispatch claim simply EXPIRES (the crashed
//     owner stopped renewing it) and the unit becomes re-claimable by any survivor
//     on its next refill — passive expiry is the reclaim guarantee, no active sweep
//     is required for correctness (the leader-gated hygiene sweep only tidies).
//   - A flushed-as-reserved unit whose in-memory owner vanished is reclaimed by the
//     lapsed-reservation sweep (FindLapsedReservations, WP-HEAD-DEADLINE) once
//     reserved_until passes.
//   - In-memory counters are rebuilt lazily / reconciled from authoritative DB
//     counts on the reconcile tick, so crash/drift cannot cause permanent
//     over-admission or stranding.

const (
	defaultRefillTickInterval = 250 * time.Millisecond
	defaultReconcileInterval  = 30 * time.Second
	dispatchDBTimeout         = 2 * time.Second
	// defaultLeafSnapshotTTL bounds how long the cache trusts a cached leaf snapshot
	// before re-reading it on the assignment-build path. It is the propagation
	// ceiling for an artifact publish/rollback (TODO #38): a RUNNING volunteer picks
	// up a new version on its next work request within this window, with no restart.
	defaultLeafSnapshotTTL = 30 * time.Second
)

// candidate is one pre-fetched, ready-to-assign QUEUED unit in the ready pool. It
// carries everything HandOut + buildWorkUnitAssignment need so a hand-out touches
// no DB.
type candidate struct {
	unit *workunit.WorkUnit
	// effectiveRedundancy is the leaf redundancy (2 for spot-check), the cap on the
	// number of distinct in-memory holders of this unit.
	effectiveRedundancy int
	// dbActiveCount is the active-history-row count of this unit at refill time,
	// the authoritative floor on its redundancy headroom.
	dbActiveCount int
}

// inMemHolderCap is the maximum number of DISTINCT in-memory reservation holders the
// cache may stage for this candidate concurrently, before the flush has landed.
//
// This is NOT the leaf redundancy_factor: it is bounded by how many concurrent live
// reservations Postgres can actually persist for the unit before a run-start frees
// the column. A NORMAL (non-spot-check) reservation lands in the SINGLE
// reserved_volunteer_id column, so AT MOST ONE NORMAL reservation can be live at a
// time (reservation_test.go's columns-only "one reserver at a time" invariant). If
// the cache staged a redundancy>1 NORMAL unit to a second distinct holder from one
// ready snapshot, that holder's flush would conflict on the column (only one
// reserved_volunteer_id row landed via RETURNING) and — worse — the void-check could
// not tell which holder lost, leaking a phantom in-memory reservation + inflight that
// reconcile never clears. So the cache caps NORMAL in-memory holders at 1.
//
// LIMITATION (pre-existing, not introduced by the cache): a redundancy>1 NORMAL
// unit's SECOND distinct holder is NOT reached concurrently through dispatch. Once
// the first holder run-starts, Assign flips the unit QUEUED -> ASSIGNED, and BOTH
// the refiller (FindDispatchableBatch) and the Layer-1 DB path (FindNextAssignable)
// gate on state='QUEUED' — so the unit leaves the dispatchable universe and neither
// path re-stages it for a second distinct volunteer. Concurrent redundant dispatch
// would need a per-volunteer dispatch table (out of scope; a later layer). Redundant
// corroboration is produced SERIALLY: when the unit is reassigned (deadline lapse or
// abandon re-queues it to QUEUED), a different volunteer is offered it and its result
// is cross-checked. The single-holder cap here only preserves that existing serial
// flow; it does not enable concurrent redundancy. See the NOTE on the
// TestDispatchCache_RedundancyTwo... e2e test and head-setup.md
// "Redundancy and the dispatch cache".
//
// A spot-check unit is the one exception: each holder gets its OWN
// assignment_history row at flush (the history-row-at-reservation model), not the
// shared column, so two distinct holders can be staged concurrently — its in-memory
// cap is the full effectiveRedundancy (2).
func (cd candidate) inMemHolderCap() int {
	if cd.unit.SpotCheck {
		return cd.effectiveRedundancy
	}
	return 1
}

// dispatchCacheConfig holds the cache's tunables.
type dispatchCacheConfig struct {
	readyPoolSize           int
	lowWatermark            int
	refillBatchSize         int
	admissionCap            int
	maintenanceAdmissionCap int
	flushInterval           time.Duration
	flushBatchSize          int
	leaseSeconds            int
	maxInflightPerVolunteer int
	// leafSnapshotTTL bounds staleness of the cached leaf snapshot used to build
	// assignments, so a published/rolled-back artifact version (or a direct
	// execution_config change) propagates to RUNNING volunteers within the TTL with
	// no head restart (TODO #38). 0 -> defaultLeafSnapshotTTL.
	leafSnapshotTTL time.Duration

	// --- Layer 3: horizontal scale-out (claim-on-refill) ---
	//
	// headID is this replica's stable instance id, stamped as the dispatch-claim
	// owner (dispatch_claimed_by) at bulk-refill. When it is the zero value (single-
	// replica / pre-Layer-3), the cache falls back to the claim-free
	// FindDispatchableBatch refill and FlushReservations performs no claim renewal —
	// identical to Layer-2 behavior.
	headID types.ID
	// claimLease is how long a dispatch claim is held before it expires and the unit
	// becomes re-claimable. Renewed every flush tick for actively-held units.
	claimLease time.Duration
}

// scaleOutEnabled reports whether claim-on-refill is active (a non-nil head id was
// configured). When false the cache uses the Layer-2 claim-free refill/flush paths.
func (cfg dispatchCacheConfig) scaleOutEnabled() bool {
	return cfg.headID != (types.ID{})
}

// dispatchDeps is the cache's DB-facing dependency surface (the subset of repos it
// touches), narrow so tests can substitute fakes.
type dispatchDeps struct {
	wuRepo        workunit.WorkUnitRepository
	leafRepo      leaf.Repository
	assignRepo    assignment.Repository
	volunteerRepo volunteer.Repository
	// artifactVersionRepo resolves immutable artifact version rows and pins a unit to
	// a version for homogeneous redundancy (TODO #38). May be nil (legacy / tests):
	// the cache then builds assignments from the leaf's denormalized current
	// execution_config only, with no pinning.
	artifactVersionRepo leaf.ArtifactVersionRepository
}

// volunteerIdentity is the in-process snapshot of a volunteer's identity +
// capabilities the RequestWorkUnit hot path needs (Blocker 1). It mirrors the
// fields the per-request s.volunteerRepo.GetByID used to fetch from Postgres on
// every request, so the hot path resolves identity/capabilities in memory and never
// touches the pool. It is warmed at RegisterVolunteer (the natural write point),
// refreshed lazily on a cache miss under the admission semaphore, and is otherwise
// process-lifetime stable (a volunteer's pubkey/hardware change only on re-register,
// which re-warms it).
type volunteerIdentity struct {
	publicKey         []byte
	hardware          volunteer.HardwareCapabilities
	availableRuntimes []string
}

// spotCheckWrite is one deferred spot-check marking (MarkSpotCheck +
// StampReservation + a history row), flushed asynchronously like a normal
// reservation but via a distinct DB shape.
type spotCheckWrite struct {
	workUnitID    types.ID
	volunteerID   types.ID
	reservedUntil time.Time
}

// dispatchCache is the in-process dispatch ledger. All mutable state is guarded by
// mu; the refiller/flusher/reconciler acquire mu only briefly to swap slices /
// drain queues, never across a DB call.
type dispatchCache struct {
	cfg    dispatchCacheConfig
	deps   dispatchDeps
	logger *slog.Logger
	now    func() time.Time

	mu sync.Mutex
	// ready is the bounded pool of stageable units (front = highest priority).
	ready []candidate
	// reservedInMem maps a handed-out unit id -> the set of distinct volunteers that
	// currently hold an in-memory reservation on it (the in-process no-double-reserve
	// guard and the redundancy>1 multi-holder tracker).
	reservedInMem map[types.ID]map[types.ID]time.Time
	// inflight is the per-volunteer (live reservations + active history rows) count.
	inflight map[types.ID]int
	// pendingWrites is the async NORMAL reservation-write queue.
	pendingWrites []workunit.FlushReservation
	// pendingSpotChecks is the async spot-check marking queue: each is MarkSpotCheck
	// + StampReservation + one assignment_history row (the one history-row-at-
	// reservation case, kept from Layer 1). Kept separate from pendingWrites because
	// it is a different (non-batchable) DB shape.
	pendingSpotChecks []spotCheckWrite

	// leafCache caches per-leaf metadata (the full leaf, used for capability matching
	// and proto building). Guarded by leafMu (separate from mu so a leaf fetch under
	// admission does not block hand-outs).
	leafMu    sync.Mutex
	leafCache map[types.ID]*cachedLeaf

	// versionCache caches IMMUTABLE artifact version rows by id. A published version
	// never changes, so these are safe to keep for the process lifetime; only used to
	// build an assignment for a unit pinned to a version other than the leaf's current.
	versionMu    sync.Mutex
	versionCache map[types.ID]*leaf.ArtifactVersion

	// identityCache caches per-volunteer identity snapshots (pubkey + hardware +
	// available runtimes) so RequestWorkUnit resolves identity/capabilities in memory
	// (Blocker 1: takes s.volunteerRepo.GetByID off the hot path). Guarded by its own
	// mutex (separate from mu / leafMu so an identity fetch under admission does not
	// block hand-outs).
	identityMu    sync.Mutex
	identityCache map[types.ID]*volunteerIdentity

	// admission bounds concurrent CLIENT write-path dispatch-cache DB operations
	// (StartWork / SubmitResult / AbandonWorkUnit gates, the RequestWorkUnit
	// cold-miss identity read, getLeaf, resolveIdentity). See maintenanceAdmission
	// for the SEPARATE background-restock budget.
	admission chan struct{}
	// maintenanceAdmission is a SEPARATE, reserved admission budget for background
	// restock/landing ops (the refiller's fetchAndStage, the ticker flusher's
	// reservation-flush, and the spot-check flush) so a client write storm holding
	// the client `admission` budget cannot starve cache restock (FIX 4). It is a
	// brand-new channel pulled ONLY by the refiller + flusher goroutines, which
	// never simultaneously hold the client `admission` slot — and the held-slot path
	// (flushAllPendingHeld, called while StartWork holds a client slot) does NOT
	// touch it — so it cannot reintroduce the cap-1 self-deadlock.
	maintenanceAdmission chan struct{}
	// scanCount is a TEST-ONLY counter incremented once per ready-pool candidate
	// VISITED by HandOut, used to assert the FIX-1 early-exit stops scanning the pool
	// once n reservations are taken. It carries no production behavior.
	scanCount int
	// refillSignal nudges the refiller when a hand-out drains the pool.
	refillSignal chan struct{}
	// leafRefillSignal nudges the refiller to do an ON-DEMAND, LEAF-SCOPED refill
	// (resolves Blocker 2: leaf-filtered starvation). When a HandOut filtered to a set
	// of leafs finds zero eligible candidates while the ready pool is non-empty (the
	// pool is monopolized by a different leaf), it requests a leaf-scoped refill for
	// those leafs so they get staged regardless of the global low-watermark. Buffered
	// so the hot path never blocks; pendingLeafRefills coalesces requests.
	leafRefillSignal chan struct{}
	// pendingLeafRefills is the set of leaf ids awaiting an on-demand leaf-scoped
	// refill (guarded by leafRefillMu). Coalesces bursts of starved leaf-filtered
	// requests into one targeted refill.
	leafRefillMu       sync.Mutex
	pendingLeafRefills map[types.ID]struct{}
}

// newDispatchCache builds a cache. admissionCap <= 0 is treated as 1.
func newDispatchCache(cfg dispatchCacheConfig, deps dispatchDeps, logger *slog.Logger) *dispatchCache {
	if cfg.admissionCap <= 0 {
		cfg.admissionCap = 1
	}
	if cfg.readyPoolSize <= 0 {
		cfg.readyPoolSize = 2000
	}
	if cfg.lowWatermark <= 0 {
		cfg.lowWatermark = cfg.readyPoolSize / 4
	}
	if cfg.refillBatchSize <= 0 {
		cfg.refillBatchSize = 500
	}
	if cfg.flushInterval <= 0 {
		cfg.flushInterval = 100 * time.Millisecond
	}
	if cfg.flushBatchSize <= 0 {
		cfg.flushBatchSize = 200
	}
	if cfg.maintenanceAdmissionCap <= 0 {
		// Default a reserved background budget of a quarter of the client budget so
		// client writers cannot starve restock; always >= 1.
		cfg.maintenanceAdmissionCap = cfg.admissionCap / 4
		if cfg.maintenanceAdmissionCap < 1 {
			cfg.maintenanceAdmissionCap = 1
		}
	}
	if cfg.leafSnapshotTTL <= 0 {
		cfg.leafSnapshotTTL = defaultLeafSnapshotTTL
	}
	return &dispatchCache{
		cfg:                  cfg,
		deps:                 deps,
		logger:               logger,
		now:                  time.Now,
		reservedInMem:        make(map[types.ID]map[types.ID]time.Time),
		inflight:             make(map[types.ID]int),
		leafCache:            make(map[types.ID]*cachedLeaf),
		versionCache:         make(map[types.ID]*leaf.ArtifactVersion),
		identityCache:        make(map[types.ID]*volunteerIdentity),
		admission:            make(chan struct{}, cfg.admissionCap),
		maintenanceAdmission: make(chan struct{}, cfg.maintenanceAdmissionCap),
		refillSignal:         make(chan struct{}, 1),
		leafRefillSignal:     make(chan struct{}, 1),
		pendingLeafRefills:   make(map[types.ID]struct{}),
	}
}

// handOutResult is one reserved unit + its leaf, ready to build into a proto
// assignment.
type handOutResult struct {
	unit *workunit.WorkUnit
	leaf *leaf.Leaf
	// execConfig overrides leaf.ExecutionConfig when the unit is pinned to an artifact
	// version that differs from the leaf's current one (homogeneous redundancy across a
	// mid-flight publish). Nil = build from leaf.ExecutionConfig (the common path).
	execConfig *leaf.ExecutionConfig
}

// admissionSaturated reports whether the DB-admission semaphore is currently full
// (every slot held). Used by the shed rule.
func (c *dispatchCache) admissionSaturated() bool {
	return len(c.admission) >= cap(c.admission)
}

// tryAcquire attempts to take an admission slot without blocking. Returns a release
// func and true on success.
func (c *dispatchCache) tryAcquire() (func(), bool) {
	select {
	case c.admission <- struct{}{}:
		return func() { <-c.admission }, true
	default:
		return nil, false
	}
}

// acquire blocks (until ctx is done) for an admission slot.
func (c *dispatchCache) acquire(ctx context.Context) (func(), bool) {
	select {
	case c.admission <- struct{}{}:
		return func() { <-c.admission }, true
	case <-ctx.Done():
		return nil, false
	}
}

// maintenanceAdmissionSaturated reports whether the maintenance admission budget is
// currently full (FIX 4).
func (c *dispatchCache) maintenanceAdmissionSaturated() bool {
	return len(c.maintenanceAdmission) >= cap(c.maintenanceAdmission)
}

// tryAcquireMaintenance attempts to take a maintenance admission slot without
// blocking. Returns a release func and true on success (FIX 4).
func (c *dispatchCache) tryAcquireMaintenance() (func(), bool) {
	select {
	case c.maintenanceAdmission <- struct{}{}:
		return func() { <-c.maintenanceAdmission }, true
	default:
		return nil, false
	}
}

// acquireMaintenance blocks (until ctx is done) for a maintenance admission slot.
// Used by background restock/landing ops (refiller fetchAndStage, ticker
// reservation-flush, spot-check flush) so a client write storm holding the client
// `admission` budget cannot starve them (FIX 4).
func (c *dispatchCache) acquireMaintenance(ctx context.Context) (func(), bool) {
	select {
	case c.maintenanceAdmission <- struct{}{}:
		return func() { <-c.maintenanceAdmission }, true
	case <-ctx.Done():
		return nil, false
	}
}

// readyLen returns the current ready-pool length (for tests / shed checks).
func (c *dispatchCache) readyLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ready)
}

// signalRefill nudges the refiller (non-blocking).
func (c *dispatchCache) signalRefill() {
	select {
	case c.refillSignal <- struct{}{}:
	default:
	}
}

// requestLeafRefill records that one or more leafs need an on-demand, leaf-scoped
// refill (Blocker 2) and nudges the refiller (non-blocking, never blocks the hot
// path). Requests for the same leaf coalesce into one refill.
func (c *dispatchCache) requestLeafRefill(leafIDs []types.ID) {
	if len(leafIDs) == 0 {
		return
	}
	c.leafRefillMu.Lock()
	for _, id := range leafIDs {
		c.pendingLeafRefills[id] = struct{}{}
	}
	c.leafRefillMu.Unlock()
	select {
	case c.leafRefillSignal <- struct{}{}:
	default:
	}
}

// drainLeafRefills returns and clears the set of leafs awaiting an on-demand
// leaf-scoped refill.
func (c *dispatchCache) drainLeafRefills() []types.ID {
	c.leafRefillMu.Lock()
	defer c.leafRefillMu.Unlock()
	if len(c.pendingLeafRefills) == 0 {
		return nil
	}
	out := make([]types.ID, 0, len(c.pendingLeafRefills))
	for id := range c.pendingLeafRefills {
		out = append(out, id)
	}
	c.pendingLeafRefills = make(map[types.ID]struct{})
	return out
}

// HandOut serves up to n reservations to volunteerID from the in-memory ready pool,
// re-checking every per-requester predicate in memory (ported verbatim from the SQL
// FindNextAssignable). It is the zero-DB hot path. Returned units carry a
// reserved_until window; their reservations are enqueued for the async flush.
//
// On return it also reports whether the pool is now below the low watermark (so the
// caller can nudge the refiller).
func (c *dispatchCache) HandOut(volunteerID types.ID, opts workunit.AssignmentOptions, n int) (results []handOutResult, drained bool) {
	if n < 1 {
		n = 1
	}
	leaseFallback := time.Duration(c.cfg.leaseSeconds) * time.Second

	c.mu.Lock()
	kept := c.ready[:0]
	taken := 0
	// FIX 1: scan front-to-back, but STOP scanning once n reservations are taken and
	// splice the unscanned tail back in one append (below), instead of copying every
	// trailing element tail-into-kept under the global lock (the O(pool) latency
	// cliff). `kept` aliases c.ready's backing array; an element is DROPPED only when
	// accepted-and-exhausted, never inserted ahead of the read cursor, so len(kept)
	// <= i always and the tail-splice is a safe forward-overlapping copy with no
	// realloc (len(kept)+len(tail) <= len(c.ready) <= cap). A fully-ineligible or
	// tightly leaf-filtered request that never reaches n legitimately scans to the
	// end (i == len(c.ready)); that O(pool) corner is accepted/rare per the directive.
	i := 0
	for ; i < len(c.ready); i++ {
		cand := c.ready[i]
		if taken >= n {
			break
		}
		c.scanCount++ // TEST-ONLY: count candidates actually visited (FIX-1 early-exit probe).
		if !c.eligibleLocked(volunteerID, opts, cand) {
			kept = append(kept, cand)
			continue
		}
		// Hold the buffered unit until its head-owned deadline (the buffer window);
		// fall back to the configured lease only when the unit has no deadline.
		reservedUntil := c.now().UTC().Add(leaseFallback)
		if cand.unit.DeadlineSeconds > 0 {
			reservedUntil = c.now().UTC().Add(time.Duration(cand.unit.DeadlineSeconds) * time.Second)
		}
		// Accept this candidate as a reservation for volunteerID.
		uid := cand.unit.ID
		holders := c.reservedInMem[uid]
		if holders == nil {
			holders = make(map[types.ID]time.Time)
			c.reservedInMem[uid] = holders
		}
		holders[volunteerID] = reservedUntil
		c.inflight[volunteerID]++

		// Spot-check decision: evaluated in memory at the FIRST reservation of a
		// redundancy-1, spot-check-enabled unit that is not already a spot-check.
		// A spot-checked unit stays QUEUED for a SECOND corroborating volunteer, so
		// we mark the in-memory candidate spot_check + redundancy 2 and route its
		// write to the deferred spot-check queue (MarkSpotCheck + history row).
		isFirstHold := len(holders) == 1 && cand.dbActiveCount == 0
		newlySpotChecked := false
		if isFirstHold && !cand.unit.SpotCheck {
			lf := c.peekLeaf(cand.unit.LeafID)
			if lf != nil &&
				lf.ValidationConfig.SpotCheckEnabled &&
				lf.ValidationConfig.RedundancyFactor == 1 &&
				workunit.ShouldSpotCheck(lf.ValidationConfig.SpotCheckPercentage) {
				newlySpotChecked = true
				cand.unit.SpotCheck = true
				cand.effectiveRedundancy = 2
			}
		}
		if newlySpotChecked || cand.unit.SpotCheck {
			c.pendingSpotChecks = append(c.pendingSpotChecks, spotCheckWrite{
				workUnitID:    uid,
				volunteerID:   volunteerID,
				reservedUntil: reservedUntil,
			})
		} else {
			c.pendingWrites = append(c.pendingWrites, workunit.FlushReservation{
				WorkUnitID:    uid,
				VolunteerID:   volunteerID,
				ReservedUntil: reservedUntil,
			})
		}

		// Echo the reservation window on the unit copy returned to the requester.
		ru := reservedUntil
		unitCopy := *cand.unit
		unitCopy.ReservedUntil = &ru
		vid := volunteerID
		unitCopy.ReservedVolunteerID = &vid
		results = append(results, handOutResult{unit: &unitCopy})
		taken++

		// Keep the candidate staged only if it still has BOTH total-redundancy headroom
		// AND concurrent in-memory-holder headroom for another DISTINCT volunteer;
		// otherwise it is exhausted for now and dropped from ready. A NORMAL unit (cap 1)
		// is always dropped after its first hold — its next distinct holder is re-staged
		// by the refiller once the first holder run-starts and frees the column. A
		// spot-check unit (cap = effectiveRedundancy) stays until its in-memory holders
		// are exhausted, so the SAME ready snapshot can serve its second corroborator.
		if cand.dbActiveCount+len(holders) < cand.effectiveRedundancy &&
			len(holders) < cand.inMemHolderCap() {
			kept = append(kept, cand)
		}
	}
	// Splice the unscanned tail [i:] back. When the loop ran to completion (n never
	// reached) i == len(c.ready) and the tail is empty, degenerating to the old
	// full-compaction result. kept aliases c.ready's backing array and len(kept) <= i,
	// so this forward-overlapping append never reallocates and Go's copy handles it.
	c.ready = append(kept, c.ready[i:]...)
	readyNonEmpty := len(c.ready) > 0
	drained = len(c.ready) < c.cfg.lowWatermark
	c.mu.Unlock()

	// Blocker 2 (leaf-filtered starvation): a requester filtered to specific leafs that
	// got NOTHING while the ready pool still holds units is being starved by a different
	// leaf monopolizing the pool (the global watermark refill never notices, since the
	// pool is "full"). Request an on-demand, leaf-scoped refill for the requester's
	// leafs so they get staged regardless of the watermark. (BlockedLeafIDs alone are an
	// exclusion, not a positive scope, so we only do this for an explicit LeafIDs
	// filter.)
	if len(results) == 0 && len(opts.LeafIDs) > 0 && readyNonEmpty {
		c.requestLeafRefill(opts.LeafIDs)
	}

	// Attach leaf metadata (may fetch under admission; never holds c.mu).
	final := results[:0]
	for _, r := range results {
		lf, err := c.getLeaf(r.unit.LeafID)
		if err != nil || lf == nil {
			// Could not load the leaf to build the assignment: void this hand-out
			// (it would otherwise be un-buildable). The reservation flush is harmless
			// (the unit stays QUEUED+reserved and lapses), but to avoid leaking an
			// in-memory holder we release it.
			c.releaseInMem(r.unit.ID, volunteerID)
			c.logger.Warn("dispatch cache: failed to load leaf for hand-out; voiding",
				"work_unit_id", r.unit.ID, "leaf_id", r.unit.LeafID, "error", err)
			continue
		}
		r.leaf = lf
		// Artifact pinning (TODO #38): on a versioned leaf, pin EVERY unit to the
		// current version at its first dispatch (first-writer-wins). This gives every
		// work unit and result exact per-unit version provenance (BOINC's per-WU model)
		// and is what makes redundant replicas of one unit run a homogeneous version. If
		// a unit was ALREADY pinned to a different version (e.g. a reassignment after a
		// mid-flight publish), the assignment is built from the PINNED version, not the
		// leaf's current one. Unversioned leaves (no current version) keep the legacy
		// path with no pin.
		if c.deps.artifactVersionRepo != nil && lf.CurrentArtifactVersionID != nil {
			r.execConfig = c.resolvePinnedExecConfig(r.unit.ID, *lf.CurrentArtifactVersionID)
		}
		final = append(final, r)
	}
	if drained {
		c.signalRefill()
	}
	return final, drained
}

// eligibleLocked re-checks every per-requester predicate in memory against the
// cached candidate. Ported verbatim from FindNextAssignable's SQL. Caller holds mu.
func (c *dispatchCache) eligibleLocked(volunteerID types.ID, opts workunit.AssignmentOptions, cand candidate) bool {
	uid := cand.unit.ID
	leafID := cand.unit.LeafID

	// Redundancy headroom, enforced by TWO bounds:
	//   (1) total redundancy: dbActiveCount (already-running history rows) + distinct
	//       in-memory holders must stay under the leaf's effectiveRedundancy;
	//   (2) concurrent in-memory holders: at most inMemHolderCap() at once — 1 for a
	//       NORMAL unit (the single reserved_volunteer_id column persists only one live
	//       reservation at a time), effectiveRedundancy for a spot-check unit (whose
	//       holders each get their own history row).
	// NOTE: for a NORMAL unit this caps concurrent dispatch to ONE holder. The second
	// distinct holder is NOT reached concurrently — once the first run-starts the unit
	// is ASSIGNED (no longer QUEUED) and neither the refiller nor the DB find re-stages
	// it. Redundant corroboration is serial (on reassignment). See inMemHolderCap and
	// head-setup.md "Redundancy and the dispatch cache".
	holders := c.reservedInMem[uid]
	if cand.dbActiveCount+len(holders) >= cand.effectiveRedundancy {
		return false
	}
	if len(holders) >= cand.inMemHolderCap() {
		return false
	}
	// Self-exclusion: never hand this volunteer a unit it already holds in memory.
	if _, held := holders[volunteerID]; held {
		return false
	}
	// Per-volunteer inflight cap.
	if opts.MaxInflightPerVolunteer > 0 && c.inflight[volunteerID] >= opts.MaxInflightPerVolunteer {
		return false
	}
	// Leaf-id filter (preferred leafs) and blocked-leaf filter.
	if len(opts.LeafIDs) > 0 && !containsID(opts.LeafIDs, leafID) {
		return false
	}
	if containsID(opts.BlockedLeafIDs, leafID) {
		return false
	}
	// Capability fit against the cached leaf metadata.
	lf := c.peekLeaf(leafID)
	if lf == nil {
		// Leaf not yet cached: be conservative and skip; the next refill / a warmed
		// cache lets it through. (getLeaf is not called under mu to avoid a DB touch
		// while locked.)
		return false
	}
	return leafMatchesCapabilities(lf, opts)
}

// releaseInMem drops a single in-memory reservation (one holder) and decrements the
// volunteer's inflight count. Used to void a hand-out (flush conflict, un-buildable
// leaf) or on submit/abandon.
func (c *dispatchCache) releaseInMem(unitID, volunteerID types.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseInMemLocked(unitID, volunteerID)
}

func (c *dispatchCache) releaseInMemLocked(unitID, volunteerID types.ID) {
	holders := c.reservedInMem[unitID]
	if holders == nil {
		return
	}
	if _, ok := holders[volunteerID]; !ok {
		return
	}
	delete(holders, volunteerID)
	if len(holders) == 0 {
		delete(c.reservedInMem, unitID)
	}
	if c.inflight[volunteerID] > 0 {
		c.inflight[volunteerID]--
		if c.inflight[volunteerID] == 0 {
			delete(c.inflight, volunteerID)
		}
	}
	// MINOR: when an in-memory hold is VOIDED (flush conflict, un-buildable leaf, or
	// a buffered-abandon's ClearReservation), purge any STILL-QUEUED pending
	// reservation / spot-check write for this (unit, volunteer) so a late flush
	// cannot re-stamp a reservation onto a unit whose hold was just dropped (and, for
	// abandon, already requeued). Done under the same mu as the hold drop so there is
	// no window where the hold is gone but the queued write survives.
	c.purgePendingForLocked(unitID, volunteerID)
}

// purgePendingForLocked drops any queued reservation / spot-check write for
// (unitID, volunteerID) so a late flush cannot re-stamp a reservation onto a unit
// whose in-memory hold was just voided. Caller holds mu. (Forward-overlapping
// in-place compaction, the same safe pattern as FIX 1's tail-splice: the write
// cursor never overtakes the read cursor.)
//
// This closes the re-stamp window only for entries STILL QUEUED here; an entry
// already snapshotted into an in-flight flushBatch (copied under mu, written outside
// the lock) cannot be recalled. For the buffered-abandon path that residual window
// is backstopped by the prior ClearReservation in PG (cleared BEFORE releaseInMem),
// so a late landed reservation on the already-cleared/requeued unit is a no-op
// conflict (not returned by FlushReservations), never a double-reserve.
func (c *dispatchCache) purgePendingForLocked(unitID, volunteerID types.ID) {
	if len(c.pendingWrites) > 0 {
		w := c.pendingWrites[:0]
		for _, r := range c.pendingWrites {
			if r.WorkUnitID == unitID && r.VolunteerID == volunteerID {
				continue
			}
			w = append(w, r)
		}
		c.pendingWrites = w
	}
	if len(c.pendingSpotChecks) > 0 {
		s := c.pendingSpotChecks[:0]
		for _, r := range c.pendingSpotChecks {
			if r.workUnitID == unitID && r.volunteerID == volunteerID {
				continue
			}
			s = append(s, r)
		}
		c.pendingSpotChecks = s
	}
}

// hasInMemReservation reports whether the cache currently holds an in-memory
// reservation for (unitID, volunteerID) that has not yet been flushed/cleared. Used
// by StartWork to tolerate the flush race (Major 3): a unit handed out in memory but
// whose async reservation-write has not yet landed reads back as plain QUEUED with a
// NULL reserved_volunteer_id, so the DB precondition alone would wrongly reject the
// run-start. The in-memory hold is the authoritative source for "this volunteer
// reserved this unit," so StartWork consults it and proceeds.
func (c *dispatchCache) hasInMemReservation(unitID, volunteerID types.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	holders := c.reservedInMem[unitID]
	if holders == nil {
		return false
	}
	_, ok := holders[volunteerID]
	return ok
}

// flushPendingFor forces an immediate flush of the pending reservation-write queue so
// a freshly handed-out reservation is durable in Postgres before StartWork's run-start
// transaction reads/Assigns the unit. The CALLER (StartWork) already holds an admission
// slot, so this drains without re-acquiring (avoiding a self-deadlock when
// admissionCap == 1). It closes the flush race deterministically: after it returns, a
// landed reservation is durable; an in-memory hold the flush could not land was voided,
// so StartWork's subsequent in-memory check fails closed.
func (c *dispatchCache) flushPendingFor(ctx context.Context) {
	c.flushAllPendingHeld(ctx)
}

// onUnitDone evicts a unit from the in-memory ledger entirely (all holders) and
// decrements their inflight counts. Called when a unit completes / is abandoned /
// run-starts so the cache no longer counts it. Also removes it from the ready pool.
func (c *dispatchCache) onUnitDone(unitID types.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if holders := c.reservedInMem[unitID]; holders != nil {
		for vol := range holders {
			if c.inflight[vol] > 0 {
				c.inflight[vol]--
				if c.inflight[vol] == 0 {
					delete(c.inflight, vol)
				}
			}
		}
		delete(c.reservedInMem, unitID)
	}
	for i := range c.ready {
		if c.ready[i].unit.ID == unitID {
			c.ready = append(c.ready[:i], c.ready[i+1:]...)
			break
		}
	}
}

// onRunStart converts one volunteer's in-memory reservation into an active history
// row (the StartWork transaction wrote it). The cache stops tracking the
// reservation hold but KEEPS the volunteer's inflight count: the slot is still
// occupied, now by an active history row the reconcile counts authoritatively. Any
// other distinct holder of a redundancy>1 unit is preserved. The unit is removed
// from the ready pool (it is now ASSIGNED, no longer QUEUED) when no holder remains.
func (c *dispatchCache) onRunStart(unitID, volunteerID types.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	holders := c.reservedInMem[unitID]
	if holders != nil {
		if _, ok := holders[volunteerID]; ok {
			delete(holders, volunteerID)
			if len(holders) == 0 {
				delete(c.reservedInMem, unitID)
			}
		}
	}
	// Once ASSIGNED the unit is no longer QUEUED; drop it from ready so a stale
	// snapshot is never handed out.
	for i := range c.ready {
		if c.ready[i].unit.ID == unitID {
			c.ready = append(c.ready[:i], c.ready[i+1:]...)
			break
		}
	}
}

// --- leaf metadata cache -----------------------------------------------------

// cachedLeaf is a leaf snapshot plus the time it was read, so getLeaf can bound its
// staleness (leafSnapshotTTL) and re-read after an artifact publish/rollback or a
// direct execution_config change — the fix for RUNNING volunteers keeping the old
// artifact (TODO #38).
type cachedLeaf struct {
	leaf      *leaf.Leaf
	fetchedAt time.Time
}

// peekLeaf returns a cached leaf without a DB fetch (nil if not warmed). Used by the
// hot-path capability check; a slightly stale capability snapshot is harmless (the
// build path re-resolves freshness via getLeaf).
func (c *dispatchCache) peekLeaf(id types.ID) *leaf.Leaf {
	c.leafMu.Lock()
	defer c.leafMu.Unlock()
	if cl := c.leafCache[id]; cl != nil {
		return cl.leaf
	}
	return nil
}

// getLeaf returns the leaf for building an accepted hand-out's assignment. Off the
// hot path. It re-reads from Postgres on a miss OR when the cached snapshot is older
// than leafSnapshotTTL, so a new artifact version propagates to assignments within
// the TTL with no head restart. On a refresh that cannot be admitted (DB pressure) or
// errors, it serves the existing snapshot rather than failing the hand-out.
func (c *dispatchCache) getLeaf(id types.ID) (*leaf.Leaf, error) {
	c.leafMu.Lock()
	cl := c.leafCache[id]
	c.leafMu.Unlock()
	if cl != nil && c.now().Sub(cl.fetchedAt) < c.cfg.leafSnapshotTTL {
		return cl.leaf, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		if cl != nil {
			return cl.leaf, nil // serve the (stale) snapshot under admission pressure
		}
		return nil, ctx.Err()
	}
	defer release()
	lf, err := c.deps.leafRepo.GetByID(ctx, id)
	if err != nil {
		if cl != nil {
			return cl.leaf, nil // serve the (stale) snapshot on a transient read error
		}
		return nil, err
	}
	c.leafMu.Lock()
	c.leafCache[id] = &cachedLeaf{leaf: lf, fetchedAt: c.now()}
	c.leafMu.Unlock()
	return lf, nil
}

// warmLeaf caches a leaf if not present (best-effort, called by the refiller so the
// capability check in eligibleLocked has metadata for newly-staged units). Freshness
// for the build path is handled by getLeaf's TTL, not here.
func (c *dispatchCache) warmLeaf(ctx context.Context, id types.ID) {
	if c.peekLeaf(id) != nil {
		return
	}
	lf, err := c.deps.leafRepo.GetByID(ctx, id)
	if err != nil {
		c.logger.Warn("dispatch cache: failed to warm leaf metadata", "leaf_id", id, "error", err)
		return
	}
	c.leafMu.Lock()
	c.leafCache[id] = &cachedLeaf{leaf: lf, fetchedAt: c.now()}
	c.leafMu.Unlock()
}

// InvalidateLeaf drops a cached leaf snapshot so the next getLeaf re-reads it
// immediately. Called when a leaf's artifact version is published/rolled back (or its
// config changes) so the change reaches assignments at once on THIS replica; other
// replicas converge within leafSnapshotTTL. Safe from any goroutine.
func (c *dispatchCache) InvalidateLeaf(id types.ID) {
	c.leafMu.Lock()
	delete(c.leafCache, id)
	c.leafMu.Unlock()
}

// getVersion returns an immutable artifact version row, caching it for the process
// lifetime (a published version never changes). Off the hot path. (nil, nil) when no
// artifact repo is wired.
func (c *dispatchCache) getVersion(id types.ID) (*leaf.ArtifactVersion, error) {
	if c.deps.artifactVersionRepo == nil {
		return nil, nil
	}
	c.versionMu.Lock()
	v := c.versionCache[id]
	c.versionMu.Unlock()
	if v != nil {
		return v, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return nil, ctx.Err()
	}
	defer release()
	ver, err := c.deps.artifactVersionRepo.GetVersionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	c.versionMu.Lock()
	c.versionCache[id] = ver
	c.versionMu.Unlock()
	return ver, nil
}

// ensurePin pins unitID to currentVersionID if unpinned and returns the effective pin
// (ok=false on DB pressure / error, so the caller falls back to the current config).
func (c *dispatchCache) ensurePin(unitID, currentVersionID types.ID) (types.ID, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return types.ID{}, false
	}
	defer release()
	pinned, err := c.deps.artifactVersionRepo.EnsureWorkUnitPin(ctx, unitID, currentVersionID)
	if err != nil {
		c.logger.Warn("dispatch cache: failed to pin work unit version", "work_unit_id", unitID, "error", err)
		return types.ID{}, false
	}
	return pinned, true
}

// resolvePinnedExecConfig pins the unit (first dispatch) and returns the pinned
// version's execution config when it differs from the leaf's current (denormalized)
// config — else nil (build from leaf.ExecutionConfig). Off the hot path; acquires and
// releases admission per call (never nests acquires).
func (c *dispatchCache) resolvePinnedExecConfig(unitID, currentVersionID types.ID) *leaf.ExecutionConfig {
	pinned, ok := c.ensurePin(unitID, currentVersionID)
	if !ok || pinned == currentVersionID {
		return nil
	}
	ver, err := c.getVersion(pinned)
	if err != nil || ver == nil {
		c.logger.Warn("dispatch cache: failed to load pinned version",
			"work_unit_id", unitID, "version_id", pinned, "error", err)
		return nil
	}
	cfg := ver.ExecutionConfig
	return &cfg
}

// --- volunteer identity cache (Blocker 1: identity off the hot path) ----------

// peekIdentity returns a cached volunteer-identity snapshot without a DB fetch (nil
// on a miss).
func (c *dispatchCache) peekIdentity(id types.ID) *volunteerIdentity {
	c.identityMu.Lock()
	defer c.identityMu.Unlock()
	return c.identityCache[id]
}

// putIdentity warms (or refreshes) the identity snapshot for a volunteer. Called at
// RegisterVolunteer (the natural write point) so the FIRST RequestWorkUnit after a
// registration already resolves in memory, and on a lazy DB refresh.
func (c *dispatchCache) putIdentity(v *volunteer.Volunteer) {
	if v == nil {
		return
	}
	pk := make([]byte, len(v.PublicKey))
	copy(pk, v.PublicKey)
	rts := make([]string, len(v.AvailableRuntimes))
	copy(rts, v.AvailableRuntimes)
	c.identityMu.Lock()
	c.identityCache[v.ID] = &volunteerIdentity{
		publicKey:         pk,
		hardware:          v.HardwareCapabilities,
		availableRuntimes: rts,
	}
	c.identityMu.Unlock()
}

// resolveIdentity returns the volunteer-identity snapshot for id, fetching+caching
// it under the admission semaphore on a miss. The hot path (RequestWorkUnit) calls
// this; a warmed snapshot (the steady state, since RegisterVolunteer pre-warms it)
// resolves entirely in memory with NO pool touch. Only a cold miss hits Postgres,
// and that single read is bounded by the admission semaphore + a short shed timeout
// so it fails fast under overload instead of blocking on the request ctx.
//
// notFound reports a 404 (volunteer unknown). shed reports the DB read could not be
// admitted (pool saturated / timed out) — the caller sheds with ResourceExhausted
// rather than collapsing on a "context deadline exceeded".
func (c *dispatchCache) resolveIdentity(id types.ID) (ident *volunteerIdentity, notFound, shed bool) {
	if v := c.peekIdentity(id); v != nil {
		return v, false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return nil, false, true
	}
	defer release()
	v, err := c.deps.volunteerRepo.GetByID(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, true, false
		}
		// A transient DB error (timeout under load): treat as shed so the volunteer
		// backs off rather than seeing an Internal collapse.
		c.logger.Warn("dispatch cache: identity resolve failed", "volunteer_id", id, "error", err)
		return nil, false, true
	}
	c.putIdentity(v)
	return c.peekIdentity(id), false, false
}

// --- refiller ----------------------------------------------------------------

// runRefiller is the background goroutine that keeps the ready pool topped up. It
// runs on a ticker and on-demand (when a hand-out drains the pool below the low
// watermark). Returns when ctx is done.
func (c *dispatchCache) runRefiller(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = defaultRefillTickInterval
	}
	c.logger.Info("dispatch cache refiller starting",
		"ready_pool_size", c.cfg.readyPoolSize,
		"low_watermark", c.cfg.lowWatermark,
		"refill_batch_size", c.cfg.refillBatchSize,
		"tick", tick)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	// Prime the pool immediately on start.
	c.refillOnce(ctx)
	// FIX 4 observability: count consecutive ticks where the ready pool sits below the
	// low watermark and emit a rate-limited warning. This is the refill-starvation
	// probe the operator currently lacks (the refiller logs nothing after "starting")
	// and doubles as the FIX-4 acceptance signal.
	const lowTickLogEvery = 8 // ~2s at the default 250ms tick
	consecutiveLowTicks := 0
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("dispatch cache refiller stopping")
			return
		case <-ticker.C:
			c.refillOnce(ctx)
			// Service any pending leaf-scoped requests on the tick too, so a starved
			// leaf is unblocked even if its signal was coalesced away.
			c.leafRefillOnce(ctx)
			if c.readyLen() < c.cfg.lowWatermark {
				consecutiveLowTicks++
				if consecutiveLowTicks%lowTickLogEvery == 1 {
					c.logger.Warn("dispatch cache: ready pool below low watermark",
						"ready_len", c.readyLen(),
						"low_watermark", c.cfg.lowWatermark,
						"client_admission_inflight", len(c.admission),
						"maintenance_admission_inflight", len(c.maintenanceAdmission),
						"consecutive_low_ticks", consecutiveLowTicks)
				}
			} else {
				consecutiveLowTicks = 0
			}
		case <-c.refillSignal:
			c.refillOnce(ctx)
		case <-c.leafRefillSignal:
			c.leafRefillOnce(ctx)
		}
	}
}

// refillOnce performs one bulk refill if the pool is below its low watermark and
// there is headroom. It is bounded by the admission semaphore and a short DB
// timeout so a slow pool fails fast instead of piling up.
func (c *dispatchCache) refillOnce(ctx context.Context) {
	c.mu.Lock()
	have := len(c.ready)
	if have >= c.cfg.readyPoolSize || have >= c.cfg.lowWatermark {
		// Either full, or above the low watermark: no refill needed. (A drained pool
		// signals on-demand, which lands here below the watermark.)
		c.mu.Unlock()
		return
	}
	want := c.cfg.refillBatchSize
	if have+want > c.cfg.readyPoolSize {
		want = c.cfg.readyPoolSize - have
	}
	// Exclude every id the cache currently holds in memory (ready + reserved) so a
	// refill never re-stages an in-flight unit (the DB-level backstop).
	excluded := c.excludedIDsLocked()
	c.mu.Unlock()

	if want <= 0 {
		return
	}
	c.fetchAndStage(ctx, want, excluded, nil)
}

// leafRefillOnce services pending on-demand, leaf-scoped refill requests (Blocker 2).
// Unlike refillOnce it does NOT gate on the global low-watermark — its whole purpose
// is to stage units for a starved leaf even when the pool is "full" of a different
// leaf. It is bounded by the same admission semaphore + ready-pool ceiling.
func (c *dispatchCache) leafRefillOnce(ctx context.Context) {
	leafIDs := c.drainLeafRefills()
	if len(leafIDs) == 0 {
		return
	}
	c.mu.Lock()
	have := len(c.ready)
	if have >= c.cfg.readyPoolSize {
		// Pool is genuinely at capacity: cannot stage more without evicting. Re-queue
		// the request so it is retried once a hand-out frees space.
		c.mu.Unlock()
		c.requestLeafRefill(leafIDs)
		return
	}
	want := c.cfg.refillBatchSize
	if have+want > c.cfg.readyPoolSize {
		want = c.cfg.readyPoolSize - have
	}
	excluded := c.excludedIDsLocked()
	c.mu.Unlock()
	if want <= 0 {
		return
	}
	c.fetchAndStage(ctx, want, excluded, leafIDs)
}

// fetchAndStage runs one bounded FindDispatchableBatch (optionally leaf-scoped) and
// appends the results to the ready pool, warming leaf metadata first. Shared by the
// watermark refill and the leaf-scoped refill.
func (c *dispatchCache) fetchAndStage(ctx context.Context, want int, excluded, leafIDs []types.ID) {
	dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
	defer cancel()
	// FIX 4: restock pulls from the SEPARATE maintenance budget so a client write
	// storm holding the client `admission` slots cannot starve cache refill.
	release, ok := c.acquireMaintenance(dbCtx)
	if !ok {
		return // admission/ctx timeout: shed the refill, retry next tick
	}
	defer release()

	// Layer 3: when scale-out is enabled, the refill ATOMICALLY stamps a per-head
	// dispatch claim on each staged unit so no other replica can stage it (claim-on-
	// refill). When disabled (single-replica), fall back to the claim-free Layer-2
	// refill. The claim cost is amortized here at bulk-refill, NOT per request.
	var cands []workunit.DispatchCandidate
	var err error
	if c.cfg.scaleOutEnabled() {
		cands, err = c.deps.wuRepo.ClaimDispatchableBatch(dbCtx, c.cfg.headID, c.cfg.claimLease, want, excluded, leafIDs)
	} else {
		cands, err = c.deps.wuRepo.FindDispatchableBatch(dbCtx, want, excluded, leafIDs)
	}
	if err != nil {
		c.logger.Warn("dispatch cache: refill failed", "error", err, "leaf_scoped", len(leafIDs) > 0)
		return
	}
	if len(cands) == 0 {
		return
	}

	// Warm leaf metadata for the staged units (so eligibleLocked has capability data)
	// before they become visible in the ready pool.
	seenLeaf := make(map[types.ID]struct{})
	staged := make([]candidate, 0, len(cands))
	for _, dc := range cands {
		if _, ok := seenLeaf[dc.LeafID]; !ok {
			seenLeaf[dc.LeafID] = struct{}{}
			c.warmLeaf(dbCtx, dc.LeafID)
		}
		staged = append(staged, candidate{
			unit:                dc.WorkUnit,
			effectiveRedundancy: dc.RedundancyFactor,
			dbActiveCount:       dc.ActiveAssignments,
		})
	}

	c.mu.Lock()
	// Skip any id that became in-memory-held between the snapshot and now (a hand-out
	// raced the refill); SKIP LOCKED + excluded make this rare, but guard anyway.
	for _, cd := range staged {
		uid := cd.unit.ID
		if _, held := c.reservedInMem[uid]; held {
			continue
		}
		if c.readyContainsLocked(uid) {
			continue
		}
		if len(c.ready) >= c.cfg.readyPoolSize {
			break
		}
		c.ready = append(c.ready, cd)
	}
	c.mu.Unlock()
}

// excludedIDsLocked returns the set of ids the cache currently holds (ready units +
// in-memory reservations) so a refill never re-stages an in-flight unit. Caller
// holds mu.
func (c *dispatchCache) excludedIDsLocked() []types.ID {
	out := make([]types.ID, 0, len(c.ready)+len(c.reservedInMem))
	for i := range c.ready {
		out = append(out, c.ready[i].unit.ID)
	}
	for id := range c.reservedInMem {
		out = append(out, id)
	}
	return out
}

func (c *dispatchCache) readyContainsLocked(id types.ID) bool {
	for i := range c.ready {
		if c.ready[i].unit.ID == id {
			return true
		}
	}
	return false
}

// --- flusher -----------------------------------------------------------------

// runFlusher is the background goroutine that drains pendingWrites to Postgres in
// batched multi-row UPDATEs. It flushes every flushInterval or whenever the queue
// reaches flushBatchSize. Returns when ctx is done (with a final best-effort flush).
func (c *dispatchCache) runFlusher(ctx context.Context) {
	c.logger.Info("dispatch cache flusher starting",
		"flush_interval", c.cfg.flushInterval, "flush_batch_size", c.cfg.flushBatchSize)
	ticker := time.NewTicker(c.cfg.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("dispatch cache flusher stopping")
			// Best-effort final flush so freshly handed-out reservations are durable.
			c.flushOnce(context.Background())
			c.flushSpotChecksOnce(context.Background())
			return
		case <-ticker.C:
			c.flushOnce(ctx)
			c.flushSpotChecksOnce(ctx)
		}
	}
}

// flushOnce drains up to flushBatchSize pending reservation writes and persists them
// in one multi-row UPDATE, acquiring the admission semaphore for the DB touch.
// Conflicts (ids the UPDATE did not return) void their in-memory hand-out per the
// no-double-reserve rule.
func (c *dispatchCache) flushOnce(ctx context.Context) {
	c.flushBatch(ctx, true)
}

// flushAllPendingHeld drains the ENTIRE pending reservation queue (looping over
// flushBatchSize-sized batches) WITHOUT acquiring the admission semaphore — the
// caller must already hold an admission slot. StartWork uses this to force a freshly
// handed-out reservation durable inside the flush window (Major 3) without
// self-deadlocking against its own held admission slot when admissionCap == 1.
func (c *dispatchCache) flushAllPendingHeld(ctx context.Context) {
	for {
		c.mu.Lock()
		remaining := len(c.pendingWrites)
		c.mu.Unlock()
		if remaining == 0 {
			return
		}
		c.flushBatch(ctx, false)
	}
}

// flushBatch drains up to flushBatchSize pending reservation writes and persists them
// in one multi-row UPDATE. When acquireAdmission is true it takes an admission slot
// for the DB touch; when false the caller is assumed to already hold one. Conflicts
// (ids the UPDATE did not return) void their in-memory hand-out per the
// no-double-reserve rule.
func (c *dispatchCache) flushBatch(ctx context.Context, acquireAdmission bool) {
	c.mu.Lock()
	if len(c.pendingWrites) == 0 {
		c.mu.Unlock()
		return
	}
	take := len(c.pendingWrites)
	if take > c.cfg.flushBatchSize {
		take = c.cfg.flushBatchSize
	}
	batch := make([]workunit.FlushReservation, take)
	copy(batch, c.pendingWrites[:take])
	c.pendingWrites = c.pendingWrites[take:]
	// Compact the backing array occasionally so it does not grow unbounded.
	if len(c.pendingWrites) == 0 {
		c.pendingWrites = nil
	}
	c.mu.Unlock()

	dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
	defer cancel()
	if acquireAdmission {
		// FIX 4: the ticker flusher's reservation-flush pulls from the SEPARATE
		// maintenance budget so a client write storm cannot starve reservation
		// landing. The held-slot path (flushAllPendingHeld, acquireAdmission=false,
		// called while StartWork holds a CLIENT slot) acquires nothing here, so the
		// cap-1 anti-deadlock is untouched.
		release, ok := c.acquireMaintenance(dbCtx)
		if !ok {
			// Could not get an admission slot: requeue the batch so it is not dropped.
			c.requeueWrites(batch)
			return
		}
		defer release()
	}

	// Layer 3: pass headID + claimLease so the flush also RENEWS this head's dispatch
	// claim on each landed unit (off the hot path), keeping a held-but-unflushed
	// unit's claim from expiring under it. headID == zero (single-replica) disables
	// renewal inside FlushReservations.
	landed, err := c.deps.wuRepo.FlushReservations(dbCtx, batch, c.cfg.headID, c.cfg.claimLease)
	if err != nil {
		// Transient DB error: requeue so the reservations are retried next tick.
		c.requeueWrites(batch)
		c.logger.Warn("dispatch cache: reservation flush failed; requeued", "count", len(batch), "error", err)
		return
	}

	// Void any reservation that did NOT land (a flush conflict): the unit was taken
	// by someone else / no longer QUEUED. Remove the in-memory hold so the cache does
	// not count a reservation it could not persist.
	//
	// NORMAL units are staged to at most ONE concurrent in-memory holder
	// (inMemHolderCap), so a batch never carries two records for the same WorkUnitID
	// and the RETURNING id set maps cleanly to "this volunteer's reservation landed".
	// We still consume landed ids per-occurrence rather than as a plain set so that —
	// even if a same-unit pair ever reached one batch — the single landed row is
	// credited to exactly one record and the other is correctly voided, never leaving
	// a phantom in-memory holder that reconcile cannot clear.
	landedCount := make(map[types.ID]int, len(landed))
	for _, id := range landed {
		landedCount[id]++
	}
	for _, rec := range batch {
		if landedCount[rec.WorkUnitID] > 0 {
			landedCount[rec.WorkUnitID]--
			continue
		}
		c.releaseInMem(rec.WorkUnitID, rec.VolunteerID)
	}
}

// requeueWrites prepends a batch back onto the pending queue (preserving order).
func (c *dispatchCache) requeueWrites(batch []workunit.FlushReservation) {
	if len(batch) == 0 {
		return
	}
	c.mu.Lock()
	c.pendingWrites = append(batch, c.pendingWrites...)
	c.mu.Unlock()
}

// flushSpotChecksOnce drains pending spot-check markings: each is MarkSpotCheck +
// StampReservation + one assignment_history row (the one history-row-at-reservation
// case, unchanged from Layer 1). The unit stays QUEUED so the second corroborating
// volunteer can still be dispatched it. Unlike the NORMAL flush, a spot-check that
// fails to land (the unit is no longer QUEUED) voids the in-memory hold.
func (c *dispatchCache) flushSpotChecksOnce(ctx context.Context) {
	c.mu.Lock()
	if len(c.pendingSpotChecks) == 0 {
		c.mu.Unlock()
		return
	}
	take := len(c.pendingSpotChecks)
	if take > c.cfg.flushBatchSize {
		take = c.cfg.flushBatchSize
	}
	batch := make([]spotCheckWrite, take)
	copy(batch, c.pendingSpotChecks[:take])
	c.pendingSpotChecks = c.pendingSpotChecks[take:]
	if len(c.pendingSpotChecks) == 0 {
		c.pendingSpotChecks = nil
	}
	c.mu.Unlock()

	for _, sc := range batch {
		dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
		// FIX 4: the spot-check landing (MarkSpotCheck + StampReservation + history
		// row) is part of the flusher goroutine and is correctness-bearing for
		// spot-check deferral, so it pulls from the SEPARATE maintenance budget. After
		// FIX 3, Submit/Abandon hold heavier client slots; leaving this on the client
		// budget would let a write storm starve spot-check landing MORE than at HEAD.
		release, ok := c.acquireMaintenance(dbCtx)
		if !ok {
			cancel()
			c.requeueSpotChecks([]spotCheckWrite{sc})
			continue
		}
		if err := c.deps.wuRepo.MarkSpotCheck(dbCtx, sc.workUnitID); err != nil {
			release()
			cancel()
			// Could not mark (unit gone / not QUEUED): void the in-memory hold.
			c.releaseInMem(sc.workUnitID, sc.volunteerID)
			c.logger.Warn("dispatch cache: spot-check mark failed; voided",
				"work_unit_id", sc.workUnitID, "error", err)
			continue
		}
		if _, err := c.deps.wuRepo.StampReservation(dbCtx, sc.workUnitID, sc.volunteerID, time.Until(sc.reservedUntil)); err != nil {
			release()
			cancel()
			c.releaseInMem(sc.workUnitID, sc.volunteerID)
			c.logger.Warn("dispatch cache: spot-check reservation stamp failed; voided",
				"work_unit_id", sc.workUnitID, "error", err)
			continue
		}
		if err := c.deps.assignRepo.Create(dbCtx, &assignment.AssignmentHistoryEntry{
			WorkUnitID:  sc.workUnitID,
			VolunteerID: sc.volunteerID,
			AssignedAt:  c.now().UTC(),
		}); err != nil {
			c.logger.Warn("dispatch cache: spot-check history-row create failed",
				"work_unit_id", sc.workUnitID, "error", err)
			// The spot-check mark + reservation landed; a missing history row is
			// recoverable (the unit is still QUEUED+reserved and re-stageable on lease
			// lapse), so we do NOT void the in-memory hold here.
		}
		release()
		cancel()
	}
}

// requeueSpotChecks prepends spot-check writes back onto the queue.
func (c *dispatchCache) requeueSpotChecks(batch []spotCheckWrite) {
	if len(batch) == 0 {
		return
	}
	c.mu.Lock()
	c.pendingSpotChecks = append(batch, c.pendingSpotChecks...)
	c.mu.Unlock()
}

// pendingWriteCount returns the queued reservation-write count (for tests).
func (c *dispatchCache) pendingWriteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pendingWrites)
}

// --- reconciler --------------------------------------------------------------

// runReconciler periodically rebuilds the per-volunteer inflight counters from the
// authoritative DB counts so crash/drift cannot cause permanent over-admission.
func (c *dispatchCache) runReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce reconciles the in-memory inflight counters with the authoritative
// DB per-volunteer count. The DB count (active history rows + live reservations) is
// authoritative; the in-memory deltas for not-yet-flushed reservations are layered
// on top so a freshly handed-out (still-unflushed) reservation is not under-counted.
func (c *dispatchCache) reconcileOnce(ctx context.Context) {
	dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(dbCtx)
	if !ok {
		return
	}
	defer release()
	dbCounts, err := c.deps.wuRepo.CountActiveByVolunteer(dbCtx)
	if err != nil {
		c.logger.Warn("dispatch cache: inflight reconcile failed", "error", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Count not-yet-flushed in-memory reservations per volunteer (these may not yet
	// be reflected in dbCounts).
	pending := make(map[types.ID]int)
	for _, rec := range c.pendingWrites {
		pending[rec.VolunteerID]++
	}
	for _, rec := range c.pendingSpotChecks {
		pending[rec.volunteerID]++
	}
	next := make(map[types.ID]int)
	for vol, n := range dbCounts {
		next[vol] = n
	}
	for vol, n := range pending {
		next[vol] += n
	}
	c.inflight = next
}

// --- capability matching -----------------------------------------------------

// leafMatchesCapabilities re-checks the volunteer's capability fit against the leaf
// in memory, ported verbatim from FindNextAssignable's SQL predicates (cpu cores,
// max_memory_mb budget, disk, GPU required/vram/vendor/compute-capability, runtime).
func leafMatchesCapabilities(lf *leaf.Leaf, opts workunit.AssignmentOptions) bool {
	rr := lf.ResourceRequirements
	ec := lf.ExecutionConfig

	// CPU cores: leaf min must fit the volunteer's budget.
	if rr.MinCPUCores > opts.MaxCPUCores {
		return false
	}
	// Memory: the container limit (execution_config.max_memory_mb), the single
	// source of truth, must fit the volunteer's budget.
	if ec.MaxMemoryMB > opts.MaxMemoryMB {
		return false
	}
	// Disk.
	if int64(rr.MinDiskMB) > opts.MaxDiskMB {
		return false
	}
	// GPU requirement (resource_requirements.gpu_required): the volunteer must have a
	// GPU with sufficient VRAM.
	if rr.GPURequired {
		if !opts.HasGPU || rr.MinGPUVRAMMB > opts.MaxGPUVRAMMB {
			return false
		}
		// Compute capability, when required.
		if rr.GPUComputeCapability != nil && *rr.GPUComputeCapability != "" {
			if !containsString(opts.GPUComputeCapabilities, *rr.GPUComputeCapability) {
				return false
			}
		}
	}
	// Runtime: leaf runtime must be one the volunteer can run.
	runtime := ec.Runtime
	if runtime == "" {
		runtime = leaf.RuntimeNative
	}
	if !containsString(opts.AvailableRuntimes, runtime) {
		return false
	}
	// GPU vendor/type (execution_config.gpu_required + gpu_type): if the exec config
	// requires a GPU and pins a specific vendor/type, the volunteer must have it.
	if ec.GPURequired {
		gpuType := strings.ToUpper(strings.TrimSpace(ec.GPUType))
		if gpuType != "" && gpuType != "ANY" {
			if !containsString(opts.GPUVendors, gpuType) {
				return false
			}
		}
	}
	return true
}

func containsID(ids []types.ID, target types.ID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// isNotFound reports whether err is a 404 APIError (volunteer/leaf/unit not found).
func isNotFound(err error) bool {
	apiErr, ok := err.(*apierror.APIError)
	return ok && apiErr.HTTPStatus == 404
}
