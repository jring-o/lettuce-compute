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
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/trust"
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
	// reconcileGracePeriod is the minimum age a buffered (un-started) copy must reach
	// before the buffer reconcile may release it as no-longer-held. It protects a copy
	// handed out moments ago from being reaped before the volunteer's next request
	// reports holding it.
	reconcileGracePeriod = 60 * time.Second
	// heldReportFreshness bounds how recent a volunteer's reported held set must be for
	// the buffer reconcile to act on it. A volunteer that has stopped polling (stale
	// report) is not reconciled against — its buffered copies are reclaimed by the
	// deadline instead, so a transient disconnect never wrongly drops its buffer.
	heldReportFreshness = 90 * time.Second
	// trustScoreTTL bounds how long the cache trusts its in-memory snapshot of subject
	// trust scores before the refill path re-reads them. The snapshot feeds only the
	// trusted-corroborator reservation in eligibleLocked, which is stale-tolerant by
	// construction (the SQL landing writes re-check the reservation against fresh scores),
	// so a modest window keeps the read off the hot path at negligible correctness cost.
	trustScoreTTL = 30 * time.Second
	// standingSnapshotTTL bounds how long the cache trusts its in-memory snapshot of the
	// non-OK account-standing population (BG-24b) before the refill path re-reads it. Like
	// trustScoreTTL it is stale-tolerant by construction: the SQL landing gates
	// (FlushReservations / ReserveCopy / FindNextAssignable) recompute standing fresh and
	// are authoritative, so a stale snapshot costs at most a voided hand-out to a
	// just-benched account or a briefly-late bench — never a wrong LANDED copy.
	standingSnapshotTTL = 30 * time.Second
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
	// the authoritative floor on its redundancy headroom. This is the RAW seed (live
	// copies + PENDING results, standing-agnostic); the countable portion subtracts
	// probationCoverage below.
	dbActiveCount int
	// probationCoverage is the NON-COUNTABLE portion of dbActiveCount at refill time
	// (account standing, BG-24b — DispatchCandidate.ProbationCoverage): live copies held by
	// a non-OK account plus PENDING results stamped non-OK. eligibleLocked's COVERAGE bound
	// subtracts it (and the in-memory non-OK holders) so redundancy is closed only by
	// COUNTABLE copies — the same number the SQL headroom enforces — forcing full
	// replication around neutralized copies/results. 0 for an all-OK population, so the
	// coverage arithmetic reduces to today's.
	probationCoverage int
	// contributors is the set of trust SUBJECTS that already count toward this unit's
	// redundancy: live-copy holders + PENDING-result authors at refill time, kept
	// current as copies run-start (onRunStart). A subject is the account-level trust
	// key — a live-bound DID, else the per-keypair "vol:<uuid>" sentinel
	// (trust.SubjectForVolunteer). eligibleLocked excludes them so each of the N
	// redundant results comes from a DISTINCT PRINCIPAL: two devices under one live DID
	// are ONE principal, so handing each a copy of one unit buys no extra corroboration
	// (validation counts them as one subject) and only wastes compute. A subject is
	// never removed once added (a result/copy is monotonic coverage), so a candidate
	// that lingers staged across the submitter's submit still excludes it.
	contributors map[string]struct{}
	// benched is the set of volunteers whose recent copy of this unit timed out / was
	// abandoned within ~one deadline window (a refill-time snapshot). They are given
	// last refusal so a fresh volunteer gets first crack on a requeue; the DB
	// reservation is the authoritative cooldown gate, this is the hand-out optimization.
	benched map[types.ID]struct{}
	// effectiveTrustK is the leaf's resolved trusted-corroborator requirement
	// (DispatchCandidate.EffectiveTrustK): the number of this unit's redundant results
	// that must come from TRUSTED subjects. 0 disables the trusted-corroborator
	// reservation for this candidate (the head trust gate is off, or the leaf requires no
	// trusted corroborators) — eligibleLocked then does ZERO extra work, the gate-off fast
	// path. Non-zero turns on the reservation: the unit's last K-minus-already-present
	// slots are withheld from UNTRUSTED requesters so the quorum can still be completed by
	// trusted results.
	effectiveTrustK int
	// effectiveTrustFloor is the leaf's resolved trust floor
	// (DispatchCandidate.EffectiveTrustFloor): the minimum score at which a subject counts
	// TRUSTED for this unit's reservation. Read against the cache's trust-score snapshot to
	// classify the requester and the post-refill live holders.
	effectiveTrustFloor int
	// trustedContributors is the refill-time snapshot of contributor subjects that already
	// count TRUSTED toward this unit (DispatchCandidate.TrustedContributorSubjects): a
	// live-copy holder whose CURRENT score met the floor, or a PENDING-result author whose
	// STAMPED submission-time score met it. It is FROZEN at refill on purpose — a pending
	// author's verdict counts its stamped score, so its trustedness must never be
	// re-evaluated against a later (drifted) current score. eligibleLocked unions this set
	// with the trusted subjects among the post-refill live copies (current in-memory holds
	// + onRunStart-converted running copies) to size the reservation.
	trustedContributors map[string]struct{}
	// runStartedSubjects is the set of subjects whose in-memory hold was converted to a
	// RUNNING copy AFTER this candidate was staged (recorded by onRunStart). Unlike the
	// refill-time contributors these are post-refill LIVE copies, so their trustedness is
	// evaluated against the CURRENT score snapshot (like an in-memory hold), not frozen —
	// kept separate from the frozen contributors precisely so the reservation does not
	// re-score a refill-time pending author. Only populated when effectiveTrustK > 0.
	runStartedSubjects map[string]struct{}
}

// heldCopy is one account's in-memory reservation on a unit: when it expires (the lease),
// which MACHINE holds it, and the holder's trust SUBJECT. The reservedInMem inner map keys
// on the ACCOUNT (release bookkeeping is per-account — a user's own machines must not
// corroborate each other), but a release must decrement the right host's in-flight count,
// so the host id rides along here (TODO #19); and the self-held distinctness check compares
// PRINCIPALS, so the holder's subject (a live-bound DID, else the "vol:<uuid>" sentinel —
// trust.SubjectForVolunteer, resolved at hand-out) rides along too. The subject can go
// stale across a mid-process bind/revoke, which is SAFE: the SQL landing writes recompute
// subjects fresh and refuse a same-subject copy, so staleness only costs a voided hand-out.
type heldCopy struct {
	reservedUntil time.Time
	hostID        types.ID
	subject       string
}

// meterID returns the effective host id the per-machine metering (in-flight cap, send
// floor) keys on: the VALIDATED server-issued host id when present (BG-25 — the handler
// only populates opts.HostID for an id issued to the requesting account), else the
// account id (the per-account fallback). Matches COALESCE(host_id, volunteer_id) in SQL.
func meterID(volunteerID types.ID, hostID *types.ID) types.ID {
	if hostID != nil {
		return *hostID
	}
	return volunteerID
}

// requesterSubject returns the requester's account-level trust subject for the in-memory
// distinctness checks: opts.TrustSubject when RequestWorkUnit resolved it from the identity
// snapshot (a live-bound DID, else the per-keypair sentinel), else — defensive, for an
// unresolved snapshot or a test that did not populate it — the sentinel of the account id.
// It is the single place the requester subject is read, so the fallback rule lives once.
// Two volunteer rows sharing a live DID resolve to the SAME subject here, so they are
// treated as one principal.
func requesterSubject(volunteerID types.ID, opts workunit.AssignmentOptions) string {
	if opts.TrustSubject != "" {
		return opts.TrustSubject
	}
	return trust.SubjectForVolunteerID(volunteerID)
}

// inMemHolderCap is the maximum number of DISTINCT in-memory reservation holders the
// cache may stage for this candidate concurrently, before the flush has landed.
//
// Per-copy dispatch (migration 00006): each reservation lands as its OWN copy row
// (a work_unit_assignment_history row), not a shared single column, so a redundancy=N
// unit can have up to N live copies AT ONCE, each held by a DISTINCT volunteer. The
// cache therefore stages up to effectiveRedundancy concurrent holders from one ready
// snapshot — the N copies of one unit go out to N different volunteers IN PARALLEL
// (property 7), and run-starting one copy no longer flips the whole unit out of the
// dispatchable universe (the unit stays QUEUED while its copies run). Each holder is
// a distinct volunteer (the self-exclusion in eligibleLocked + the live-copy partial
// unique guarantee no two copies to one volunteer).
func (cd candidate) inMemHolderCap() int {
	return cd.effectiveRedundancy
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
	// minSendInterval is the per-volunteer minimum interval between successful work
	// hand-outs. When > 0, HandOut refuses
	// to hand any new work to a volunteer within this window of its last hand-out — a
	// server-side hard floor on work-acquisition cadence that holds even when a client
	// ignores the advisory server-directed retry delay. 0 disables it.
	minSendInterval time.Duration
	// leafSnapshotTTL bounds staleness of the cached leaf snapshot used to build
	// assignments, so a published/rolled-back artifact version (or a direct
	// execution_config change) propagates to RUNNING volunteers within the TTL with
	// no head restart (TODO #38). 0 -> defaultLeafSnapshotTTL.
	leafSnapshotTTL time.Duration

	// --- TODO #54: reliability-weighted adaptive in-flight quota ---
	//
	// reliabilityQuotaEnabled turns the per-MACHINE in-flight cap into a function of the
	// host's MEASURED reliability (the adaptive "buffer size") instead of the flat
	// maxInflightPerVolunteer. When false, HandOut uses the flat cap exactly as today
	// (byte-for-byte) and the budget cache / refresher are inert. It is also inert when
	// maxInflightPerVolunteer <= 0 (an unbounded cap cannot be shaped).
	reliabilityQuotaEnabled bool
	// reliabilityFloor is the cold-start / fully-throttled in-flight buffer a host with no
	// measured signal gets (a brand-new host, or one not yet warmed after a restart). Small
	// but non-zero (never starves an honest new host) and below the cap (a fresh key never
	// gets the full quota). An honest host ramps from here to maxInflightPerVolunteer over
	// reliability.DefaultRampUnits validated units.
	reliabilityFloor int

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
	// hostRepo resolves per-MACHINE host rows for the per-host runtime cold miss (TODO
	// #19): when a host's advertised runtimes are not warmed in memory (e.g. just after a
	// head restart, before the volunteer re-registers), the hot path reads the
	// authoritative runtimes from the hosts table once and warms them. May be nil
	// (tests / no-pool): the cache then falls back to the account's stored runtimes.
	hostRepo volunteer.HostRepository
	// artifactVersionRepo resolves immutable artifact version rows and pins a unit to
	// a version for homogeneous redundancy (TODO #38). May be nil (legacy / tests):
	// the cache then builds assignments from the leaf's denormalized current
	// execution_config only, with no pinning.
	artifactVersionRepo leaf.ArtifactVersionRepository
	// reliabilityRepo provides the per-host measured-reliability score (TODO #54). The
	// budget refresher reads it OFF the hot path to recompute each host's adaptive in-flight
	// budget; the hand-out path never touches it. May be nil (tests / reliability disabled):
	// the budget refresher is then a no-op and the flat in-flight cap applies.
	reliabilityRepo reliability.Repository
	// trustRepo provides the account-level trust scores for the trusted-corroborator
	// reservation. The refill path reads AllScores OFF the hot path (on trustScoreTTL
	// cadence, riding the maintenance admission slot fetchAndStage already holds) to
	// refresh an in-memory subject -> score snapshot; the hand-out path reads only that
	// snapshot. May be nil (tests / no pool / trust gate never used): the snapshot then
	// stays nil, and since a nil-score map classifies nobody as trusted while every
	// candidate with effectiveTrustK == 0 skips the reservation entirely, nothing changes.
	trustRepo trust.Repository
	// standingRepo provides the non-OK account-standing population (BG-24b) for the
	// BENCHED dispatch gate, the countable-coverage / trusted-present standing filters, and
	// the PROBATION in-flight floor. The refill path reads AllNonOK OFF the hot path (on
	// standingSnapshotTTL cadence, riding the same maintenance slot as the trust read) into
	// an in-memory account -> entry snapshot; the hand-out path reads only that snapshot.
	// May be nil (tests / no pool / standing never used): the snapshot then stays nil and
	// EVERY account resolves OK, so the gates are inert and dispatch behaves as before.
	standingRepo standingSnapshotReader
}

// standingSnapshotReader is the narrow read the dispatch cache needs from the account-
// standing store (internal/standing.Repository): the whole non-OK population in one map
// keyed by account id. A consumer-side interface so tests can substitute a fake without the
// full standing repository, and so the cache never depends on the write surface.
type standingSnapshotReader interface {
	AllNonOK(ctx context.Context) (map[types.ID]standing.Entry, error)
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
	// trustSubject is the volunteer's account-level trust subject at warm time
	// (trust.SubjectForVolunteer): the bound DID while the binding is live (OK or STALE),
	// else the per-keypair "vol:<uuid>" sentinel. RequestWorkUnit copies it into
	// opts.TrustSubject so the hot-path distinctness checks compare PRINCIPALS (two
	// devices under one live DID are one subject) with no DB read. It can go STALE across
	// a mid-process bind/revoke that does not re-register (the snapshot is only re-warmed
	// at RegisterVolunteer / a cold-miss resolve). That is SAFE: the SQL landing writes
	// (FlushReservations / ReserveCopy) recompute the subject fresh and refuse a
	// same-subject copy, so a stale snapshot costs at most one voided hand-out, never a
	// wrong corroboration.
	trustSubject string
}

// spotCheckWrite is one deferred spot-check marking (MarkSpotCheck + ReserveCopy to
// land the spot-check copy row), flushed asynchronously like a normal reservation but
// via a distinct DB shape.
type spotCheckWrite struct {
	workUnitID  types.ID
	volunteerID types.ID
	// hostID attributes the spot-check copy to the requesting machine (TODO #19); nil =
	// no host reported.
	hostID        *types.ID
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
	// reservedInMem maps a handed-out unit id -> the set of distinct ACCOUNTS (volunteer
	// ids) that currently hold an in-memory reservation on it: the in-process
	// no-double-reserve guard and the redundancy>1 multi-holder tracker. The KEY stays the
	// ACCOUNT (per-WU distinctness is per-account — a user's own machines must not
	// corroborate each other); the VALUE records which MACHINE (host id) holds it so a
	// release decrements that host's in-flight count, not the account's (TODO #19).
	reservedInMem map[types.ID]map[types.ID]heldCopy
	// inflight is the per-MACHINE (effective host id) count of live reservations + active
	// history rows. Re-keyed off the account onto the host (TODO #19) so a user's beefy rig
	// and laptop each get their OWN in-flight budget instead of sharing one account cap.
	inflight map[types.ID]int
	// lastHandOut records, per MACHINE (effective host id), the wall-clock time of its most
	// recent SUCCESSFUL work hand-out (taken > 0). When cfg.minSendInterval > 0, HandOut
	// refuses any new work to a machine within that interval of its last hand-out — a
	// server-side, per-machine minimum send interval that does NOT depend on the client
	// honoring the advisory retry delay. Re-keyed off the account onto the host (TODO #19)
	// so each of a user's machines has its own send clock.
	// Pruned of entries older than the interval on the reconcile tick so it cannot grow
	// unbounded with the lifetime host set. Empty/unused when minSendInterval == 0.
	lastHandOut map[types.ID]time.Time
	// pendingWrites is the async copy-reservation write queue (each lands as a
	// RESERVED copy row via FlushReservations).
	pendingWrites []workunit.FlushReservation
	// pendingSpotChecks is the async spot-check marking queue: each is MarkSpotCheck +
	// ReserveCopy (land the spot-check copy row). Kept separate from pendingWrites
	// because it is a different (non-batchable) DB shape.
	pendingSpotChecks []spotCheckWrite
	// trustScores is a TTL snapshot of subject -> current trust score (positively-scored
	// subjects only; see trust.Repository.AllScores). eligibleLocked reads it — under mu —
	// to classify the requester and the post-refill live holders for the trusted-
	// corroborator reservation. It is refreshed OFF the hot path on the refill cadence
	// (refreshTrustScores, called from fetchAndStage where a DB touch already happens),
	// NEVER by a DB call while holding mu (the peekLeaf rule). A nil/empty map means nobody
	// is known trusted — the conservative default: the reservation then withholds a slot
	// rather than admit an untrusted requester in a trusted subject's place. Staleness is
	// SAFE: the SQL landing writes re-check the reservation against fresh scores (mirroring
	// the #86 subject-distinctness precedent), so a stale verdict costs at most a voided
	// hand-out or a briefly withheld slot, never a wrong acceptance.
	trustScores map[string]int
	// trustScoresAt is when trustScores was last refreshed (zero = never), the TTL clock
	// refreshTrustScores checks against trustScoreTTL. Guarded by mu with trustScores.
	trustScoresAt time.Time
	// standingSnapshot is a TTL snapshot of the NON-OK account-standing population (account
	// id -> its raw standing entry; only non-OK accounts appear — see
	// standing.Repository.AllNonOK). eligibleLocked and the coverage/reservation helpers read
	// it — under mu — via effectiveStandingLocked, which resolves each entry through
	// volunteer.EffectiveStanding at read time (so an expired bench reads PROBATION, not
	// BENCHED). Refreshed OFF the hot path on the refill cadence (refreshStanding, from
	// fetchAndStage), NEVER by a DB call while holding mu (the peekLeaf rule). A nil/absent
	// entry means the account is OK — the snapshot only carries the neutralized minority, so
	// a nil map (dep unwired) treats EVERYONE as OK and every standing gate is inert.
	// Staleness is SAFE: the SQL landing gates recompute standing fresh and are
	// authoritative, so a stale verdict costs at most a voided hand-out or a briefly-late
	// bench, never a wrong landed copy (the trustScores precedent).
	standingSnapshot map[types.ID]standing.Entry
	// standingSnapshotAt is when standingSnapshot was last refreshed (zero = never), the TTL
	// clock refreshStanding checks against standingSnapshotTTL. Guarded by mu with the snapshot.
	standingSnapshotAt time.Time

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

	// hostRuntimeCache caches per-MACHINE advertised runtimes keyed by effective host id
	// (TODO #19). RequestWorkUnit resolves the REQUESTING host's runtimes from here so
	// two machines under one account no longer overwrite each other's runtime set on the
	// single volunteers row (the flapping-row bug): a NATIVE-only laptop is never handed
	// container work just because the account's beefy box registered CONTAINER last.
	// Warmed at RegisterVolunteer (the natural write point); a cold miss (e.g. after a
	// head restart, before the volunteer re-registers) falls back to the account's stored
	// runtimes — self-correcting on the next register, and the per-request hardware still
	// gates capability. Guarded by its own mutex so a read never blocks hand-outs.
	hostRuntimeMu    sync.Mutex
	hostRuntimeCache map[types.ID][]string

	// hostOwnerCache caches per-host OWNERSHIP facts (issued host id -> account) plus
	// the work-path last-seen bump throttle, for BG-25's work-path validation: a
	// non-empty host id must have been issued to the requesting account or the request
	// is refused. TTL'd (hostOwnerTTL) — unlike hostRuntimeCache — because expiry is
	// what makes DELETE-based revocation and mint-time eviction land on the hot path;
	// negative outcomes are cached too, bounding unknown-id lookups. See
	// host_identity.go for the methods and semantics (incl. the fold-don't-refuse rule
	// on shed/error).
	hostOwnerMu    sync.Mutex
	hostOwnerCache map[types.ID]*hostOwnerEntry

	// hostBudgetCache maps a machine's effective host id -> its current adaptive in-flight
	// budget (TODO #54), recomputed OFF the hot path by runBudgetRefresher from the
	// reliability store. The hand-out hot path reads ONE entry here under budgetMu (mirrors
	// hostRuntimeCache) with no DB touch. A MISS means a host with no measured signal yet
	// (brand new, or before the first refresh tick) -> the cold-start floor, so a fresh key
	// is throttled until it earns more. Empty / unread when reliabilityQuotaEnabled is false.
	// The whole map is swapped (not mutated in place) on each refresh, so a reader holds a
	// consistent snapshot.
	budgetMu        sync.Mutex
	hostBudgetCache map[types.ID]int

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

	// heldReports records, per MACHINE (effective host id), the set of work units that
	// host last reported holding in its client buffer (NoteVolunteerHeld, set on every
	// RequestWorkUnit). The buffer reconcile (reconcileBuffers) releases buffered
	// reservations a host no longer holds, so a client that drops its buffer (e.g. across
	// a restart) stops being charged for reservations it forgot. Keyed per HOST (TODO #19)
	// because the held set is per-machine: two machines under one key report DIFFERENT
	// buffers, so account-keying would make one machine's report evict the other's copies.
	// Guarded by heldMu, separate from mu so recording a report on the hot path never
	// blocks hand-outs.
	heldMu      sync.Mutex
	heldReports map[types.ID]heldReport

	// flusherDone is closed by runFlusher after its final best-effort flush on
	// shutdown. Drained() exposes it so the shutdown tail can wait for the final
	// flush to finish BEFORE closing the pool (BG-32) — closing first would fail
	// the flush and drop freshly handed-out reservations back to the lease-expiry
	// recovery path.
	flusherDone chan struct{}
}

// heldReport is a MACHINE's most recently reported client-buffer contents (the work units
// it currently holds) plus when it reported them. `at` gates staleness so the reconcile
// only trusts a recent report. account carries the owning account id so the reconcile can
// drop the released unit from the in-memory ledger, whose holders key on the ACCOUNT
// (distinctness is per-account) even though the buffer report keys on the host.
type heldReport struct {
	units   map[types.ID]struct{}
	account types.ID
	at      time.Time
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
		reservedInMem:        make(map[types.ID]map[types.ID]heldCopy),
		inflight:             make(map[types.ID]int),
		lastHandOut:          make(map[types.ID]time.Time),
		leafCache:            make(map[types.ID]*cachedLeaf),
		versionCache:         make(map[types.ID]*leaf.ArtifactVersion),
		identityCache:        make(map[types.ID]*volunteerIdentity),
		hostRuntimeCache:     make(map[types.ID][]string),
		hostOwnerCache:       make(map[types.ID]*hostOwnerEntry),
		hostBudgetCache:      make(map[types.ID]int),
		admission:            make(chan struct{}, cfg.admissionCap),
		maintenanceAdmission: make(chan struct{}, cfg.maintenanceAdmissionCap),
		refillSignal:         make(chan struct{}, 1),
		leafRefillSignal:     make(chan struct{}, 1),
		pendingLeafRefills:   make(map[types.ID]struct{}),
		heldReports:          make(map[types.ID]heldReport),
		flusherDone:          make(chan struct{}),
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
	// hostKey is the requesting MACHINE's effective host id — the key for the per-machine
	// in-flight cap and send-interval floor (TODO #19). It is the account id when the
	// volunteer reported no host, so the metering transparently falls back to per-account.
	// Distinctness keys on the requester's trust SUBJECT (reqSubject), never on hostKey.
	hostKey := meterID(volunteerID, opts.HostID)
	// reqSubject is the requester's account-level trust subject, resolved once for this
	// whole hand-out (opts and volunteerID are fixed): recorded on each accepted hold so
	// the self-held distinctness check compares PRINCIPALS.
	reqSubject := requesterSubject(volunteerID, opts)

	// TODO #54: when the reliability quota is on, the per-machine in-flight cap becomes the
	// host's ADAPTIVE budget (grounded in measured throughput), not the flat configured cap.
	// Resolved once per hand-out from the in-memory budget cache (no DB touch); eligibleLocked
	// then enforces it per candidate exactly as it enforces the flat cap. opts is a value
	// copy, so overriding the field here is scoped to this hand-out. A no-op when the quota
	// is disabled or the flat cap is unbounded.
	opts.MaxInflightPerVolunteer = c.effectiveInflightCap(hostKey, opts.MaxInflightPerVolunteer)

	c.mu.Lock()
	// PROBATION dispatch-budget floor (account standing, BG-24b): a requester the head has
	// neutralized (effective standing non-OK) is pinned to the cold-start reliability floor
	// regardless of the adaptive budget effectiveInflightCap just resolved for it — a
	// neutralized account's results cannot corroborate, so it must not hog dispatch capacity
	// a once-proven-then-benched account would otherwise keep. A BENCHED requester never
	// reaches acceptance (eligibleLocked refuses it outright below), so this floor is what
	// throttles the still-dispatched PROBATION case. Gated on the reliability quota because
	// that is where the floor exists — with a flat cap every account already shares one
	// budget, so there is no adaptive budget to hog. Resolved here under the main lock (the
	// standing snapshot's guard) so it costs no extra lock, before eligibleLocked reads the
	// capped value. Only lowers, never raises (never above the resolved adaptive cap).
	if c.cfg.reliabilityQuotaEnabled && opts.MaxInflightPerVolunteer > c.cfg.reliabilityFloor &&
		c.effectiveStandingLocked(volunteerID) != volunteer.StandingOK {
		opts.MaxInflightPerVolunteer = c.cfg.reliabilityFloor
	}
	// Per-machine minimum send interval: refuse to hand any new work to a machine within
	// cfg.minSendInterval of ITS last successful hand-out. This is a server-side hard floor
	// on per-machine work-acquisition cadence that holds even when a (self-compiled)
	// volunteer ignores the advisory RetryAfterSeconds — the request is still served (it
	// simply returns no work), and the per-pubkey rate limit backstops the polling itself.
	// Keyed per host so a user's rig and laptop each have their own send clock. A zero
	// interval disables the floor; the resolved interval comes from
	// config.EffectiveMinSendIntervalSeconds, which is ENABLED by default.
	if c.cfg.minSendInterval > 0 {
		if last, ok := c.lastHandOut[hostKey]; ok && c.now().Sub(last) < c.cfg.minSendInterval {
			c.mu.Unlock()
			if c.logger.Enabled(context.Background(), slog.LevelDebug) {
				c.logger.Debug("hand-out throttled: min send interval not elapsed",
					"volunteer_id", volunteerID,
					"host_id", hostKey,
					"min_send_interval", c.cfg.minSendInterval)
			}
			return nil, false
		}
	}
	kept := c.ready[:0]
	taken := 0
	// D-1: per-reason tally of why candidates were refused, so a hand-out that returns
	// nothing can explain itself ("why did this volunteer get zero work"). Stack-allocated
	// fixed array — incremented per rejected candidate only, never allocates.
	var rejects [numRejectReasons]int
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
		if ok, reason := c.eligibleLocked(volunteerID, hostKey, opts, cand); !ok {
			rejects[reason]++
			kept = append(kept, cand)
			continue
		}
		// Hold the buffered unit until its head-owned deadline (the buffer window);
		// fall back to the configured lease only when the unit has no deadline.
		reservedUntil := c.now().UTC().Add(leaseFallback)
		if cand.unit.DeadlineSeconds > 0 {
			reservedUntil = c.now().UTC().Add(time.Duration(cand.unit.DeadlineSeconds) * time.Second)
		}
		// Accept this candidate as a reservation for volunteerID (the ACCOUNT — distinctness
		// keys here) held by hostKey (the MACHINE — in-flight metering keys there).
		uid := cand.unit.ID
		holders := c.reservedInMem[uid]
		if holders == nil {
			holders = make(map[types.ID]heldCopy)
			c.reservedInMem[uid] = holders
		}
		holders[volunteerID] = heldCopy{reservedUntil: reservedUntil, hostID: hostKey, subject: reqSubject}
		c.inflight[hostKey]++

		// HR pin (in-memory, first-writer-wins): the first holder of an unpinned unit on
		// an HR-enabled leaf pins it to that holder's class HERE, under c.mu, so the very
		// next hand-out (which must re-acquire c.mu) is already constrained to the same
		// class by eligibleLocked — closing the window where one ready snapshot could hand
		// copies to two different classes before any DB pin lands. The durable pin is
		// written off-lock in the metadata loop below; a re-stage from DB rehydrates it.
		if cand.unit.HRClass == nil && opts.HRClass != "" {
			if lf := c.peekLeaf(cand.unit.LeafID); lf != nil && lf.ValidationConfig.HomogeneousRedundancy {
				cls := opts.HRClass
				cand.unit.HRClass = &cls
			}
		}

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
				hostID:        opts.HostID,
				reservedUntil: reservedUntil,
			})
		} else {
			c.pendingWrites = append(c.pendingWrites, workunit.FlushReservation{
				WorkUnitID:  uid,
				VolunteerID: volunteerID,
				// Per-machine attribution (TODO #19): the copy row records which machine
				// reserved it. Metering (inflight / send floor) is re-keyed onto the host
				// separately; this is the durable attribution half.
				HostID:          opts.HostID,
				ReservedUntil:   reservedUntil,
				DeadlineSeconds: cand.unit.DeadlineSeconds,
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

		// Keep the candidate staged while it still has redundancy headroom for another
		// DISTINCT volunteer, so the SAME ready snapshot hands the N copies of one unit
		// to N different volunteers in parallel (property 7). Dropped once its copies
		// are exhausted; a copy that later times out frees a slot and the refiller
		// re-stages the unit for a fresh distinct volunteer.
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
	readyLen := len(c.ready)
	readyNonEmpty := readyLen > 0
	drained = readyLen < c.cfg.lowWatermark
	// Stamp the per-MACHINE send clock ONLY when work was actually handed out, so a
	// machine that got nothing (no eligible work) may retry immediately and the interval
	// governs only the spacing between real hand-outs.
	if taken > 0 && c.cfg.minSendInterval > 0 {
		c.lastHandOut[hostKey] = c.now()
	}
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
				"work_unit_id", r.unit.ID, "leaf_id", r.unit.LeafID, "volunteer_id", volunteerID, "error", err)
			continue
		}
		r.leaf = lf
		// Artifact pinning (TODO #38): on a versioned leaf, pin EVERY unit to the
		// current version at its first dispatch (first-writer-wins). This gives every
		// work unit and result exact per-unit version provenance
		// and is what makes redundant replicas of one unit run a homogeneous version. If
		// a unit was ALREADY pinned to a different version (e.g. a reassignment after a
		// mid-flight publish), the assignment is built from the PINNED version, not the
		// leaf's current one. Unversioned leaves (no current version) keep the legacy
		// path with no pin.
		if c.deps.artifactVersionRepo != nil && lf.CurrentArtifactVersionID != nil {
			r.execConfig = c.resolvePinnedExecConfig(r.unit.ID, *lf.CurrentArtifactVersionID)
		}
		// HR durable pin (first-writer-wins): persist the hardware-class pin set in memory
		// during hand-out so it survives a re-stage / restart / cross-replica and gates the
		// DB-fallback FindNextAssignable. Off the hot lock, under the admission semaphore.
		if c.deps.wuRepo != nil && lf.ValidationConfig.HomogeneousRedundancy && opts.HRClass != "" {
			c.ensureHRPin(r.unit.ID, opts.HRClass)
		}
		final = append(final, r)
	}
	if drained {
		c.signalRefill()
	}
	// D-2 / D-1: per-call hand-out summary, and (when nothing was handed out) the reject
	// tally that explains it. Guarded behind a single Enabled check so the steady-state
	// hot path allocates nothing when Debug is disabled (the production default).
	if c.logger.Enabled(context.Background(), slog.LevelDebug) {
		c.logger.Debug("hand-out",
			"volunteer_id", volunteerID,
			"requested", n,
			"handed", len(final),
			"ready_len", readyLen,
			"drained", drained)
		if taken == 0 {
			attrs := make([]any, 0, 4+2*numRejectReasons)
			attrs = append(attrs, "volunteer_id", volunteerID, "ready_len", readyLen)
			for r := rejectReason(1); r < numRejectReasons; r++ {
				if rejects[r] > 0 {
					attrs = append(attrs, r.String(), rejects[r])
				}
			}
			c.logger.Debug("hand-out empty: reject tally", attrs...)
		}
	}
	return final, drained
}

// rejectReason explains why eligibleLocked refused a candidate for a volunteer. It
// is the per-candidate answer to "why did this volunteer get zero work": HandOut
// tallies these across the ready-pool scan and, when nothing was handed out, logs the
// non-zero counts. The iota order has no meaning beyond being a stable array index.
type rejectReason int

const (
	rejectNone               rejectReason = iota // eligible (handed out)
	rejectRedundancyFull                         // redundancy headroom exhausted (db active + in-mem holders)
	rejectHolderCap                              // concurrent in-mem holder cap reached
	rejectSelfHeld                               // volunteer already holds an in-mem copy of this unit
	rejectAlreadyContributed                     // volunteer already contributed (distinctness)
	rejectBenched                                // volunteer benched (post-failure cooldown)
	rejectInflightCap                            // per-volunteer inflight cap reached
	rejectLeafFilter                             // unit's leaf not in the requested LeafIDs
	rejectBlockedLeaf                            // unit's leaf is blocked for this volunteer
	rejectHRClassMismatch                        // unit pinned to a different hardware class
	rejectLeafNotCached                          // leaf metadata not yet warmed
	rejectCapabilityMismatch                     // volunteer capabilities do not fit the leaf
	rejectInfeasibleDeadline                     // host too slow to finish this unit before its deadline
	rejectTrustReserved                          // untrusted requester; unit's last slots reserved for trusted subjects
	rejectStandingBenched                        // requester's account is BENCHED (no dispatch until the bench lapses)
	numRejectReasons                             // sentinel: count of reasons (tally array size)
)

// String returns the canonical field-key form for a reject reason, used as the slog
// key in HandOut's "reject tally" line.
func (r rejectReason) String() string {
	switch r {
	case rejectNone:
		return "eligible"
	case rejectRedundancyFull:
		return "redundancy_full"
	case rejectHolderCap:
		return "holder_cap"
	case rejectSelfHeld:
		return "self_held"
	case rejectAlreadyContributed:
		return "already_contributed"
	case rejectBenched:
		return "benched_cooldown"
	case rejectInflightCap:
		return "inflight_cap"
	case rejectLeafFilter:
		return "leaf_filter"
	case rejectBlockedLeaf:
		return "blocked_leaf"
	case rejectHRClassMismatch:
		return "hr_class_mismatch"
	case rejectLeafNotCached:
		return "leaf_not_cached"
	case rejectCapabilityMismatch:
		return "capability_mismatch"
	case rejectInfeasibleDeadline:
		return "infeasible_deadline"
	case rejectTrustReserved:
		return "trust_reserved"
	case rejectStandingBenched:
		return "standing_benched"
	default:
		return "unknown"
	}
}

// eligibleLocked re-checks every per-requester predicate in memory against the
// cached candidate. Ported verbatim from FindNextAssignable's SQL. Caller holds mu.
// It returns whether the candidate is eligible and, when not, the specific
// rejectReason so HandOut can tally why a volunteer was handed nothing. Callers that
// do not need the reason discard the second value.
//
// The distinctness rules (self-held copy, already-contributed) are per-PRINCIPAL: each
// of a unit's N redundant results must come from a distinct trust SUBJECT, so two
// volunteer rows sharing one live DID (one principal) never both hold or contribute to
// the same unit — validation counts them as one, so a second copy only wastes compute.
// The post-failure cooldown ("benched") and the per-machine in-flight cap deliberately
// stay keyed on the account / host, not the subject: they are reliability / metering
// signals, not corroboration distinctness.
//
// volunteerID is the ACCOUNT (the key for redundancy headroom release bookkeeping and the
// per-account post-failure cooldown). hostKey is the requesting MACHINE's effective host
// id (the key for the in-flight cap, per-machine by TODO #19) — distinct so a user's rig
// and laptop get independent in-flight budgets. The requester's SUBJECT (for distinctness)
// is resolved from opts via requesterSubject.
func (c *dispatchCache) eligibleLocked(volunteerID, hostKey types.ID, opts workunit.AssignmentOptions, cand candidate) (bool, rejectReason) {
	uid := cand.unit.ID
	leafID := cand.unit.LeafID
	subject := requesterSubject(volunteerID, opts)

	// Redundancy bounds, enforced by TWO checks that key on DIFFERENT numbers by design:
	//   (1) COVERAGE (corroboration): only COUNTABLE copies close a unit's redundancy need
	//       (account standing, BG-24b). The countable coverage is the RAW db seed minus its
	//       non-countable portion (cand.probationCoverage — non-OK live holders + non-OK
	//       pending results at refill) PLUS the in-memory holders whose ACCOUNT standing is
	//       OK (countableHoldersLocked). A unit whose copies are all held by neutralized
	//       accounts has zero countable coverage, so a fresh OK requester still finds
	//       headroom — full replication FORCED around neutralized results. Mirrors the SQL
	//       countableCoverageSQL headroom. With an all-OK population it reduces to
	//       dbActiveCount + len(holders), byte-for-byte today's arithmetic.
	//   (2) CONCURRENCY cap (RAW, standing-agnostic): at most inMemHolderCap() ==
	//       effectiveRedundancy DISTINCT holders may hold this unit AT ONCE regardless of
	//       standing — it bounds simultaneous work/compute, NOT corroboration coverage, so a
	//       neutralized holder still occupies a concurrency slot. (Forced replication happens
	//       over TIME: as a neutralized copy completes or lapses its slot frees and the
	//       refiller re-stages the unit for a fresh OK volunteer — the coverage bound above,
	//       not this cap, is what keeps it dispatchable.)
	holders := c.reservedInMem[uid]
	countableCoverage := cand.dbActiveCount - cand.probationCoverage + c.countableHoldersLocked(holders)
	if countableCoverage < 0 {
		countableCoverage = 0 // defensive: snapshot skew can never push coverage below zero
	}
	if countableCoverage >= cand.effectiveRedundancy {
		return false, rejectRedundancyFull
	}
	if len(holders) >= cand.inMemHolderCap() {
		return false, rejectHolderCap
	}
	// Trusted-corroborator reservation: a unit whose leaf requires K TRUSTED
	// corroborators keeps its LAST slots reserved for trusted subjects, so an UNTRUSTED
	// volunteer cannot consume a slot the quorum still needs a trusted result to fill. This
	// is the in-memory mirror of the SQL reservation the landing writes enforce.
	//
	// Gate-off fast path: when the leaf resolves no trusted requirement (effectiveTrustK ==
	// 0 — the head trust gate is disabled, or the leaf asks for no trusted corroborators)
	// the whole block is skipped, so a non-trust deployment does ZERO extra work here and
	// behaves byte-for-byte as before.
	//
	// With K > 0: the requester is TRUSTED iff its CURRENT snapshot score meets the leaf's
	// floor, and a trusted requester is NEVER blocked by the reservation (it can fill a
	// reserved slot itself). An UNTRUSTED requester is refused iff handing it this copy
	// would leave too few of the unit's remaining slots for the trusted results the quorum
	// still requires:
	//     countableCoverage + 1 + max(0, K - trustedPresent) > effectiveRedundancy
	// using the SAME countable coverage as the redundancy bound above (account standing,
	// BG-24b: a neutralized copy/result covers nothing, so it must not be counted here
	// either) and trustedPresent the number of DISTINCT trusted-AND-countable subjects
	// already counting toward the unit (see trustedPresentLocked). Below that bound an
	// untrusted copy still leaves room for the outstanding trusted results, so it is admitted.
	//
	// STALENESS: trustScores / standingSnapshot are TTL snapshots refreshed off the hot path
	// on the refill cadence (never a DB read under mu). A stale verdict is self-correcting —
	// the SQL landing re-checks the reservation against fresh scores/standing (the #86
	// precedent) — so it costs at most a voided hand-out (a copy the landing then refuses) or
	// a briefly withheld slot, never a wrong acceptance into a trusted subject's place.
	if cand.effectiveTrustK > 0 {
		if c.trustScores[subject] < cand.effectiveTrustFloor {
			reservedForTrusted := cand.effectiveTrustK - c.trustedPresentLocked(cand)
			if reservedForTrusted < 0 {
				reservedForTrusted = 0
			}
			if countableCoverage+1+reservedForTrusted > cand.effectiveRedundancy {
				return false, rejectTrustReserved
			}
		}
	}
	// Self-exclusion (per PRINCIPAL): never hand this requester a unit any of whose current
	// in-memory holders shares its trust subject — the requester's own account, or another
	// of its devices under the same live DID (one principal cannot corroborate itself). The
	// holder map stays keyed on the account for release bookkeeping, so the check scans the
	// holders (at most redundancy of them, trivially cheap) comparing subjects. The direct
	// account-key check stays IN ADDITION to the subject scan: a hold's subject is a
	// hand-out-time snapshot that can lag a mid-process bind/revoke, but the account key
	// cannot — the requester's own live hold must be refused no matter how its subject has
	// since moved.
	if _, held := holders[volunteerID]; held {
		return false, rejectSelfHeld
	}
	for _, hc := range holders {
		if hc.subject == subject {
			return false, rejectSelfHeld
		}
	}
	// Distinctness (per PRINCIPAL): never hand this requester a unit its subject already
	// contributed to — a live copy held elsewhere or an already-submitted (still-PENDING)
	// result, by any device of the same principal. Each of a unit's N redundant results
	// must come from a DISTINCT subject; without this a unit re-queued for corroboration is
	// re-handed to its own prior submitter (or that submitter's other device), which can
	// only run it and have the duplicate result rejected. The DB reservation is the
	// authoritative gate; this avoids the wasted hand-out entirely.
	if _, did := cand.contributors[subject]; did {
		return false, rejectAlreadyContributed
	}
	// Post-failure cooldown: a volunteer whose recent copy of this unit timed out or was
	// abandoned is benched for ~one deadline so a fresh volunteer gets first crack. Keyed on
	// the ACCOUNT, not the subject, by design — the cooldown is a per-account reliability
	// signal (this account's copy failed), not corroboration distinctness.
	if _, benched := cand.benched[volunteerID]; benched {
		return false, rejectBenched
	}
	// Account standing — BENCHED requester (BG-24b): an account the head has BENCHED gets NO
	// dispatch at all until its bench lapses. The per-ACCOUNT standing twin of the per-unit
	// cooldown just above (both are "this requester may not take work right now"), keyed on
	// the account like the cooldown and read from the TTL standing snapshot. Only a LIVE
	// bench refuses — effectiveStandingLocked resolves an expired bench to PROBATION, and a
	// PROBATION account is still dispatched (its results simply never corroborate; the
	// coverage/reservation arithmetic above and the in-flight floor neutralize it instead).
	// Absent snapshot / nil dep ⇒ OK ⇒ inert, so a non-standing deployment is unchanged.
	if c.effectiveStandingLocked(volunteerID) == volunteer.StandingBenched {
		return false, rejectStandingBenched
	}
	// Per-MACHINE inflight cap (TODO #19): a user's beefy rig is not throttled to its
	// laptop's share — each host has its own live-copy budget. Keyed on hostKey, which is
	// the account id in the per-account fallback (so the cap is unchanged for a volunteer
	// that reports no host).
	if opts.MaxInflightPerVolunteer > 0 && c.inflight[hostKey] >= opts.MaxInflightPerVolunteer {
		return false, rejectInflightCap
	}
	// Leaf-id filter (preferred leafs) and blocked-leaf filter.
	if len(opts.LeafIDs) > 0 && !containsID(opts.LeafIDs, leafID) {
		return false, rejectLeafFilter
	}
	if containsID(opts.BlockedLeafIDs, leafID) {
		return false, rejectBlockedLeaf
	}
	// Homogeneous Redundancy: once a unit is pinned to a hardware class, only volunteers
	// of that SAME class may take a copy (so redundant results are bit-comparable).
	// Unpinned units (hr_class == nil, incl. every non-HR leaf) are unconstrained.
	if cand.unit.HRClass != nil && *cand.unit.HRClass != "" && *cand.unit.HRClass != opts.HRClass {
		return false, rejectHRClassMismatch
	}
	// Capability fit against the cached leaf metadata.
	lf := c.peekLeaf(leafID)
	if lf == nil {
		// Leaf not yet cached: be conservative and skip; the next refill / a warmed
		// cache lets it through. (getLeaf is not called under mu to avoid a DB touch
		// while locked.)
		return false, rejectLeafNotCached
	}
	if !leafMatchesCapabilities(lf, opts) {
		return false, rejectCapabilityMismatch
	}
	// Feasibility-at-dispatch: don't hand this host a unit its measured benchmark says
	// it can't finish before the deadline — the head re-offers it to a faster volunteer
	// instead of this host burning the whole deadline window on a run that the runtime
	// would kill at the timeout. Skipped (feasible) when any input is unknown. Mirrors
	// the SQL gate in FlushReservations/ReserveCopy/FindNextAssignable.
	if !workunit.FeasibleByDeadline(lf.ExecutionConfig.RscFpopsEst, opts.BenchmarkFPOPS, cand.unit.DeadlineSeconds) {
		return false, rejectInfeasibleDeadline
	}
	return true, rejectNone
}

// trustedPresentLocked returns how many DISTINCT trusted subjects already count toward
// cand's trusted-corroborator quorum, the trustedPresent term of eligibleLocked's
// reservation gate. It unions three sources, deduped by subject string:
//
//	(a) cand.trustedContributors — the refill-time snapshot of contributor subjects that
//	    counted trusted then (a live holder by its score, a PENDING author by its STAMPED
//	    submission-time score). FROZEN: these are taken as-is and never re-scored, because a
//	    pending author's verdict counts its stamped score, so re-scoring it against a later
//	    current value would diverge from what validation will actually credit.
//	(b) the current in-memory holds (heldCopy.subject) whose CURRENT snapshot score meets
//	    the floor AND whose ACCOUNT's current standing is OK — post-refill live copies,
//	    evaluated live. A neutralized (PROBATION/BENCHED) holder cannot corroborate, so it is
//	    not trusted-present even if its subject scores above the floor (account standing,
//	    BG-24b) — the live-arm twin of the standing filter now in trustedContributorSubjectsSQL.
//	(c) cand.runStartedSubjects — subjects converted from a hold to a RUNNING copy after
//	    staging (onRunStart), also post-refill live copies, evaluated by CURRENT score the
//	    same way as (b). Kept apart from (a) precisely so a refill-time pending author is
//	    never swept into the live-scored set. UNLIKE (a) and (b), arm (c) is deliberately NOT
//	    standing-filtered (BG-24b): runStartedSubjects are bare subject strings with no
//	    account id to resolve against the standing snapshot. The error direction is bounded
//	    and safe — a neutralized run-started subject can only OVER-count trusted_present,
//	    which makes the in-memory reservation verdict slightly MORE permissive (it may admit
//	    an untrusted requester the reservation would otherwise withhold), and that
//	    self-corrects at the standing-filtered SQL landing (a voided hand-out at worst — the
//	    #86/#87 staleness class), never a wrong LANDED copy. Tightening it would require
//	    stamping account ids onto run-started entries; evaluated and skipped as net-new
//	    coupling for a stale-tolerant optimization.
//
// Caller holds mu (it reads reservedInMem and the trust-score / standing snapshots). Only
// reached for a candidate with effectiveTrustK > 0, so the map allocation stays off the
// gate-off path.
func (c *dispatchCache) trustedPresentLocked(cand candidate) int {
	floor := cand.effectiveTrustFloor
	present := make(map[string]struct{}, len(cand.trustedContributors)+len(cand.runStartedSubjects))
	for s := range cand.trustedContributors {
		present[s] = struct{}{}
	}
	for acct, hc := range c.reservedInMem[cand.unit.ID] {
		if hc.subject != "" && c.trustScores[hc.subject] >= floor &&
			c.effectiveStandingLocked(acct) == volunteer.StandingOK {
			present[hc.subject] = struct{}{}
		}
	}
	for s := range cand.runStartedSubjects {
		if c.trustScores[s] >= floor {
			present[s] = struct{}{}
		}
	}
	return len(present)
}

// effectiveStandingLocked resolves the requester/holder ACCOUNT's CURRENT effective standing
// (BG-24b) from the TTL snapshot. Caller holds mu. An account ABSENT from the snapshot is OK
// (the snapshot carries only the non-OK minority); a present entry is resolved through
// volunteer.EffectiveStanding against now() so a live bench reads BENCHED, an EXPIRED bench
// reads PROBATION, and a stored probation reads PROBATION. A nil snapshot (dep unwired /
// never refreshed) ⇒ everyone OK, so every standing gate is inert.
func (c *dispatchCache) effectiveStandingLocked(accountID types.ID) string {
	e, ok := c.standingSnapshot[accountID]
	if !ok {
		return volunteer.StandingOK
	}
	return volunteer.EffectiveStanding(e.Standing, e.BenchedUntil, c.now())
}

// countableHoldersLocked returns how many of the given in-memory holders CORROBORATE — i.e.
// whose ACCOUNT's current effective standing is OK (BG-24b). A holder held by a
// PROBATION/BENCHED account occupies a concurrency slot but does not cover redundancy, so
// eligibleLocked's COVERAGE bound counts only these; the holder map keys on the ACCOUNT
// (volunteer id), exactly the standing snapshot's key. Caller holds mu. An empty / all-OK
// population makes this len(holders), so the coverage arithmetic reduces to today's.
func (c *dispatchCache) countableHoldersLocked(holders map[types.ID]heldCopy) int {
	n := 0
	for acct := range holders {
		if c.effectiveStandingLocked(acct) == volunteer.StandingOK {
			n++
		}
	}
	return n
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
	hc, ok := holders[volunteerID]
	if !ok {
		return
	}
	delete(holders, volunteerID)
	if len(holders) == 0 {
		delete(c.reservedInMem, unitID)
	}
	// Decrement the count of the MACHINE that held this copy, not the account (TODO #19).
	if c.inflight[hc.hostID] > 0 {
		c.inflight[hc.hostID]--
		if c.inflight[hc.hostID] == 0 {
			delete(c.inflight, hc.hostID)
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

// voidNonLandedCopy reverses a hand-out the DB flush refused (FlushReservations returned the
// copy un-landed) and benches the volunteer on the still-staged candidate so the cache stops
// re-offering an un-reservable unit to the same volunteer.
//
// A flush conflict is usually the POST-FAILURE COOLDOWN — a recent EXPIRED/ABANDONED copy of
// this unit benches the volunteer for ~one deadline (FlushReservations / ReserveCopy enforce it
// authoritatively) — but can also be a live copy already held or redundancy already met. The
// ready candidate keeps its REFILL-TIME benched/contributor snapshot, taken BEFORE this
// rejection; eligibleLocked reads that snapshot, so without recording the rejection here HandOut
// re-offers the same unit to the same volunteer every tick. That is a tight LIVELOCK when the
// volunteer is the only one polling for the unit's leaf — e.g. a small (2-volunteer)
// redundancy=2 pool where one volunteer is benched: the unit still needs that volunteer's copy,
// the DB refuses it, and it is handed back, refused, and re-fetched forever (the volunteer burns
// its whole buffer on run-start-denied units). Marking it benched in memory makes eligibleLocked
// exclude the volunteer until the candidate is next re-staged with a fresh DB snapshot (which
// re-benches it if the cooldown still holds, or admits it once the cooldown has lapsed).
func (c *dispatchCache) voidNonLandedCopy(unitID, volunteerID types.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseInMemLocked(unitID, volunteerID)
	// releaseInMemLocked drops the hold but keeps the candidate staged (for its remaining
	// redundancy copies), so it is still here to bench.
	for i := range c.ready {
		if c.ready[i].unit.ID == unitID {
			if c.ready[i].benched == nil {
				c.ready[i].benched = make(map[types.ID]struct{})
			}
			c.ready[i].benched[volunteerID] = struct{}{}
			return
		}
	}
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
		for _, hc := range holders {
			// Decrement the MACHINE that held the copy, not the account (TODO #19).
			if c.inflight[hc.hostID] > 0 {
				c.inflight[hc.hostID]--
				if c.inflight[hc.hostID] == 0 {
					delete(c.inflight, hc.hostID)
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

// onRunStart converts one volunteer's in-memory reservation hold into a RUNNING copy
// (the StartWork transaction set started_at on the copy row). The cache drops the
// reservation hold but KEEPS the volunteer's inflight count (the slot is still
// occupied, now by a live RUNNING copy the reconcile counts authoritatively).
//
// Per-copy dispatch: the WORK UNIT stays QUEUED while its copies run, so the cache
// keeps it staged for its REMAINING redundancy copies — it moves the run-started
// holder from the in-memory holder set into the candidate's dbActiveCount so the
// accounting is exact, and only evicts the unit once its redundancy is fully covered.
// A redundancy=1 unit (or one whose copies are now all accounted) is evicted.
func (c *dispatchCache) onRunStart(unitID, volunteerID types.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	holders := c.reservedInMem[unitID]
	// Capture the run-starting holder's trust SUBJECT before dropping its hold, so the
	// contributor recorded on the staged candidate below is the PRINCIPAL (matching the
	// subject-keyed contributor set), not the account. Fall back to the sentinel of the
	// account id when there is no holder entry (e.g. a reservation this replica did not
	// hand out in memory — recovered from the DB after a restart).
	subject := trust.SubjectForVolunteerID(volunteerID)
	if holders != nil {
		if hc, ok := holders[volunteerID]; ok {
			if hc.subject != "" {
				subject = hc.subject
			}
			delete(holders, volunteerID)
			if len(holders) == 0 {
				delete(c.reservedInMem, unitID)
			}
		}
	}
	for i := range c.ready {
		if c.ready[i].unit.ID == unitID {
			// The run-started copy is now a live DB row: move it from the holder set
			// into dbActiveCount so eligibleLocked still sees the correct coverage.
			c.ready[i].dbActiveCount++
			// Record this principal as a contributor on the STAGED candidate. The unit
			// stays QUEUED while a redundancy>1 copy runs, so the candidate lingers in the
			// ready pool across this volunteer's submit (which does not meet redundancy and
			// so does not evict it). Its refill-time contributor snapshot predates this
			// run-start, so without recording it here the same subject would become
			// eligible again the moment its in-memory hold is released — re-handed its own
			// unit. Adding it now keeps the staged candidate's distinctness correct.
			if c.ready[i].contributors == nil {
				c.ready[i].contributors = make(map[string]struct{})
			}
			c.ready[i].contributors[subject] = struct{}{}
			// Trusted-corroborator reservation: also record this run-started subject as a
			// post-refill live copy so trustedPresentLocked can count it (evaluated against
			// the CURRENT score snapshot, like an in-memory hold — NOT frozen like the
			// refill-time contributors). Kept off the gate-off path: only tracked when the
			// candidate carries a trusted requirement.
			if c.ready[i].effectiveTrustK > 0 {
				if c.ready[i].runStartedSubjects == nil {
					c.ready[i].runStartedSubjects = make(map[string]struct{})
				}
				c.ready[i].runStartedSubjects[subject] = struct{}{}
			}
			remainingHolders := len(c.reservedInMem[unitID])
			if c.ready[i].dbActiveCount+remainingHolders >= c.ready[i].effectiveRedundancy {
				// Redundancy fully covered: drop it from ready (the refiller re-stages it
				// if a copy later times out and frees a slot).
				c.ready = append(c.ready[:i], c.ready[i+1:]...)
			}
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

// ensureHRPin durably stamps the homogeneous-redundancy hardware class on a unit
// (first-writer-wins). Mirrors ensurePin: off the hot lock, under the admission
// semaphore, best-effort (a failed pin just means the next hand-out retries it — the
// in-memory pin already constrains same-process dispatch).
func (c *dispatchCache) ensureHRPin(unitID types.ID, class string) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return
	}
	defer release()
	if _, err := c.deps.wuRepo.EnsureWorkUnitHRClass(ctx, unitID, class); err != nil {
		c.logger.Warn("dispatch cache: failed to pin work unit hr_class", "work_unit_id", unitID, "error", err)
	}
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
		// Trust subject resolved through the production rule (the single source of truth):
		// the bound DID while live (OK/STALE), else the per-keypair sentinel.
		trustSubject: trust.SubjectForVolunteer(v),
	}
	c.identityMu.Unlock()
}

// putHostRuntimes warms (or refreshes) the advertised-runtimes snapshot for a machine,
// keyed by effective host id (TODO #19). Called at RegisterVolunteer when the volunteer
// reports a host, so the first RequestWorkUnit from that machine resolves its own
// runtimes in memory.
func (c *dispatchCache) putHostRuntimes(hostID types.ID, runtimes []string) {
	rts := make([]string, len(runtimes))
	copy(rts, runtimes)
	c.hostRuntimeMu.Lock()
	c.hostRuntimeCache[hostID] = rts
	c.hostRuntimeMu.Unlock()
}

// peekHostRuntimes returns a machine's advertised runtimes without a DB fetch (nil,false
// on a miss). The hot path uses it to resolve the REQUESTING host's runtimes; a miss
// falls back to resolveHostRuntimes (a bounded DB read) and then the account's runtimes.
func (c *dispatchCache) peekHostRuntimes(hostID types.ID) ([]string, bool) {
	c.hostRuntimeMu.Lock()
	defer c.hostRuntimeMu.Unlock()
	rts, ok := c.hostRuntimeCache[hostID]
	return rts, ok
}

// resolveHostRuntimes returns the host's advertised runtimes, reading the authoritative
// hosts row on a cache miss and warming the cache (TODO #19). It exists for the cold-miss
// case that the warm-at-register path does not cover: after a head restart the new
// instance's hostRuntimeCache is empty and a volunteer reconnects WITHOUT re-registering
// (registration happens only at volunteer start), so without this the per-host runtimes
// would fall back to the account's (flapping) stored set for the rest of the session,
// undermining the flapping-row fix — and a head restart is exactly what deploying this
// change does. The miss read is bounded by the dispatch admission semaphore + a short
// timeout, mirroring resolveIdentity, so a reconnect storm sheds instead of collapsing the
// pool; on shed / not-found / no host repo it returns ok=false and the caller falls back
// to the account runtimes. The steady state (warm cache) never reaches here.
func (c *dispatchCache) resolveHostRuntimes(hostID types.ID) ([]string, bool) {
	if rts, ok := c.peekHostRuntimes(hostID); ok {
		return rts, true
	}
	if c.deps.hostRepo == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return nil, false // admission saturated: fall back to account runtimes this request
	}
	defer release()
	h, err := c.deps.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		if !isNotFound(err) {
			c.logger.Warn("dispatch cache: host runtimes resolve failed", "host_id", hostID, "error", err)
		}
		return nil, false
	}
	c.putHostRuntimes(hostID, h.AvailableRuntimes)
	return c.peekHostRuntimes(hostID)
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

// --- reliability-weighted adaptive in-flight budget (TODO #54) ----------------

// defaultBudgetRefreshInterval is how often runBudgetRefresher recomputes per-host
// adaptive in-flight budgets from the reliability store. Off the hot path; a budget only
// needs to track the slowly-decaying reliability signal within tens of seconds (it grows
// as a host's units validate, which is itself paced by real throughput).
const defaultBudgetRefreshInterval = 30 * time.Second

// effectiveInflightCap returns the per-machine in-flight cap to enforce for hostKey: the
// host's ADAPTIVE budget when the reliability quota is enabled, else the flat configured
// cap (today's behavior, byte-for-byte). A host with no warmed budget (brand new, or before
// the first refresher tick after a restart) gets the cold-start floor — never the full cap
// (a fresh key does not get the full quota) and never zero (the floor keeps an honest new
// host busy while it proves itself). One map read under budgetMu, off the hand-out lock; no
// DB touch. Inert (returns flatCap) when the quota is off or the flat cap is unbounded.
func (c *dispatchCache) effectiveInflightCap(hostKey types.ID, flatCap int) int {
	if !c.cfg.reliabilityQuotaEnabled || flatCap <= 0 {
		return flatCap
	}
	c.budgetMu.Lock()
	b, ok := c.hostBudgetCache[hostKey]
	c.budgetMu.Unlock()
	if ok {
		return b
	}
	// No measured signal yet: cold-start at the floor, bounded by the flat cap (a floor
	// configured above the cap can never exceed it).
	if c.cfg.reliabilityFloor < flatCap {
		return c.cfg.reliabilityFloor
	}
	return flatCap
}

// runBudgetRefresher periodically recomputes the per-host adaptive in-flight budgets (#54)
// from the reliability store and SWAPS them into hostBudgetCache, so the hand-out hot path
// reads a fresh budget with no DB touch. It primes ONCE at start (so an established host
// keeps the budget it EARNED across a head restart — the score is persisted and barely
// decays over a restart, so this avoids re-throttling proven hosts to the floor on deploy)
// then runs on a ticker. A no-op when the reliability quota is disabled or no reliability
// repo is wired. Returns when ctx is done.
func (c *dispatchCache) runBudgetRefresher(ctx context.Context, interval time.Duration) {
	if !c.cfg.reliabilityQuotaEnabled || c.deps.reliabilityRepo == nil {
		return
	}
	if interval <= 0 {
		interval = defaultBudgetRefreshInterval
	}
	c.logger.Info("dispatch cache budget refresher starting",
		"interval", interval, "floor", c.cfg.reliabilityFloor, "cap", c.cfg.maxInflightPerVolunteer)
	c.refreshBudgetsOnce(ctx) // prime so warmed hosts keep their earned budget from the first hand-out
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("dispatch cache budget refresher stopping")
			return
		case <-ticker.C:
			c.refreshBudgetsOnce(ctx)
		}
	}
}

// refreshBudgetsOnce reads the active hosts' decayed reliability scores and rebuilds the
// in-memory per-host budget map. Bounded by the maintenance admission semaphore + a short
// timeout so it sheds under DB pressure (the existing budget map keeps serving, slightly
// stale). The whole map is swapped atomically so the hot path never sees a half-built one.
func (c *dispatchCache) refreshBudgetsOnce(ctx context.Context) {
	dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquireMaintenance(dbCtx)
	if !ok {
		return // admission/ctx pressure: keep the current (stale) budgets, retry next tick
	}
	inputs, err := c.deps.reliabilityRepo.ListBudgetInputs(dbCtx)
	release()
	if err != nil {
		c.logger.Warn("dispatch cache: reliability budget refresh failed", "error", err)
		return
	}
	next := make(map[types.ID]int, len(inputs))
	for _, in := range inputs {
		next[in.HostID] = reliability.Budget(in.Score, c.cfg.reliabilityFloor, c.cfg.maxInflightPerVolunteer, reliability.DefaultRampUnits)
	}
	c.budgetMu.Lock()
	c.hostBudgetCache = next
	c.budgetMu.Unlock()
	c.logger.Debug("dispatch cache: reliability budgets refreshed", "hosts", len(next))
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

	// Refresh the trusted-subject score snapshot on the refill cadence. This is the ONE
	// place the trust store is read for the reservation: a DB touch already happens here
	// under the maintenance slot we hold, so the trust read rides the existing budget and
	// never lands on the hand-out hot path (nor a DB call under mu — the peekLeaf rule). It
	// runs before the dispatchable query so an empty dispatchable universe still keeps the
	// snapshot fresh for already-staged candidates. Stale-tolerant and nil-repo-tolerant.
	c.refreshTrustScores(dbCtx)
	// Same for the account-standing snapshot (BG-24b): one more OFF-hot-path read under the
	// maintenance slot already held, feeding the BENCHED dispatch gate, the countable-
	// coverage / trusted-present standing filters, and the PROBATION in-flight floor. Same
	// staleness / nil-repo tolerance as the trust read.
	c.refreshStanding(dbCtx)

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
		// D-3: nothing came back from the dispatchable query — the operator's signal that
		// the refiller is healthy but the QUEUED/eligible universe is empty (vs. a DB error,
		// which logs separately above).
		c.logger.Debug("refill: nothing dispatchable",
			"want", want, "excluded_count", len(excluded), "leaf_scoped", len(leafIDs) > 0)
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
			// Non-countable portion of the raw seed (account standing, BG-24b), subtracted
			// by eligibleLocked's coverage bound so redundancy is closed only by countable
			// copies — the cache's forced-replication parity with countableCoverageSQL.
			probationCoverage: dc.ProbationCoverage,
			contributors:      strSet(dc.ContributorSubjects),
			benched:           idSet(dc.BenchedVolunteerIDs),
			// Trusted-corroborator reservation inputs (SQL twin: DispatchCandidate). K == 0
			// leaves the reservation inert; the trusted-contributor snapshot is frozen here
			// (see the candidate field docs) so a pending author's stamped trustedness is
			// never re-scored at hand-out.
			effectiveTrustK:     dc.EffectiveTrustK,
			effectiveTrustFloor: dc.EffectiveTrustFloor,
			trustedContributors: strSet(dc.TrustedContributorSubjects),
		})
	}

	c.mu.Lock()
	stagedCount := 0
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
		stagedCount++
	}
	c.mu.Unlock()
	// D-3: confirm a successful restock and how much of the returned batch actually
	// landed (the remainder was already held/staged or hit the pool ceiling).
	c.logger.Debug("refill: staged",
		"returned", len(cands), "staged", stagedCount, "leaf_scoped", len(leafIDs) > 0)
}

// refreshTrustScores re-reads the subject trust-score snapshot from the trust store when
// it has gone stale (older than trustScoreTTL), feeding eligibleLocked's trusted-
// corroborator reservation. It is called ONLY from fetchAndStage, where a DB touch already
// happens under the maintenance admission slot the caller holds, so the read adds no DB
// call to the hot path — and it never reads the DB while holding mu (the peekLeaf rule):
// the staleness check and the store are under mu, the AllScores read is not.
//
// A nil trust repo (tests, no-pool / mux-only constructions) is tolerated — the snapshot
// stays nil, which classifies nobody as trusted; combined with only-K==0 candidates the
// reservation is a no-op. On a read error the previous (stale) snapshot is kept: a stale
// verdict is self-correcting at the SQL landing, so serving stale beats dropping the
// snapshot to nil and needlessly withholding slots.
func (c *dispatchCache) refreshTrustScores(ctx context.Context) {
	if c.deps.trustRepo == nil {
		return
	}
	c.mu.Lock()
	fresh := !c.trustScoresAt.IsZero() && c.now().Sub(c.trustScoresAt) < trustScoreTTL
	c.mu.Unlock()
	if fresh {
		return
	}
	scores, err := c.deps.trustRepo.AllScores(ctx)
	if err != nil {
		c.logger.Warn("dispatch cache: trust score refresh failed; keeping prior snapshot", "error", err)
		return
	}
	c.mu.Lock()
	c.trustScores = scores
	c.trustScoresAt = c.now()
	c.mu.Unlock()
}

// refreshStanding re-reads the non-OK account-standing snapshot from the standing store when
// it has gone stale (older than standingSnapshotTTL), feeding the BENCHED dispatch gate, the
// countable-coverage / trusted-present standing filters, and the PROBATION in-flight floor
// (BG-24b). Like refreshTrustScores it is called ONLY from fetchAndStage, where a DB touch
// already happens under the maintenance admission slot the caller holds, so it adds no DB
// call to the hot path — and it never reads the DB while holding mu (the peekLeaf rule): the
// staleness check and the store are under mu, the AllNonOK read is not.
//
// A nil standing repo (tests, no-pool / mux-only constructions) is tolerated — the snapshot
// stays nil, which classifies EVERY account OK, so every standing gate is inert. On a read
// error the previous (stale) snapshot is kept: a stale verdict is self-correcting at the SQL
// landing, so serving stale beats dropping the snapshot to nil and either wrongly benching
// nobody or forcing every gate open.
func (c *dispatchCache) refreshStanding(ctx context.Context) {
	if c.deps.standingRepo == nil {
		return
	}
	c.mu.Lock()
	fresh := !c.standingSnapshotAt.IsZero() && c.now().Sub(c.standingSnapshotAt) < standingSnapshotTTL
	c.mu.Unlock()
	if fresh {
		return
	}
	entries, err := c.deps.standingRepo.AllNonOK(ctx)
	if err != nil {
		c.logger.Warn("dispatch cache: standing snapshot refresh failed; keeping prior snapshot", "error", err)
		return
	}
	c.mu.Lock()
	c.standingSnapshot = entries
	c.standingSnapshotAt = c.now()
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

// shutdownFlushTimeout bounds the flusher's final best-effort flush. The
// shutdown tail waits on Drained() before closing the pool, so an unreachable
// database must not be able to hold that join (and thus pool.Close) hostage
// beyond the shutdown budget.
const shutdownFlushTimeout = 5 * time.Second

// Drained is closed once the flusher has completed its final best-effort flush
// after context cancellation. The shutdown tail waits on it before pool.Close()
// so the final flush runs against a live pool (BG-32). If the flusher dies
// without reaching its shutdown path the channel never closes — callers must
// bound their wait.
func (c *dispatchCache) Drained() <-chan struct{} {
	return c.flusherDone
}

// runFlusher is the background goroutine that drains pendingWrites to Postgres in
// batched multi-row UPDATEs. It flushes every flushInterval or whenever the queue
// reaches flushBatchSize. Returns when ctx is done (with a final best-effort flush,
// signalled via Drained).
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
			// Bounded (not context.Background) so a dead database cannot stall the
			// shutdown join that waits on Drained().
			flushCtx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
			c.flushOnce(flushCtx)
			c.flushSpotChecksOnce(flushCtx)
			cancel()
			close(c.flusherDone)
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

	// Void any copy that did NOT land (a flush conflict: the unit is no longer QUEUED,
	// redundancy was already met, or this volunteer already holds a live copy). Remove
	// the in-memory hold so the cache does not count a copy it could not persist.
	//
	// Per-copy dispatch: a batch CAN legitimately carry several records for the SAME
	// unit (distinct volunteers — the parallel-copy case), so landed is matched on the
	// exact (work_unit, volunteer) pair, not just the unit id.
	landedPairs := make(map[[2]types.ID]bool, len(landed))
	for _, fc := range landed {
		landedPairs[[2]types.ID{fc.WorkUnitID, fc.VolunteerID}] = true
	}
	for _, rec := range batch {
		if landedPairs[[2]types.ID{rec.WorkUnitID, rec.VolunteerID}] {
			continue
		}
		c.voidNonLandedCopy(rec.WorkUnitID, rec.VolunteerID)
		// D-5: a non-landed copy silently revokes a hand-out (the unit is no longer
		// QUEUED, redundancy was met, this volunteer already holds a live copy, or it is
		// in post-failure cooldown). voidNonLandedCopy also benches the volunteer on the
		// staged candidate so the same un-reservable unit is not re-offered to it next tick.
		c.logger.Debug("voided non-landed copy (flush conflict); benched volunteer on candidate",
			"work_unit_id", rec.WorkUnitID, "volunteer_id", rec.VolunteerID)
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
// ReserveCopy (land the spot-check copy row). The unit stays QUEUED so a second
// corroborating volunteer can still be dispatched it. Unlike the NORMAL flush, a
// spot-check that fails to land (the unit is no longer QUEUED) voids the in-memory hold.
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
		// FIX 4: the spot-check landing (MarkSpotCheck + ReserveCopy + history
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
		// Land the spot-check copy as a RESERVED copy row (per-copy model). A
		// spot-check unit's effective deadline still governs its buffered hold.
		deadline := int(time.Until(sc.reservedUntil).Seconds())
		if deadline < 0 {
			deadline = 0
		}
		if _, err := c.deps.wuRepo.ReserveCopy(dbCtx, sc.workUnitID, sc.volunteerID, sc.hostID, sc.reservedUntil, deadline); err != nil {
			release()
			cancel()
			c.releaseInMem(sc.workUnitID, sc.volunteerID)
			c.logger.Warn("dispatch cache: spot-check copy reserve failed; voided",
				"work_unit_id", sc.workUnitID, "error", err)
			continue
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

// pendingWriteCount returns the queued reservation-write count (for tests and
// the lettuce_dispatch_pending_reservation_writes gauge).
func (c *dispatchCache) pendingWriteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pendingWrites)
}

// pendingSpotCheckCount returns the queued spot-check-write count (for the
// lettuce_dispatch_pending_spot_check_writes gauge).
func (c *dispatchCache) pendingSpotCheckCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pendingSpotChecks)
}

// --- reconciler --------------------------------------------------------------

// runReconciler periodically rebuilds the per-volunteer inflight counters from the
// authoritative DB counts so crash/drift cannot cause permanent over-admission.
func (c *dispatchCache) runReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	c.logger.Info("dispatch cache reconciler starting", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("dispatch cache reconciler stopping")
			return
		case <-ticker.C:
			c.reconcileOnce(ctx)
		}
	}
}

// NoteVolunteerHeld records the set of work units a MACHINE reports holding in its client
// buffer on a RequestWorkUnit (every buffered and running unit it currently has), keyed by
// the requesting host's effective id (TODO #19) so two machines under one key never evict
// each other's buffers. volunteerID (the account) is kept so the reconcile can drop the
// released unit from the account-keyed in-memory ledger. Cheap and purely in-memory — it
// does NOT touch Postgres, so it stays off the request hot path; the DB reconciliation
// happens on the reconciler tick.
func (c *dispatchCache) NoteVolunteerHeld(volunteerID, hostID types.ID, held []types.ID) {
	set := make(map[types.ID]struct{}, len(held))
	for _, id := range held {
		set[id] = struct{}{}
	}
	c.heldMu.Lock()
	c.heldReports[hostID] = heldReport{units: set, account: volunteerID, at: c.now()}
	c.heldMu.Unlock()
}

// reconcileBuffers releases each volunteer's buffered (RESERVED, not-yet-run-started)
// reservations that it no longer holds, per the held set it last reported. This is the
// durable correction for a client whose buffer and the head's reservations have
// diverged — a client that dropped its buffer across a restart, or a head restart that
// left buffered copies in the DB the volunteer no longer tracks: the stale copy is
// closed, its work unit redispatches at once, and it stops counting against the
// volunteer's inflight cap. Only reports fresh enough to trust are acted on (a
// volunteer that stopped polling has its copies reclaimed by the deadline instead), and
// a copy is released only if it was created before BOTH the volunteer's last report and
// the grace window — so the batch that filled a now-quiet client's buffer (created after
// its last report) and a just-handed copy are never wrongly reaped. Running copies are untouched.
func (c *dispatchCache) reconcileBuffers(ctx context.Context) {
	now := c.now()

	// Snapshot fresh reports and prune stale ones (a returning machine re-reports). Keyed
	// per host; account carried for the in-memory release.
	type pending struct {
		host     types.ID
		account  types.ID
		held     []types.ID
		reported time.Time
	}
	var todo []pending
	c.heldMu.Lock()
	for host, r := range c.heldReports {
		if now.Sub(r.at) > heldReportFreshness {
			delete(c.heldReports, host)
			continue
		}
		held := make([]types.ID, 0, len(r.units))
		for u := range r.units {
			held = append(held, u)
		}
		todo = append(todo, pending{host: host, account: r.account, held: held, reported: r.at})
	}
	c.heldMu.Unlock()
	if len(todo) == 0 {
		return
	}

	graceCutoff := now.Add(-reconcileGracePeriod)
	releasedAny := false
	for _, p := range todo {
		// Reap only copies created BEFORE the volunteer's last held report: a copy newer
		// than the report could not have been in it (the volunteer had not received it when
		// it built the request), so reaping it would drop work the volunteer holds but has
		// not yet had the chance to report — e.g. the batch that filled its buffer, after
		// which a full client stops requesting and goes quiet. The grace window further
		// guards a just-handed copy. So reap iff created < min(report time, now - grace).
		cutoff := graceCutoff
		if p.reported.Before(cutoff) {
			cutoff = p.reported
		}
		relCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
		release, ok := c.acquireMaintenance(relCtx)
		if !ok {
			cancel()
			continue // admission/ctx pressure: retry on the next tick
		}
		// Release by HOST (TODO #19): only THIS machine's buffered copies it no longer
		// holds, so host A's report never reaps host B's buffer.
		released, err := c.deps.wuRepo.ReleaseStaleBufferedCopies(relCtx, p.host, p.held, cutoff)
		release()
		cancel()
		if err != nil {
			c.logger.Warn("dispatch cache: buffer reconcile failed", "host_id", p.host, "error", err)
			continue
		}
		if len(released) == 0 {
			continue
		}
		releasedAny = true
		// Drop the released units from this replica's in-memory ledger so they stop
		// counting as held and can be re-staged. The in-memory holders key on the ACCOUNT,
		// so release by account (the host's owner); releaseInMemLocked then decrements the
		// host's inflight via the holder's stored host id. A no-op for copies this replica
		// never held in memory (e.g. recovered from the DB after a head restart).
		c.mu.Lock()
		for _, uid := range released {
			c.releaseInMemLocked(uid, p.account)
		}
		c.mu.Unlock()
		c.logger.Info("dispatch cache: released stale buffered reservations",
			"host_id", p.host, "volunteer_id", p.account, "released", len(released))
	}
	if releasedAny {
		c.signalRefill()
	}
}

// reconcileOnce reconciles the in-memory inflight counters with the authoritative DB
// per-MACHINE count (TODO #19). The DB count (active history rows + live reservations,
// keyed by COALESCE(host_id, volunteer_id)) is authoritative; the in-memory deltas for
// not-yet-flushed reservations are layered on top so a freshly handed-out (still-
// unflushed) reservation is not under-counted. Both are keyed on the effective host id,
// which equals the account id for a copy with no host — so the keys agree everywhere.
func (c *dispatchCache) reconcileOnce(ctx context.Context) {
	// First release any buffered reservations machines no longer hold, so the freed
	// copies are reflected in the authoritative inflight counts recomputed below.
	c.reconcileBuffers(ctx)

	dbCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(dbCtx)
	if !ok {
		return
	}
	defer release()
	dbCounts, err := c.deps.wuRepo.CountActiveByHost(dbCtx)
	if err != nil {
		c.logger.Warn("dispatch cache: inflight reconcile failed", "error", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Count not-yet-flushed in-memory reservations per MACHINE (these may not yet be
	// reflected in dbCounts). The pending write carries a nullable host id; meterID folds
	// a no-host write onto the account id, matching CountActiveByHost's COALESCE.
	pending := make(map[types.ID]int)
	for _, rec := range c.pendingWrites {
		pending[meterID(rec.VolunteerID, rec.HostID)]++
	}
	for _, rec := range c.pendingSpotChecks {
		pending[meterID(rec.volunteerID, rec.hostID)]++
	}
	next := make(map[types.ID]int)
	for host, n := range dbCounts {
		next[host] = n
	}
	for host, n := range pending {
		next[host] += n
	}
	// D-4: report the drift the reconcile is about to correct, but only when the
	// authoritative recount actually differs from the in-memory counters (a steady-state
	// tick stays silent). Guarded by Enabled so the comparison loops run only when Debug
	// is on.
	if c.logger.Enabled(context.Background(), slog.LevelDebug) {
		changed := 0
		oldTotal := 0
		newTotal := 0
		for vol, n := range c.inflight {
			oldTotal += n
			if next[vol] != n {
				changed++
			}
		}
		for vol, n := range next {
			newTotal += n
			if _, ok := c.inflight[vol]; !ok && n != 0 {
				changed++
			}
		}
		if changed > 0 {
			c.logger.Debug("dispatch cache: inflight reconcile corrected drift",
				"volunteers_changed", changed, "old_total", oldTotal, "new_total", newTotal)
		}
	}
	c.inflight = next

	// Prune per-volunteer send-clock entries older than the min-send interval: such an
	// entry can never throttle again, so dropping it keeps lastHandOut bounded by the
	// volunteers seen within one interval rather than the lifetime set.
	if c.cfg.minSendInterval > 0 {
		cutoff := c.now().Add(-c.cfg.minSendInterval)
		for vol, at := range c.lastHandOut {
			if at.Before(cutoff) {
				delete(c.lastHandOut, vol)
			}
		}
	}
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
	// GPU presence: a leaf needs a GPU if EITHER flag is set.
	// execution_config.gpu_required is the natural place a leaf author declares it;
	// resource_requirements.gpu_required is the parallel matching field. The two were
	// historically unsynced, so a leaf that set only the execution_config flag (with
	// gpu_type left at the default ANY) slipped past the presence gate and reached
	// GPU-less volunteers, which then failed at runtime (#30). Gate presence + VRAM on
	// either flag; min_gpu_vram_mb lives in resource_requirements.
	if rr.GPURequired || ec.GPURequired {
		if !opts.HasGPU || rr.MinGPUVRAMMB > opts.MaxGPUVRAMMB {
			return false
		}
	}
	// GPU compute capability (resource_requirements.gpu_compute_capability), when required.
	if rr.GPURequired && rr.GPUComputeCapability != nil && *rr.GPUComputeCapability != "" {
		if !containsString(opts.GPUComputeCapabilities, *rr.GPUComputeCapability) {
			return false
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
	// GPU vendor/type (execution_config.gpu_type): if the exec config requires a GPU and
	// pins a specific vendor/type, the volunteer must have it.
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

// idSet builds a set from a slice of ids (nil for an empty/absent slice, so an
// unstaged candidate carries a nil — not empty — map, which membership checks read
// as "no members" at no allocation cost).
func idSet(ids []types.ID) map[types.ID]struct{} {
	if len(ids) == 0 {
		return nil
	}
	s := make(map[types.ID]struct{}, len(ids))
	for _, id := range ids {
		s[id] = struct{}{}
	}
	return s
}

// strSet builds a set from a slice of strings (nil for an empty/absent slice, so an
// unstaged candidate carries a nil — not empty — map, read as "no members" at no
// allocation cost). The string twin of idSet, for the subject-keyed contributor set.
func strSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	s := make(map[string]struct{}, len(ss))
	for _, v := range ss {
		s[v] = struct{}{}
	}
	return s
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
