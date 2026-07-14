package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/safego"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type volunteerService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	pool          *pgxpool.Pool
	version       string
	startTime     time.Time
	volunteerRepo volunteer.Repository
	// hostRepo upserts per-MACHINE host rows (TODO #19). Constructed from the pool, so it
	// is nil only in gRPC-plumbing unit tests that pass a nil pool (which never send a
	// host_id); RegisterVolunteer guards on it.
	hostRepo volunteer.HostRepository
	// reliabilityRepo backs the per-host adaptive work quota (TODO #54): the budget
	// refresher reads measured-reliability scores from it off the hot path. Constructed from
	// the pool, so it is nil only in gRPC-plumbing unit tests with a nil pool (the budget
	// refresher is then a no-op and the flat in-flight cap applies).
	reliabilityRepo     reliability.Repository
	wuRepo              workunit.WorkUnitRepository
	leafRepo            leaf.Repository
	artifactVersionRepo leaf.ArtifactVersionRepository
	assignRepo          assignment.Repository
	resultRepo          result.Repository
	batchRepo           workunit.BatchRepository
	checkpointRepo      checkpoint.Repository
	validationEngine    *validation.Engine
	// trustRepo snapshots the submitting subject's account-level trust score onto each result
	// at submit time (see internal/trust). Constructed from the pool, so it is nil only in the
	// gRPC-plumbing unit tests that pass a nil pool; SubmitResult is nil-safe (stamps score 0).
	trustRepo trust.Repository
	// standingRepo feeds the dispatch cache's account-standing snapshot (BG-24b): the BENCHED
	// gate, the countable-coverage / trusted-present standing filters, and the PROBATION
	// in-flight floor. Same nil-only-in-plumbing-tests treatment as trustRepo; the cache is
	// nil-safe (no snapshot = every account OK = gates inert).
	standingRepo standingSnapshotReader
	// trustPolicy is the head trust-gate configuration overlaid onto each leaf by the
	// transitioner. Zero value = gate off (the deploy-safety default); the real policy is
	// threaded from config via NewVolunteerService.
	trustPolicy transition.TrustPolicy
	// registrationCap is the registration admission cap policy (design §4.1): at most
	// PerDay volunteer CREATIONS per (IP bucket, UTC day). Zero value = gate off (the
	// deploy-safety default); main.go threads the real policy via SetAdmissionPolicy.
	// Re-registration of an existing key never consults it.
	registrationCap admission.CapPolicy
	// registrationPow is the registration proof-of-work policy (design §4.1): when
	// enforcement is on, a registration that would CREATE a new volunteer must redeem
	// a valid server-issued challenge. Zero value = enforcement off (the deploy-safety
	// default); main.go threads the real policy via SetRegistrationPowPolicy.
	// Challenge ISSUANCE (GetRegistrationChallenge) works regardless so clients can be
	// written probe-free. Re-registration of an existing key never consults it.
	registrationPow admission.PowPolicy
	// hostCap is the per-account host cap policy (BG-25): how many server-issued
	// per-machine host ids one account may hold, and the staleness window that makes
	// an idle slot evictable at mint time. Zero value = cap off (mints always succeed
	// — the unit-test default); main.go threads the real policy via SetHostCapPolicy.
	hostCap HostCapPolicy
	// contentFetchEnabled is the head-wide external-output content-verification knob
	// (LETTUCE_HEAD_CONTENT_FETCH_ENABLED, design §10.9). Zero value = off (the
	// deploy-safety default): with it off SubmitResult refuses every external
	// output_data_url at the front door — even for an opted-in leaf — so no
	// volunteer-claimed checksum can reach validation. main.go threads the real value
	// via SetContentFetchPolicy.
	contentFetchEnabled bool
	// now supplies the current time for trust power-suppression checks; overridable in tests.
	now func() time.Time
	// transitioner is the SINGLE owner of work-unit redundancy decisions (TODO #50):
	// SubmitResult delegates the validate/reject/wait/dead-letter decision to it. Built in
	// the constructor from the validation engine + repos; nil only when validationEngine is
	// nil (gRPC-plumbing unit tests), where SubmitResult simply skips the evaluation.
	transitioner            *transition.Transitioner
	logger                  *slog.Logger
	headName                string
	headDescription         string
	headURL                 string
	defaultWeights          map[string]int32
	maxInflightPerVolunteer int

	// Layer 1: work batching + buffer lease.
	maxBatchPerRequest int
	leaseSeconds       int

	// Layer 1: load measurement → server-directed retry delay.
	loadEstimator *loadEstimator

	// Layer 2: in-process dispatch cache (single-replica). When non-nil,
	// RequestWorkUnit serves reservations from the cache (zero DB on the hot path)
	// and sheds with ResourceExhausted under overload. nil keeps the Layer-1
	// per-request transaction path (used by the gRPC-plumbing unit tests that never
	// start the cache goroutines).
	dispatchCache *dispatchCache
	// dispatchCfg holds the Layer-2 dispatch-cache knobs captured from SetHeadConfig
	// so StartDispatchCache can build the cache later.
	dispatchCfg HeadDispatchConfig

	// Load-shed log sampling counters (H-5). Each shed site logs the first shed and
	// every shedLogSampleN-th thereafter (first-then-every-N), so a sustained overload
	// stays visible without one Warn per shed on the hot path. Atomic, zero-valued.
	cacheShedLogN    uint64
	identityShedLogN uint64
	// capRefusalLogN samples the creation-cap refusal Warn the same way (a capped
	// network retrying is an abuse signal, not a per-line event).
	capRefusalLogN uint64
	// hostCapRefusalLogN samples the host-mint refusal Warn (BG-25) the same way.
	hostCapRefusalLogN uint64
}

// referenceBenchmarkFPOPS is the head's fixed reference-host throughput (FP ops /
// second) used to derive a per-leaf duration estimate from rsc_fpops_est for the
// GetHeadInfo LeafInfo. It is a coarse seed only: the volunteer refines the estimate
// with its OWN measured benchmark + DCF once it has buffered units. ~50 GFLOP/s is a
// conservative single-core throughput, deliberately modest so a short-unit leaf is
// never under-estimated to zero (the #29 zero-trap).
const referenceBenchmarkFPOPS = 5.0e10

// estimatedDurationSecondsForLeaf derives a per-leaf duration estimate (seconds)
// from the leaf's rsc_fpops_est against the reference benchmark. Returns 0 when no
// estimate is available (rsc_fpops_est <= 0), which the volunteer treats as "use the
// flat cap" — the same behavior as before #29.
func estimatedDurationSecondsForLeaf(rscFpopsEst float64) float64 {
	if rscFpopsEst <= 0 {
		return 0
	}
	return rscFpopsEst / referenceBenchmarkFPOPS
}

// asArtifactVersionRepo returns leafRepo as an ArtifactVersionRepository when the
// concrete repo implements it (the *leaf.PgxRepository does), else nil — so a test
// mock leafRepo simply disables versioning/pinning.
func asArtifactVersionRepo(r leaf.Repository) leaf.ArtifactVersionRepository {
	av, _ := r.(leaf.ArtifactVersionRepository)
	return av
}

// NewVolunteerService creates a new VolunteerService gRPC implementation.
func NewVolunteerService(
	pool *pgxpool.Pool,
	version string,
	startTime time.Time,
	volunteerRepo volunteer.Repository,
	wuRepo workunit.WorkUnitRepository,
	leafRepo leaf.Repository,
	assignRepo assignment.Repository,
	resultRepo result.Repository,
	batchRepo workunit.BatchRepository,
	checkpointRepo checkpoint.Repository,
	validationEngine *validation.Engine,
	logger *slog.Logger,
	trustPolicy transition.TrustPolicy,
) lettucev1.VolunteerServiceServer {
	s := &volunteerService{
		pool:                pool,
		version:             version,
		startTime:           startTime,
		volunteerRepo:       volunteerRepo,
		wuRepo:              wuRepo,
		leafRepo:            leafRepo,
		artifactVersionRepo: asArtifactVersionRepo(leafRepo),
		assignRepo:          assignRepo,
		resultRepo:          resultRepo,
		batchRepo:           batchRepo,
		checkpointRepo:      checkpointRepo,
		validationEngine:    validationEngine,
		trustPolicy:         trustPolicy,
		now:                 time.Now,
		logger:              logger,
		maxBatchPerRequest:  defaultMaxBatchPerRequest,
		leaseSeconds:        defaultLeaseSeconds,
	}
	// Per-machine host upserts (TODO #19) need the pool; nil only in the gRPC-plumbing
	// unit tests that pass a nil pool and never send a host_id.
	if pool != nil {
		s.hostRepo = volunteer.NewPgxHostRepository(pool)
		// Per-host reliability store for the adaptive quota (TODO #54); same nil-only-in-
		// plumbing-tests treatment as hostRepo.
		s.reliabilityRepo = reliability.NewPgxRepository(pool)
		// Account-level trust store for submit-time score snapshots (see internal/trust); same
		// nil-only-in-plumbing-tests treatment. Stamping is nil-safe regardless.
		s.trustRepo = trust.NewPgxRepository(pool)
		// Account-standing store for the dispatch cache's standing snapshot (BG-24b); same
		// treatment. The cache is nil-safe (everyone OK, gates inert).
		s.standingRepo = standing.NewPgxRepository(pool)
	}
	// Default load estimator until SetHeadConfig overrides the tunables. The pool
	// saturation closure is nil-safe (returns 0 if the pool is nil, as in some
	// gRPC-plumbing unit tests).
	s.loadEstimator = newLoadEstimator(defaultLoadEstimatorConfig(), poolSaturation(pool))

	// Wire the single transitioner (TODO #50). It owns the redundancy decision; the
	// validation engine is its comparator + accept/reject implementation. nil when no engine
	// is present (the gRPC-plumbing tests pass nil and skip evaluation).
	s.transitioner = newTransitioner(pool, wuRepo, leafRepo, resultRepo, validationEngine, trustPolicy, logger)
	return s
}

// newTransitioner wires the single redundancy transitioner (TODO #50) over the given repos +
// comparator. The transitioner is the SOLE owner of work-unit state transitions, so EVERY live
// submit path builds one and routes its redundancy decision through it: the gRPC volunteer
// service (above) and the browser/WASM REST submit path (handleBrowserSubmitResult) — TODO #66.
// Returns nil when validationEngine is nil (the gRPC-plumbing unit tests that skip evaluation).
// The per-unit lock is the cross-replica Postgres advisory lock when a pool is available, else a
// no-op; the optimistic state guards + unique constraints remain the correctness backstop either
// way, so two transitioner instances over the same pool still serialize per unit via the DB lock.
func newTransitioner(pool *pgxpool.Pool, wuRepo workunit.WorkUnitRepository, leafRepo leaf.Repository, resultRepo result.Repository, validationEngine *validation.Engine, trustPolicy transition.TrustPolicy, logger *slog.Logger) *transition.Transitioner {
	if validationEngine == nil {
		return nil
	}
	// Finalization-atomicity boot assertion (★BG-21e): a pool-backed service is the
	// production shape, and a production engine running the mock-friendly passthrough
	// (no FinalizationTxRunner) finalizes NON-atomically — marks, the VALIDATED flip, and
	// the credit rows as separate autocommits. That exact gap shipped once because every
	// test hand-wired the runner while main.go did not, so this is enforced where pool and
	// engine meet: fail construction (and therefore head boot) instead of running open.
	// Mock-based tests keep the passthrough by passing a nil pool.
	if pool != nil && !validationEngine.HasTxRunner() {
		panic("validation engine has no FinalizationTxRunner: a pool-backed head must wire " +
			".WithTxRunner(validation.NewPgxFinalizationTxRunner(pool)) or finalization is not atomic (★BG-21e)")
	}
	var locker transition.Locker = transition.NoopLocker{}
	if pool != nil {
		locker = transition.NewPgxLocker(pool, logger)
	}
	return transition.NewTransitioner(locker, wuRepo, leafRepo, resultRepo, validationEngine, trustPolicy, logger)
}

// HeadDispatchConfig carries the Layer-1 dispatch tunables (work batching,
// buffer lease, server-directed retry delay) into the gRPC volunteer service.
// It is a plain struct (no config-package dependency) so SetHeadConfig stays
// decoupled from internal/config; main.go fills it from HeadConfig.Effective*.
type HeadDispatchConfig struct {
	MaxBatchPerRequest int
	LeaseSeconds       int
	// MinSendIntervalSeconds is the per-volunteer minimum interval (seconds) between
	// successful work hand-outs — the server-side enforced floor on work-acquisition
	// cadence, keyed on the verified Ed25519 identity. 0 disables it (only the advisory
	// retry delay + rate limits + inflight cap apply).
	MinSendIntervalSeconds  int
	MinRetryDelaySeconds    int
	MaxRetryDelaySeconds    int
	RetryDelayJitterPct     float64
	TargetRequestRatePerSec float64

	// --- Layer 2: dispatch-cache knobs ---
	ReadyPoolSize           int
	RefillBatchSize         int
	DispatchAdmissionCap    int // 0 = derive max(1, MaxConns/2) from the pool
	MaintenanceAdmissionCap int // 0 = derive max(1, admissionCap/4)
	FlushIntervalMs         int
	FlushBatchSize          int

	// --- Layer 3: horizontal scale-out (claim-on-refill) ---
	// HeadInstanceID is this replica's stable instance id, stamped as the dispatch-
	// claim owner. The zero value (types.ID{}) keeps single-replica behavior (claim-
	// free refill/flush). main.go fills it from HeadConfig.EffectiveInstanceID().
	HeadInstanceID types.ID
	// ClaimLeaseSeconds is the per-head dispatch-claim lease (seconds). Default 120.
	ClaimLeaseSeconds int

	// --- TODO #54: reliability-weighted adaptive in-flight quota ---
	// ReliabilityQuotaEnabled turns a host's in-flight cap into a function of its measured
	// reliability instead of the flat MaxInflightPerVolunteer. Disabled -> today's flat cap
	// for everyone. main.go fills it from HeadConfig.EffectiveReliabilityQuotaEnabled().
	ReliabilityQuotaEnabled bool
	// ReliabilityQuotaFloor is the cold-start / fully-throttled in-flight buffer a host with
	// no measured signal gets. main.go fills it from HeadConfig.EffectiveReliabilityQuotaFloor().
	ReliabilityQuotaFloor int
}

// SetHeadConfig sets the head identity for GetHeadInfo gRPC responses and the
// Layer-1 dispatch tunables (batching/lease/retry-delay). dispatch may be the
// zero value, in which case per-field defaults apply.
func SetHeadConfig(svc lettucev1.VolunteerServiceServer, name, description, url string, weights map[string]int32, maxInflight int, dispatch HeadDispatchConfig) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.headName = name
		vs.headDescription = description
		vs.headURL = url
		vs.defaultWeights = weights
		vs.maxInflightPerVolunteer = maxInflight

		vs.maxBatchPerRequest = defaultInt(dispatch.MaxBatchPerRequest, defaultMaxBatchPerRequest)
		vs.leaseSeconds = defaultInt(dispatch.LeaseSeconds, defaultLeaseSeconds)
		vs.dispatchCfg = dispatch

		cfg := loadEstimatorConfig{
			targetRequestRatePerSec: defaultFloat(dispatch.TargetRequestRatePerSec, defaultTargetRequestRatePerSec),
			targetAssignLatency:     defaultTargetAssignLatency,
			minDelaySeconds:         defaultInt(dispatch.MinRetryDelaySeconds, defaultMinRetryDelaySeconds),
			maxDelaySeconds:         defaultInt(dispatch.MaxRetryDelaySeconds, defaultMaxRetryDelaySeconds),
			jitterPct:               defaultFloat(dispatch.RetryDelayJitterPct, defaultRetryDelayJitterPct),
		}
		vs.loadEstimator = newLoadEstimator(cfg, poolSaturation(vs.pool))
	}
}

// SetAdmissionPolicy sets the registration admission policy (the per-IP creation cap,
// design §4.1) on the gRPC volunteer service. The zero value is the deploy-safety
// default (gate off, registration unchanged); main.go fills the real policy from
// HeadConfig.Effective* values — a plain struct, so the service stays decoupled from
// internal/config (the SetHeadConfig pattern).
func SetAdmissionPolicy(svc lettucev1.VolunteerServiceServer, cap admission.CapPolicy) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.registrationCap = cap
	}
}

// SetRegistrationPowPolicy sets the registration proof-of-work policy (design §4.1)
// on the gRPC volunteer service. The zero value is the deploy-safety default
// (enforcement off); main.go fills the real policy — effective difficulty/TTL always
// populated so challenge ISSUANCE works even while enforcement is off — from
// HeadConfig.Effective* values (the SetHeadConfig decoupling pattern).
func SetRegistrationPowPolicy(svc lettucev1.VolunteerServiceServer, pow admission.PowPolicy) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.registrationPow = pow
	}
}

// SetHostCapPolicy sets the per-account host cap policy (BG-25) on the gRPC volunteer
// service. The zero value disables the cap (mints always succeed — the unit-test
// default); main.go fills the real policy from HeadConfig.Effective* values (the
// SetHeadConfig decoupling pattern). Host-id ISSUANCE itself is unconditional — the
// cap only bounds how many ids one account may hold.
func SetHostCapPolicy(svc lettucev1.VolunteerServiceServer, p HostCapPolicy) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.hostCap = p
	}
}

// SetContentFetchPolicy sets the head-wide external-output content-verification knob
// (LETTUCE_HEAD_CONTENT_FETCH_ENABLED, design §10.9) on the gRPC volunteer service.
// The zero value is the deploy-safety default (off — external references are refused
// at submit for every leaf); main.go threads the real value from HeadConfig via this
// setter (the SetHostCapPolicy decoupling pattern).
func SetContentFetchPolicy(svc lettucev1.VolunteerServiceServer, enabled bool) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.contentFetchEnabled = enabled
	}
}

// GetRegistrationChallenge issues a registration proof-of-work challenge bound to the
// CALLER's key (from the verified per-request signature, like RegisterVolunteer — the
// key need not be registered yet). Issuance works whether or not enforcement is on, so
// clients can be written probe-free; the table is bounded by the per-IP/per-key rate
// limits, the TTL, and the challenge sweeper.
func (s *volunteerService) GetRegistrationChallenge(ctx context.Context, _ *lettucev1.GetRegistrationChallengeRequest) (*lettucev1.GetRegistrationChallengeResponse, error) {
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	// Guards the gRPC-plumbing unit-test wiring (nil pool / zero policy); production
	// main.go always threads a pool and an effective (>= 8 bits) policy.
	if s.pool == nil || s.registrationPow.DifficultyBits <= 0 {
		return nil, status.Errorf(codes.Unavailable, "registration challenges are not configured on this head")
	}
	c, err := admission.IssueChallenge(ctx, s.pool, authedKey, s.registrationPow.DifficultyBits, s.registrationPow.ChallengeTTL)
	if err != nil {
		s.logger.Error("failed to issue registration challenge", "method", "GetRegistrationChallenge", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	return &lettucev1.GetRegistrationChallengeResponse{
		ChallengeId:    c.ID.String(),
		Challenge:      c.Challenge,
		DifficultyBits: uint32(c.DifficultyBits),
		ExpiresAtUnix:  c.ExpiresAt.Unix(),
	}, nil
}

// registrationGate resolves the per-request admission gate for a volunteer CREATE.
// nil while the creation cap is disabled — the legacy create path, byte-for-byte.
// Fails closed (non-nil error) when the cap is enabled but the client IP is missing
// or unbucketable: a misconfigured head must not silently run uncapped.
func (s *volunteerService) registrationGate(ctx context.Context) (*admission.CreateGate, error) {
	if !s.registrationCap.Enabled {
		return nil, nil
	}
	ip, ok := GRPCClientIPFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("registration cap enabled but no client IP in request context")
	}
	bucket, err := admission.BucketForIP(ip)
	if err != nil {
		return nil, err
	}
	return &admission.CreateGate{Bucket: bucket, CapPerDay: s.registrationCap.PerDay}, nil
}

// isConflictErr reports whether err is the repository's duplicate-key Conflict (the
// get-then-create race on a new public key).
func isConflictErr(err error) bool {
	var apiErr *apierror.APIError
	return errors.As(err, &apiErr) && apiErr.HTTPStatus == 409
}

// StartDispatchCache builds and launches the Layer-2 in-process dispatch cache on
// the given service (a no-op if svc is not the concrete volunteerService). main.go
// calls this once per process at startup AFTER SetHeadConfig has captured the
// dispatch knobs. Layer 3: EVERY replica runs its own cache safely — the refill
// claims units on this head's instance id (claim-on-refill), so two replicas never
// double-hand the same QUEUED unit. With no instance id configured the refill is
// claim-free, which is correct for a single replica.
func StartDispatchCache(svc lettucev1.VolunteerServiceServer, ctx context.Context) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.StartDispatchCache(ctx)
	}
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func defaultFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// StartDispatchCache builds the Layer-2 in-process dispatch cache from the
// captured dispatch config and the service's repos/pool, then launches its
// refiller, flusher, and reconciler goroutines (returning when ctx is cancelled).
// After this call RequestWorkUnit serves reservations from the cache (zero DB on
// the hot path) and sheds with ResourceExhausted under overload.
//
// Layer 3: each replica runs its OWN cache safely under N replicas. The refill
// stamps a per-head dispatch claim (this head's instance id) on the units it pulls,
// so two caches never double-hand the same QUEUED unit and a crashed replica's
// claims expire after the claim lease and become re-claimable. main.go calls it
// once per process at startup. With no instance id configured the refill is
// claim-free — the correct single-replica behaviour.
func (s *volunteerService) StartDispatchCache(ctx context.Context) {
	admissionCap := s.dispatchCfg.DispatchAdmissionCap
	if admissionCap <= 0 {
		// Derive max(1, MaxConns/2) from the pool so the cache's DB ops cannot
		// saturate it.
		maxConns := 0
		if s.pool != nil {
			maxConns = int(s.pool.Stat().MaxConns())
		}
		admissionCap = maxConns / 2
		if admissionCap < 1 {
			admissionCap = 1
		}
	}
	flushInterval := time.Duration(defaultInt(s.dispatchCfg.FlushIntervalMs, 100)) * time.Millisecond

	// FIX 4: derive the reserved maintenance budget (refiller + ticker
	// reservation-flush + spot-check landing) as max(1, admissionCap/4) when not set,
	// so client writers cannot starve cache restock.
	maintCap := s.dispatchCfg.MaintenanceAdmissionCap
	if maintCap <= 0 {
		maintCap = admissionCap / 4
		if maintCap < 1 {
			maintCap = 1
		}
	}

	// Layer 3: derive the dispatch-claim lease (default 120s) when scale-out is on
	// (a head instance id was configured). A zero head id keeps the claim-free
	// single-replica refill/flush paths.
	claimLeaseSeconds := s.dispatchCfg.ClaimLeaseSeconds
	if claimLeaseSeconds <= 0 {
		claimLeaseSeconds = defaultClaimLeaseSeconds
	}
	cfg := dispatchCacheConfig{
		readyPoolSize:           s.dispatchCfg.ReadyPoolSize,
		refillBatchSize:         s.dispatchCfg.RefillBatchSize,
		admissionCap:            admissionCap,
		maintenanceAdmissionCap: maintCap,
		flushInterval:           flushInterval,
		flushBatchSize:          s.dispatchCfg.FlushBatchSize,
		leaseSeconds:            s.leaseSeconds,
		maxInflightPerVolunteer: s.maxInflightPerVolunteer,
		minSendInterval:         time.Duration(s.dispatchCfg.MinSendIntervalSeconds) * time.Second,
		headID:                  s.dispatchCfg.HeadInstanceID,
		claimLease:              time.Duration(claimLeaseSeconds) * time.Second,
		reliabilityQuotaEnabled: s.dispatchCfg.ReliabilityQuotaEnabled,
		reliabilityFloor:        s.dispatchCfg.ReliabilityQuotaFloor,
	}
	// artifactVersionRepo (the same *leaf.PgxRepository) lets the cache resolve the
	// current/pinned artifact version per assignment and pin units for homogeneous
	// redundancy (TODO #38). Nil on a non-Pgx leafRepo (tests) -> versioning/pinning
	// disabled, legacy behavior preserved.
	cache := newDispatchCache(cfg, dispatchDeps{
		wuRepo:              s.wuRepo,
		leafRepo:            s.leafRepo,
		assignRepo:          s.assignRepo,
		volunteerRepo:       s.volunteerRepo,
		hostRepo:            s.hostRepo,
		artifactVersionRepo: s.artifactVersionRepo,
		reliabilityRepo:     s.reliabilityRepo,
		// Reuse the already-constructed submit-time trust store (nil only in the
		// gRPC-plumbing tests that pass a nil pool). The refiller reads AllScores off the
		// hot path to keep the trusted-corroborator reservation's score snapshot fresh.
		trustRepo: s.trustRepo,
		// Account-standing store (BG-24b): the refiller reads AllNonOK off the hot path to
		// keep the standing snapshot fresh for the BENCHED gate, the countable-coverage /
		// trusted-present standing filters, and the PROBATION in-flight floor. Nil only in
		// the same plumbing tests; the cache treats nil as everyone-OK.
		standingRepo: s.standingRepo,
	}, s.logger)
	s.dispatchCache = cache

	// Launched via safego (BG-19): a panic in any cache loop must not kill the
	// head; the loop is restarted (with backoff) and resumes from its next tick.
	safego.Go(ctx, s.logger, "dispatch-cache-refiller", func(ctx context.Context) {
		cache.runRefiller(ctx, defaultRefillTickInterval)
	})
	safego.Go(ctx, s.logger, "dispatch-cache-flusher", cache.runFlusher)
	safego.Go(ctx, s.logger, "dispatch-cache-reconciler", func(ctx context.Context) {
		cache.runReconciler(ctx, defaultReconcileInterval)
	})
	// TODO #54: the adaptive in-flight budget refresher (a no-op when the quota is disabled
	// or no reliability repo is wired). Per-replica, like the rest of the cache.
	safego.Go(ctx, s.logger, "dispatch-cache-budget-refresher", func(ctx context.Context) {
		cache.runBudgetRefresher(ctx, defaultBudgetRefreshInterval)
	})
	s.logger.Info("dispatch cache started",
		"admission_cap", admissionCap,
		"maintenance_admission_cap", maintCap,
		"min_send_interval_seconds", s.dispatchCfg.MinSendIntervalSeconds,
		"reliability_quota_enabled", cfg.reliabilityQuotaEnabled,
		"reliability_quota_floor", cfg.reliabilityFloor,
		"scale_out", cfg.scaleOutEnabled(),
		"head_instance_id", cfg.headID,
		"claim_lease_seconds", claimLeaseSeconds)
}

func (s *volunteerService) GetHeadInfo(ctx context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	// Uses LEFT JOINs with pre-aggregated subqueries instead of correlated subqueries to avoid
	// O(N) sequential scans per leaf when work_units table is large.
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT l.id, l.slug, l.name, l.description, l.research_area, l.task_pattern, l.state,
			COALESCE(q.cnt, 0),
			COALESCE(a.cnt, 0),
			COALESCE(hh.cnt, 0),
			l.execution_config
		FROM leafs l
		LEFT JOIN (
			SELECT leaf_id, COUNT(*) AS cnt
			FROM work_units
			WHERE state IN ('QUEUED', 'CREATED')
			GROUP BY leaf_id
		) q ON q.leaf_id = l.id
		LEFT JOIN (%s) a ON a.leaf_id = l.id
		LEFT JOIN (%s) hh ON hh.leaf_id = l.id
		WHERE l.state = 'ACTIVE' AND l.visibility = 'PUBLIC'
		ORDER BY l.name ASC`, leaf.ActiveVolunteerSubquery(), leaf.ActiveHostSubquery()))
	if err != nil {
		s.logger.Error("query leafs", "method", "GetHeadInfo", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer rows.Close()

	var leafs []*lettucev1.LeafInfo
	for rows.Next() {
		var li lettucev1.LeafInfo
		var researchArea []string
		var execConfig leaf.ExecutionConfig
		if err := rows.Scan(&li.Id, &li.Slug, &li.Name, &li.Description, &researchArea,
			&li.TaskPattern, &li.State, &li.QueuedWorkUnits, &li.ActiveVolunteers, &li.ActiveHosts, &execConfig); err != nil {
			s.logger.Error("scan leaf", "method", "GetHeadInfo", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		li.ResearchArea = researchArea
		if li.ResearchArea == nil {
			li.ResearchArea = []string{}
		}
		li.ExecutionSpec = &lettucev1.ExecutionSpec{
			Binaries:        execConfig.Binaries,
			BinaryChecksums: execConfig.BinaryChecksums,
			Image:           derefString(execConfig.Image),
			GpuRequired:     execConfig.GPURequired,
			GpuType:         execConfig.GPUType,
			MaxMemoryMb:     int32(execConfig.MaxMemoryMB),
			MaxDiskMb:       int32(execConfig.MaxDiskMB),
			NetworkAccess:   execConfig.NetworkAccess,
		}
		// #29 duration-aware batching (head side): give the volunteer a per-leaf
		// duration estimate (seconds) so it can size its FIRST batch request to fill
		// work_buffer_hours instead of idling at the flat batch cap. The head has no
		// per-host benchmark, so it derives seconds from the leaf's rsc_fpops_est
		// against a fixed reference-host FLOPS constant; the volunteer refines this
		// with its own measured benchmark + DCF once it has buffered units. Benchmark-
		// independent and seconds-based, so an un-benchmarked volunteer host still gets
		// a usable, non-zero estimate (sidestepping the benchmark<=0 zero-trap).
		li.EstimatedDurationSeconds = estimatedDurationSecondsForLeaf(execConfig.RscFpopsEst)
		leafs = append(leafs, &li)
	}
	if leafs == nil {
		leafs = []*lettucev1.LeafInfo{}
	}

	weights := s.defaultWeights
	if weights == nil {
		weights = map[string]int32{}
	}

	return &lettucev1.GetHeadInfoResponse{
		Name:               s.headName,
		Description:        s.headDescription,
		Url:                s.headURL,
		Leafs:              leafs,
		DefaultLeafWeights: weights,
	}, nil
}

func (s *volunteerService) GetServerStatus(ctx context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	st, dbStatus := checkDBHealth(ctx, s.pool)

	return &lettucev1.GetServerStatusResponse{
		Status:         st,
		Version:        s.version,
		UptimeSeconds:  int64(time.Since(s.startTime).Seconds()),
		DatabaseStatus: dbStatus,
	}, nil
}

// validRuntimes is the set of accepted runtime values.
var validRuntimes = map[string]bool{
	leaf.RuntimeNative:    true,
	leaf.RuntimeContainer: true,
	leaf.RuntimeWasm:      true,
}

// validSchedulingModes is the set of accepted scheduling mode values.
var validSchedulingModes = map[string]bool{
	"ALWAYS":    true,
	"WHEN_IDLE": true,
	"SCHEDULED": true,
}

func (s *volunteerService) RegisterVolunteer(ctx context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	// Validate public_key: must be exactly 32 bytes.
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes (Ed25519), got %d", len(req.PublicKey))
	}

	// The request signature proves possession of the private key for the public key
	// being registered. Bind the verified identity to req.PublicKey so a caller can
	// only register a public key they actually control.
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	if !bytes.Equal(authedKey, req.PublicKey) {
		return nil, status.Errorf(codes.PermissionDenied, "authenticated key does not match public_key being registered")
	}

	// Validate hardware: required.
	if req.Hardware == nil {
		return nil, status.Errorf(codes.InvalidArgument, "hardware capabilities are required")
	}
	if req.Hardware.CpuCores <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.cpu_cores must be > 0")
	}
	if req.Hardware.MaxCpuCores <= 0 || req.Hardware.MaxCpuCores > req.Hardware.CpuCores {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.max_cpu_cores must be > 0 and <= cpu_cores")
	}
	if req.Hardware.MemoryTotalMb <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.memory_total_mb must be > 0")
	}
	if req.Hardware.MaxMemoryMb <= 0 || req.Hardware.MaxMemoryMb > req.Hardware.MemoryTotalMb {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.max_memory_mb must be > 0 and <= memory_total_mb")
	}

	// Validate available_runtimes: at least one, all valid.
	if len(req.AvailableRuntimes) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "available_runtimes must contain at least one value")
	}
	for _, rt := range req.AvailableRuntimes {
		if !validRuntimes[rt] {
			return nil, status.Errorf(codes.InvalidArgument, "invalid runtime %q: must be one of [NATIVE, CONTAINER, WASM]", rt)
		}
	}

	// Validate scheduling_mode: default to ALWAYS if empty.
	schedulingMode := req.SchedulingMode
	if schedulingMode == "" {
		schedulingMode = "ALWAYS"
	}
	if !validSchedulingModes[schedulingMode] {
		return nil, status.Errorf(codes.InvalidArgument, "invalid scheduling_mode %q: must be one of [ALWAYS, WHEN_IDLE, SCHEDULED]", schedulingMode)
	}

	// Convert proto hardware to Go struct.
	hw := volunteer.HardwareCapabilitiesFromProto(req.Hardware)

	now := time.Now().UTC()
	var displayName *string
	if req.DisplayName != "" {
		displayName = &req.DisplayName
	}

	// Upsert by public key.
	existing, err := s.volunteerRepo.GetByPublicKey(ctx, req.PublicKey)
	if err != nil {
		// Check if it's a not-found error.
		apiErr, ok := err.(*apierror.APIError)
		if !ok || apiErr.HTTPStatus != 404 {
			s.logger.Error("failed to look up volunteer", "method", "RegisterVolunteer", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// Not found — create new volunteer, through the registration admission gates
		// (design §4.1). The gate is nil while the creation cap is disabled, which is
		// exactly the legacy create path; only genuinely-NEW registrations ever pay
		// admission cost (the update branch below never consults it).
		gate, gateErr := s.registrationGate(ctx)
		if gateErr != nil {
			// Fail closed: the cap is enabled but the client IP cannot be bucketed.
			// Unreachable through a real gRPC server (the rate-limit interceptor always
			// stashes an IP for this method); a misconfigured deployment must not
			// silently run uncapped.
			s.logger.Error("failed to resolve registration admission gate", "method", "RegisterVolunteer", "error", gateErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// Proof-of-work (design §4.1): enforced only on this CREATE branch and only
		// while enabled. A request with no solution gets the pinned pow-required
		// refusal — a FailedPrecondition whose text existing CLIs classify as
		// "update your build" (IsVolunteerTooOldError) and future solver-capable
		// clients match by PowRequiredMessagePrefix. A present solution rides the
		// gate into the registration transaction (single-use redeem + verify,
		// rolling back with everything else).
		if s.registrationPow.Enabled {
			if req.PowChallengeId == "" {
				return nil, status.Error(codes.FailedPrecondition, admission.PowRequiredMessage)
			}
			challengeID, idErr := types.ParseID(req.PowChallengeId)
			if idErr != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid pow_challenge_id: %v", idErr)
			}
			if gate == nil {
				gate = &admission.CreateGate{}
			}
			gate.Pow = &admission.PowRedemption{
				ChallengeID: challengeID,
				PublicKey:   req.PublicKey,
				Nonce:       req.PowNonce,
			}
		}

		v := &volunteer.Volunteer{
			PublicKey:            req.PublicKey,
			DisplayName:          displayName,
			HardwareCapabilities: hw,
			AvailableRuntimes:    req.AvailableRuntimes,
			SchedulingMode:       volunteer.SchedulingMode(schedulingMode),
			IsActive:             true,
			LastSeenAt:           &now,
		}

		createErr := s.volunteerRepo.CreateAdmitted(ctx, v, gate)
		switch {
		case createErr == nil:
			// Warm the dispatch cache's in-memory identity snapshot so the FIRST
			// RequestWorkUnit resolves this volunteer's identity/capabilities in memory
			// (Blocker 1: identity off the hot path).
			if s.dispatchCache != nil {
				s.dispatchCache.putIdentity(v)
			}

			// Per-machine host (BG-25, TODO #19): resolve this machine's server-issued
			// host id (mint on an empty request id, under the per-account cap) and
			// record its advertised runtimes/hardware against its own host row, so two
			// machines under one key no longer overwrite each other on the account row.
			issuedHostID := s.resolveRegisteredHost(ctx, v.ID, req, hw, now)

			// Per-WU-lifecycle Info (one per registration): restores per-volunteer visibility
			// now that the generic gRPC access log is demoted to Debug.
			s.logger.Info("volunteer registered", "volunteer_id", v.ID, "is_new", true)

			return &lettucev1.RegisterVolunteerResponse{
				VolunteerId: v.ID.String(),
				Registered:  true,
				HostId:      issuedHostID,
			}, nil

		case errors.Is(createErr, admission.ErrCreationCapExceeded):
			// Sampled Warn (H-5 idiom): a capped network hammering register is an abuse
			// signal, not a per-line event. FailedPrecondition, NEVER ResourceExhausted —
			// the CLI retries ResourceExhausted forever (~30s backoff) and would look
			// hung; the pinned message avoids the IsVolunteerTooOldError trigger words.
			if n := atomic.AddUint64(&s.capRefusalLogN, 1); n%shedLogSampleN == 1 {
				s.logger.Warn("refusing registration: creation cap exceeded",
					"method", "RegisterVolunteer", "bucket", gate.Bucket, "refusal_count", n)
			}
			return nil, status.Error(codes.FailedPrecondition, admission.CapExceededMessage)

		case errors.Is(createErr, admission.ErrPowChallengeInvalid) || errors.Is(createErr, admission.ErrPowSolutionInvalid):
			// A solver-capable client sent a stale/foreign challenge or a wrong
			// nonce. InvalidArgument, NOT the pow-required signal: retrying the same
			// payload cannot succeed — the client should fetch a fresh challenge and
			// re-solve. (A failed redeem rolled back, so a merely-stale challenge was
			// not burned; an expired one is gone regardless.)
			return nil, status.Errorf(codes.InvalidArgument, "registration proof-of-work rejected: %v", createErr)

		case isConflictErr(createErr):
			// Lost the get-then-create race to a concurrent registration of the same
			// key: re-fetch and fall through to the update path — the correct upsert
			// semantics (previously this surfaced as Internal).
			existing, err = s.volunteerRepo.GetByPublicKey(ctx, req.PublicKey)
			if err != nil {
				s.logger.Error("failed to re-fetch volunteer after create race", "method", "RegisterVolunteer", "error", err)
				return nil, status.Errorf(codes.Internal, "internal error")
			}

		default:
			s.logger.Error("failed to create volunteer", "method", "RegisterVolunteer", "error", createErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	}

	// Found — update existing volunteer.
	existing.DisplayName = displayName
	existing.HardwareCapabilities = hw
	existing.AvailableRuntimes = req.AvailableRuntimes
	existing.SchedulingMode = volunteer.SchedulingMode(schedulingMode)
	existing.IsActive = true
	existing.LastSeenAt = &now

	if updateErr := s.volunteerRepo.Update(ctx, existing); updateErr != nil {
		s.logger.Error("failed to update volunteer", "method", "RegisterVolunteer", "error", updateErr)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Refresh the dispatch cache's in-memory identity snapshot (the volunteer may have
	// re-registered with changed hardware/runtimes) so the hot path stays in memory.
	if s.dispatchCache != nil {
		s.dispatchCache.putIdentity(existing)
	}

	// Per-machine host (BG-25, TODO #19): resolve this machine's server-issued host id
	// (echo-refresh a known id; mint on an empty request id) + warm the per-host caches.
	issuedHostID := s.resolveRegisteredHost(ctx, existing.ID, req, hw, now)

	s.logger.Info("volunteer registered", "volunteer_id", existing.ID, "is_new", false)

	return &lettucev1.RegisterVolunteerResponse{
		VolunteerId: existing.ID.String(),
		Registered:  false,
		HostId:      issuedHostID,
	}, nil
}

// resolveRegisteredHost resolves THIS machine's server-issued host id for a
// registration (BG-25, TODO #19): one row per (account, machine), carrying the
// machine's advertised runtimes/hardware/last-seen while credit/distinctness stay per
// account. Three-way on the request's host_id:
//
//   - a known id of THIS account → echo-refresh (best-effort: a refresh failure never
//     fails registration; the id is still echoed);
//   - EMPTY → an explicit mint request: a fresh random id under the per-account cap
//     (transactional in the repo); a cap refusal returns "" — the machine works in the
//     shared per-account bucket;
//   - unknown/foreign/unparseable → "" and NOTHING is created (mint-on-unknown would
//     let pre-issuance builds, which echo self-generated ids forever, mint garbage
//     rows every restart — the client discards its stored id and re-registers empty).
//
// The returned string rides RegisterVolunteerResponse.host_id verbatim. Also warms the
// dispatch cache's per-host runtime + ownership snapshots so the machine's first work
// request resolves in memory. Returns "" in unit tests with no pool (nil hostRepo).
func (s *volunteerService) resolveRegisteredHost(ctx context.Context, volunteerID types.ID, req *lettucev1.RegisterVolunteerRequest, hw volunteer.HardwareCapabilities, now time.Time) string {
	if s.hostRepo == nil {
		return ""
	}
	var displayName *string
	if req.DisplayName != "" {
		displayName = &req.DisplayName
	}

	if raw := req.GetHostId(); raw != "" {
		id, err := types.ParseID(raw)
		if err != nil {
			return ""
		}
		existing, err := s.hostRepo.GetByID(ctx, id)
		if err != nil {
			if !isNotFound(err) {
				// A transient read failure is answered like unknown ("" → the client
				// re-registers empty and mints): the old row stays, cap-bounded and
				// aged out by eviction. Log it — a burst here means DB trouble.
				s.logger.Warn("failed to look up echoed host id; answering unknown",
					"volunteer_id", volunteerID, "host_id", id, "error", err)
			}
			return ""
		}
		if existing.VolunteerID != volunteerID {
			return ""
		}
		h := &volunteer.Host{
			ID:                   id,
			VolunteerID:          volunteerID,
			DisplayName:          displayName,
			HardwareCapabilities: hw,
			AvailableRuntimes:    req.AvailableRuntimes,
			IsActive:             true,
			LastSeenAt:           &now,
		}
		if err := s.hostRepo.Upsert(ctx, h); err != nil {
			// Best-effort refresh: ownership already verified, so still echo the id.
			s.logger.Warn("failed to refresh host row",
				"volunteer_id", volunteerID, "host_id", id, "error", err)
			s.warmHostCaches(id, volunteerID, req.AvailableRuntimes)
			return id.String()
		}
		s.warmHostCaches(id, volunteerID, req.AvailableRuntimes)
		return id.String()
	}

	// Mint path: an explicitly empty id is the client's request for a machine id.
	h := &volunteer.Host{
		ID:                   types.NewID(),
		VolunteerID:          volunteerID,
		DisplayName:          displayName,
		HardwareCapabilities: hw,
		AvailableRuntimes:    req.AvailableRuntimes,
		IsActive:             true,
		LastSeenAt:           &now,
	}
	minted, err := s.hostRepo.Mint(ctx, h, s.hostCap.PerAccount, s.hostCap.ActiveWindow)
	if err != nil {
		s.logger.Warn("failed to mint host id; falling back to per-account",
			"volunteer_id", volunteerID, "error", err)
		return ""
	}
	if !minted {
		// At cap with every slot recently active: the refusal (sampled Warn, H-5
		// idiom — an account hammering mints is a signal, not a per-line event).
		if n := atomic.AddUint64(&s.hostCapRefusalLogN, 1); n%shedLogSampleN == 1 {
			s.logger.Warn("refusing host id mint: per-account host cap reached",
				"volunteer_id", volunteerID, "cap", s.hostCap.PerAccount, "refusal_count", n)
		}
		return ""
	}
	s.warmHostCaches(h.ID, volunteerID, req.AvailableRuntimes)
	return h.ID.String()
}

// warmHostCaches warms the dispatch cache's per-host snapshots (advertised runtimes for
// the flapping-row fix; ownership for BG-25 work-path validation) at registration, the
// natural write point.
func (s *volunteerService) warmHostCaches(hostID, owner types.ID, runtimes []string) {
	if s.dispatchCache == nil {
		return
	}
	s.dispatchCache.putHostRuntimes(hostID, runtimes)
	s.dispatchCache.putHostOwner(hostID, owner)
}

func (s *volunteerService) RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	// Feed the load estimator: every call counts toward the request-rate signal
	// that drives the server-directed retry delay, regardless of outcome.
	s.loadEstimator.recordRequest()

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Validate public_key shape (not used as the auth credential — see below).
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes, got %d", len(req.PublicKey))
	}

	// Authenticated identity (cryptographically proven by the request signature).
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}

	// Resolve the volunteer's identity (pubkey + hardware + available runtimes) and
	// bind the proven identity to the volunteer being acted on.
	//
	// Blocker 1: when the dispatch cache is running, this resolves from the IN-MEMORY
	// identity snapshot (warmed at RegisterVolunteer, refreshed lazily) so the hot
	// path touches NO Postgres. Only a cold miss hits the pool, and that single read
	// is bounded by the cache's admission semaphore + a short shed timeout — under
	// overload it sheds with ResourceExhausted instead of blocking on the request ctx
	// and collapsing with "context deadline exceeded". The pre-cache fallback path
	// keeps the per-request DB read.
	var (
		identPubKey       []byte
		identHW           volunteer.HardwareCapabilities
		identRuntime      []string
		identTrustSubject string
	)
	if s.dispatchCache != nil {
		ident, notFound, shed := s.dispatchCache.resolveIdentity(volunteerID)
		if shed {
			// H-5: identity-resolve shed (DB-admission saturated on a cold miss) was
			// emitted at no level. Sampled Warn so the overload is visible.
			if n := atomic.AddUint64(&s.identityShedLogN, 1); n%shedLogSampleN == 1 {
				s.logger.Warn("shedding work request: overloaded",
					"volunteer_id", volunteerID, "ready_len", s.dispatchCache.readyLen(), "shed_count", n)
			}
			return nil, status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry")
		}
		if notFound {
			return nil, status.Errorf(codes.NotFound, "volunteer not found")
		}
		identPubKey = ident.publicKey
		identHW = ident.hardware
		identRuntime = ident.availableRuntimes
		identTrustSubject = ident.trustSubject
	} else {
		vol, gerr := s.volunteerRepo.GetByID(ctx, volunteerID)
		if gerr != nil {
			if isNotFound(gerr) {
				return nil, status.Errorf(codes.NotFound, "volunteer not found")
			}
			s.logger.Error("failed to look up volunteer", "method", "RequestWorkUnit", "error", gerr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		identPubKey = vol.PublicKey
		identHW = vol.HardwareCapabilities
		identRuntime = vol.AvailableRuntimes
		// Pre-cache fallback: resolve the trust subject straight off the fetched row via
		// the production rule (the cache path reads it from the warmed identity snapshot).
		identTrustSubject = trust.SubjectForVolunteer(vol)
	}
	if !bytes.Equal(identPubKey, authedKey) {
		return nil, status.Errorf(codes.PermissionDenied, "authenticated key does not match volunteer record")
	}

	// Per-machine host id (BG-25, TODO #19): the SERVER-ISSUED id this machine received
	// at registration, so the head meters in-flight work + the work-send floor per
	// machine and attributes copies/results to it, while credit + distinctness stay per
	// account. hostID == nil means no host -> the copy row's host_id is NULL and
	// everything falls back to per-account behavior. A NON-empty id must have been
	// issued to THIS account (the hosts row is the ledger): validation resolves through
	// the TTL'd host-owner cache — memory-only when warm, one bounded read on a cold
	// miss. Outcomes:
	//   - valid           -> per-machine metering + the throttled last-seen bump that
	//                        keeps a WORKING machine out of the cap's eviction window;
	//   - definitively unknown/foreign (incl. unparseable) -> the pinned refusal:
	//                        pre-issuance builds classify it as "run update"
	//                        (IsVolunteerTooOldError — the word "outdated"), issuance-
	//                        era builds match HostUnknownMessagePrefix and re-register;
	//   - undeterminable (admission shed / DB error) -> fold to the account bucket for
	//                        THIS request only, never refuse (a post-deploy cold cache
	//                        under reconnect load must not start a re-mint storm).
	var hostID *types.ID
	if raw := req.GetHostId(); raw != "" {
		id, perr := types.ParseID(raw)
		if perr != nil {
			return nil, status.Error(codes.FailedPrecondition, HostUnknownMessage)
		}
		if s.dispatchCache == nil {
			// gRPC-plumbing unit tests: no oracle available — fold, never refuse.
		} else if owner, found, definitive := s.dispatchCache.resolveHostOwner(id); found && owner == volunteerID {
			hostID = &id
			if s.hostRepo != nil && s.dispatchCache.shouldBumpHostSeen(id) {
				// Best-effort, throttled (hostSeenBumpInterval): the eviction clock,
				// not correctness — an error is swallowed like the account-row bump.
				_ = s.hostRepo.UpdateLastSeen(ctx, id)
			}
		} else if definitive {
			return nil, status.Error(codes.FailedPrecondition, HostUnknownMessage)
		}
	}

	// Resolve the REQUESTING host's advertised runtimes (the flapping-row fix): two
	// machines under one key advertise different runtimes, so a NATIVE-only box must not
	// inherit the account row's CONTAINER set. Warmed at register; on a cold miss (e.g.
	// just after a head restart, before the volunteer re-registers) the resolver reads the
	// authoritative hosts row once under the admission semaphore and warms the cache, so
	// the fix survives a restart without relying on re-registration. Only if that also
	// fails (shed / unknown host) does it fall back to the account's stored runtimes.
	if hostID != nil && s.dispatchCache != nil {
		if rts, ok := s.dispatchCache.resolveHostRuntimes(*hostID); ok {
			identRuntime = rts
		}
	}

	// Determine capabilities: use current_available if provided, else registered.
	hw := identHW
	if req.CurrentAvailable != nil {
		hw = volunteer.HardwareCapabilitiesFromProto(req.CurrentAvailable)
	}

	// Parse leaf preferences (empty = any matching leaf).
	leafIDs, err := parseIDSlice(req.GetLeafIds())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid leaf_id: %v", err)
	}
	blockedIDs, err := parseIDSlice(req.GetBlockedLeafIds())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blocked_leaf_id: %v", err)
	}

	// Compute GPU capabilities.
	hasGPU := len(hw.GPUs) > 0
	maxGPUVRAM := 0
	var gpuVendors []string
	var gpuCapabilities []string
	for _, gpu := range hw.GPUs {
		effective := gpu.VRAMMB * gpu.MaxVRAMPct / 100
		if effective > maxGPUVRAM {
			maxGPUVRAM = effective
		}
		vendor := strings.ToUpper(gpu.Vendor)
		gpuVendors = append(gpuVendors, vendor)
		if gpu.ComputeCapability != "" {
			gpuCapabilities = append(gpuCapabilities, gpu.ComputeCapability)
		}
	}

	opts := workunit.AssignmentOptions{
		VolunteerID:             volunteerID,
		LeafIDs:                 leafIDs,
		BlockedLeafIDs:          blockedIDs,
		MaxCPUCores:             hw.MaxCPUCores,
		MaxMemoryMB:             hw.MaxMemoryMB,
		MaxDiskMB:               hw.MaxDiskMB,
		HasGPU:                  hasGPU,
		MaxGPUVRAMMB:            maxGPUVRAM,
		AvailableRuntimes:       identRuntime,
		GPUVendors:              gpuVendors,
		GPUComputeCapabilities:  gpuCapabilities,
		MaxInflightPerVolunteer: s.maxInflightPerVolunteer,
		// Homogeneous Redundancy: the requester's hardware class. Always populated; it only
		// filters units that are actually pinned (HR-enabled leaves), so non-HR leaves are
		// unaffected (their hr_class stays NULL).
		HRClass: hw.HRClass(),
		// Per-machine host id (TODO #19): stamped on the copy row for attribution. nil when
		// no host reported (the copy stays NULL → counts under the account). Stage-B metering
		// (inflight cap / send floor) keys on the effective host id derived from this.
		HostID: hostID,
		// Feasibility-at-dispatch: the requester's measured benchmark. Sourced from the
		// STORED hardware (identHW) so it matches what the SQL gates read from
		// volunteers.hardware_capabilities; a unit whose estimated runtime on this host
		// exceeds its deadline is excluded (see FeasibleByDeadline).
		BenchmarkFPOPS: identHW.BenchmarkFPOPS,
		// Account-level trust subject (trust.SubjectForVolunteer): the requester's DID
		// while its binding is live, else the per-keypair sentinel. Consumed ONLY by the
		// in-memory dispatch predicate for per-PRINCIPAL distinctness (two devices under one
		// live DID are one subject, so they never both take a copy of one unit); the SQL
		// gates recompute the subject fresh, so a subject that went stale since the snapshot
		// was warmed costs at most a voided hand-out.
		TrustSubject: identTrustSubject,
	}

	// Server-directed retry delay: computed once from the current load and
	// stamped on EVERY reply (work and no-work). Measure load before the
	// (possibly empty) batch fill so the delay reflects pressure at arrival.
	load := s.loadEstimator.currentLoad()
	retryAfter := s.loadEstimator.computeRetryDelaySeconds(load)

	// Batch size: client's requested max, clamped to ≥1 and the server batch cap
	// (now a SAFETY CEILING, not the limiter — the client's hours-deficit math
	// binds for short-unit leafs).
	n := int(req.GetMaxAssignments())
	if n < 1 {
		n = 1
	}
	if n > s.maxBatchPerRequest {
		n = s.maxBatchPerRequest
	}

	// Record the requesting MACHINE's reported client-buffer contents (the work units it
	// currently holds) so the head can reconcile its per-machine reservations against what
	// that machine actually has — releasing buffered reservations it no longer holds (e.g.
	// dropped across a client restart) so they stop counting against its inflight cap and
	// the units redispatch. Keyed per host (TODO #19) so a user's two machines never evict
	// each other's buffers. Parsed leniently: a malformed id is skipped rather than failing
	// the work request.
	if s.dispatchCache != nil {
		held := make([]types.ID, 0, len(req.GetHeldWorkUnitIds()))
		for _, raw := range req.GetHeldWorkUnitIds() {
			if id, perr := types.ParseID(raw); perr == nil {
				held = append(held, id)
			}
		}
		s.dispatchCache.NoteVolunteerHeld(volunteerID, meterID(volunteerID, hostID), held)
	}

	// Layer 2: serve from the in-process dispatch cache when it is running (the
	// hot path touches NO Postgres). Falls back to the Layer-1 per-request
	// transaction when the cache is not started (gRPC-plumbing unit tests).
	if s.dispatchCache != nil {
		return s.requestWorkUnitFromCache(volunteerID, opts, n, retryAfter)
	}
	return s.requestWorkUnitFromDB(ctx, volunteerID, opts, n, retryAfter)
}

// requestWorkUnitFromCache serves a hand-out from the in-memory dispatch cache
// (zero DB I/O on this path) and sheds with ResourceExhausted under overload.
func (s *volunteerService) requestWorkUnitFromCache(volunteerID types.ID, opts workunit.AssignmentOptions, n int, retryAfter int32) (*lettucev1.RequestWorkUnitResponse, error) {
	cache := s.dispatchCache

	// GRACEFUL SHEDDING: if the ready pool is empty AND the cache's DB-admission
	// semaphore is saturated (a refill is already maxed out), shed immediately —
	// return ResourceExhausted with NO DB touch and NO pool wait. The volunteer
	// converts this to a fixed jittered local backoff. This is the hard backstop
	// against the DB-pool congestion collapse.
	if cache.readyLen() == 0 && cache.admissionSaturated() {
		cache.signalRefill()
		// H-5: hard-backstop cache shed (empty pool + saturated admission) was emitted at
		// no level. Sampled Warn so a sustained overload is visible without per-request spam.
		if n := atomic.AddUint64(&s.cacheShedLogN, 1); n%shedLogSampleN == 1 {
			s.logger.Warn("shedding work request: overloaded",
				"volunteer_id", volunteerID, "ready_len", 0, "shed_count", n)
		}
		return nil, status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry")
	}

	assignStart := time.Now()
	results, _ := cache.HandOut(volunteerID, opts, n)
	// The near-zero in-memory hand-out duration folds into the latency signal,
	// lowering the latency-saturation component of the load estimate.
	s.loadEstimator.recordAssignLatency(time.Since(assignStart))

	if len(results) == 0 {
		return &lettucev1.RequestWorkUnitResponse{
			Assignments:       nil,
			RetryAfterSeconds: retryAfter,
		}, nil
	}

	assignments := make([]*lettucev1.WorkUnitAssignment, 0, len(results))
	for _, r := range results {
		assignments = append(assignments, buildWorkUnitAssignment(r.unit, r.leaf, r.execConfig))
	}
	return &lettucev1.RequestWorkUnitResponse{
		Assignments:       assignments,
		RetryAfterSeconds: retryAfter,
	}, nil
}

// requestWorkUnitFromDB is the Layer-1 per-request reservation path, retained for
// deployments / tests that do not start the dispatch cache. It opens ONE
// transaction for the whole batch and loops ReserveNextAssignable, preserving
// every reservation/redundancy/spot-check property in SQL.
func (s *volunteerService) requestWorkUnitFromDB(ctx context.Context, volunteerID types.ID, opts workunit.AssignmentOptions, n int, retryAfter int32) (*lettucev1.RequestWorkUnitResponse, error) {
	lease := time.Duration(s.leaseSeconds) * time.Second

	// Begin ONE transaction for the whole batch: amortizes the transaction +
	// round-trip cost across N units. SKIP LOCKED keeps concurrent multi-row
	// claims safe. Acquire bounded — the BG-17 backstop.
	tx, err := beginTxBounded(ctx, s.pool)
	if err != nil {
		s.logger.Error("failed to begin transaction", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer tx.Rollback(ctx)

	// Carry the head trust-gate policy so ReserveNextAssignable -> FindNextAssignable applies
	// the trusted-corroborator reservation on this Layer-1 fallback dispatch path identically
	// to the cache path (inert when the gate is off).
	txWURepo := workunit.NewPgxWorkUnitRepository(tx).WithTrustDispatch(trustDispatchFromPolicy(s.trustPolicy))

	// leafCache collapses repeated GetByID lookups so N units from one leaf cost
	// one lookup (the spot-check check and the response build share it). The leaf
	// is read on THIS handler's tx connection (leaf.GetByIDTx), NOT via the
	// pool-backed s.leafRepo: acquiring a second pool connection while already
	// holding the reserve-loop tx connection is what starved/self-deadlocked the
	// pool under concurrent batched RequestWorkUnit calls (handlers holding a tx
	// connection blocked ~29s on a getLeaf connection that never freed). One
	// handler now holds exactly one pool connection for the whole batch.
	leafCache := make(map[types.ID]*leaf.Leaf)
	getLeaf := func(id types.ID) (*leaf.Leaf, error) {
		if lf, ok := leafCache[id]; ok {
			return lf, nil
		}
		lf, lerr := leaf.GetByIDTx(ctx, tx, id)
		if lerr != nil {
			return nil, lerr
		}
		leafCache[id] = lf
		return lf, nil
	}

	type reservedUnit struct {
		wu   *workunit.WorkUnit
		leaf *leaf.Leaf
	}
	var reserved []reservedUnit

	assignStart := time.Now()
	for i := 0; i < n; i++ {
		wu, ferr := txWURepo.ReserveNextAssignable(ctx, opts, lease)
		if ferr != nil {
			s.logger.Error("failed to reserve assignable work unit", "method", "RequestWorkUnit", "error", ferr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		if wu == nil {
			break // no more assignable work for this volunteer right now
		}

		lf, lerr := getLeaf(wu.LeafID)
		if lerr != nil {
			s.logger.Error("failed to get leaf for assignment", "method", "RequestWorkUnit", "leaf_id", wu.LeafID, "error", lerr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// Per-copy dispatch: ReserveNextAssignable already inserted this volunteer's
		// RESERVED copy row (a work_unit_assignment_history row held until the unit's
		// deadline). The unit stays QUEUED so up to redundancy distinct volunteers each
		// get their own parallel copy. A spot-check decision on a redundancy-1 unit
		// just flips spot_check on the unit so it waits for a SECOND corroborator; the
		// copy row is already in place.
		if !wu.SpotCheck &&
			lf.ValidationConfig.SpotCheckEnabled &&
			lf.ValidationConfig.RedundancyFactor == 1 &&
			workunit.ShouldSpotCheck(lf.ValidationConfig.SpotCheckPercentage) {
			if serr := txWURepo.MarkSpotCheck(ctx, wu.ID); serr != nil {
				s.logger.Error("failed to mark spot-check", "method", "RequestWorkUnit", "error", serr)
				return nil, status.Errorf(codes.Internal, "internal error")
			}
			wu.SpotCheck = true
		}
		reserved = append(reserved, reservedUnit{wu: wu, leaf: lf})
	}
	// Fold the batch's assignment-query cost into the latency signal (per call,
	// not per unit, so an empty no-work probe still reports its find cost).
	s.loadEstimator.recordAssignLatency(time.Since(assignStart))

	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("failed to commit assignment", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Update volunteer last_seen (best effort, outside transaction, once per call).
	_ = s.volunteerRepo.UpdateLastSeen(ctx, volunteerID)
	_ = s.volunteerRepo.SetActive(ctx, volunteerID, true)

	// No work right now: OK response carrying only the server-directed delay.
	if len(reserved) == 0 {
		return &lettucev1.RequestWorkUnitResponse{
			Assignments:       nil,
			RetryAfterSeconds: retryAfter,
		}, nil
	}

	assignments := make([]*lettucev1.WorkUnitAssignment, 0, len(reserved))
	for _, ru := range reserved {
		assignments = append(assignments, buildWorkUnitAssignment(ru.wu, ru.leaf, nil))
	}

	return &lettucev1.RequestWorkUnitResponse{
		Assignments:       assignments,
		RetryAfterSeconds: retryAfter,
	}, nil
}

// buildWorkUnitAssignment renders a reserved work unit + its leaf into the proto
// assignment, including the lease expiry (reserved_until_unix).
func buildWorkUnitAssignment(wu *workunit.WorkUnit, lf *leaf.Leaf, exec *leaf.ExecutionConfig) *lettucev1.WorkUnitAssignment {
	// ec is the EFFECTIVE artifact config: the pinned version's config when supplied
	// (homogeneous redundancy across a mid-flight publish), else the leaf's current
	// (denormalized) config.
	ec := &lf.ExecutionConfig
	if exec != nil {
		ec = exec
	}
	a := &lettucev1.WorkUnitAssignment{
		WorkUnitId:      wu.ID.String(),
		LeafId:          wu.LeafID.String(),
		Runtime:         ec.Runtime,
		InputData:       wu.InputData,
		InputDataUrl:    derefString(wu.InputDataRef),
		CodeArtifactUrl: wu.CodeArtifactRef,
		ParametersJson:  string(wu.Parameters),
		DeadlineSeconds: int32(wu.DeadlineSeconds),
		EnvVars:         ec.EnvVars,
		RscFpopsEst:     ec.RscFpopsEst,
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Binaries:        ec.Binaries,
			BinaryChecksums: ec.BinaryChecksums,
			Image:           derefString(ec.Image),
			GpuRequired:     ec.GPURequired,
			GpuType:         ec.GPUType,
			MaxMemoryMb:     int32(ec.MaxMemoryMB),
			MaxDiskMb:       int32(ec.MaxDiskMB),
			NetworkAccess:   ec.NetworkAccess,
		},
	}
	if wu.ReservedUntil != nil {
		a.ReservedUntilUnix = wu.ReservedUntil.Unix()
	}
	if wu.LastCheckpointSequence > 0 {
		a.HasCheckpoint = true
		a.CheckpointSequence = int32(wu.LastCheckpointSequence)
	}
	if lf.FaultToleranceConfig.CheckpointingEnabled && lf.FaultToleranceConfig.CheckpointIntervalSeconds != nil {
		a.CheckpointIntervalSeconds = int32(*lf.FaultToleranceConfig.CheckpointIntervalSeconds)
	}
	return a
}

// parseIDSlice parses a slice of UUID strings into a slice of types.ID.
func parseIDSlice(ids []string) ([]types.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	result := make([]types.ID, len(ids))
	for i, s := range ids {
		id, err := types.ParseID(s)
		if err != nil {
			return nil, err
		}
		result[i] = id
	}
	return result, nil
}

// sha256HexRegex validates a 64-character lowercase hex SHA-256 digest.
var sha256HexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *volunteerService) SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Validate public_key shape (not used as the auth credential — see below).
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes, got %d", len(req.PublicKey))
	}

	// FIX 2: bind the cryptographically proven identity to the volunteer via the
	// admission-bounded identity snapshot (warm = no pool touch), replacing the
	// inline UNBOUNDED s.volunteerRepo.GetByID that collapsed the pool under a submit
	// storm. Runs BEFORE the FIX-3 shed gate so resolveIdentity's own cold-miss
	// acquire never overlaps the handler's held slot.
	if err := s.resolveAuthedVolunteer(ctx, volunteerID, "SubmitResult"); err != nil {
		return nil, err
	}

	// Validate output data: at least one of output_data or output_data_url required.
	if len(req.OutputData) == 0 && req.OutputDataUrl == "" {
		return nil, status.Errorf(codes.InvalidArgument, "either output_data or output_data_url must be provided")
	}

	// Validate checksum format.
	if !sha256HexRegex.MatchString(req.OutputChecksumSha256) {
		return nil, status.Errorf(codes.InvalidArgument, "output_checksum_sha256 must be a 64-character lowercase hex string")
	}

	// If inline output_data, verify SHA-256 matches.
	if len(req.OutputData) > 0 {
		hash := sha256.Sum256(req.OutputData)
		computed := hex.EncodeToString(hash[:])
		if computed != req.OutputChecksumSha256 {
			return nil, status.Errorf(codes.InvalidArgument, "output_checksum_sha256 mismatch: computed %s, got %s", computed, req.OutputChecksumSha256)
		}
	}

	// Validate metadata.
	if req.Metadata == nil {
		return nil, status.Errorf(codes.InvalidArgument, "metadata is required")
	}
	if req.Metadata.WallClockSeconds <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.wall_clock_seconds must be > 0")
	}

	// FIX 3 — GRACEFUL SHEDDING: bound SubmitResult's DB work by the dispatch
	// admission semaphore + a short shed ctx so a submit storm fails fast
	// (ResourceExhausted) instead of collapsing the pool. Placed after the cheap,
	// pool-free validation and AFTER the bounded auth (FIX 2) — immediately before the
	// first pool touch. ctx is reassigned so every subsequent DB call (size-check
	// GetByID, pool.Begin, FindActive..., the COMPLETED tx, onUnitDone) runs on the
	// bounded shedCtx. The !ok return happens BEFORE any pool.Begin/GetByID, so a shed
	// never opens a tx or grabs a connection. Mirrors StartWork.
	if s.dispatchCache != nil {
		shedCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
		defer cancel()
		release, ok := s.dispatchCache.acquire(shedCtx)
		if !ok {
			return nil, status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry SubmitResult")
		}
		defer release()
		ctx = shedCtx
	}

	// The leaf backing this work unit governs two pre-storage gates, so it is loaded
	// once here (before opening a transaction) when either applies:
	//
	//   1. External-reference gate: an external output (output_data_url) is accepted only
	//      when the leaf opted in (allow_external_output), the head knob
	//      LETTUCE_HEAD_CONTENT_FETCH_ENABLED is on, and the URL passes the D10 allowlist
	//      shape check. The reference is then HELD (AWAITING_CONTENT_VERIFICATION) while
	//      the head fetches the URL and hashes the served bytes itself — it never votes on
	//      the volunteer-claimed checksum (design §10.2/§10.6), which under EXACT comparison
	//      would otherwise become the agreement key, making "agreement" merely colluders
	//      repeating a string. With the knob off every reference is refused here, so the
	//      held pipeline is not even entered.
	//   2. M3 output cap: enforce the leaf's per-result max_output_size_bytes on the
	//      INLINE output, so an authenticated, assigned volunteer cannot store output far
	//      larger than configured (unbounded JSONB storage and memory pressure — the
	//      aggregation engine later loads all agreed outputs into memory). Applies only
	//      to inline output_data; an external reference carries no inline bytes here.
	if len(req.OutputData) > 0 || req.OutputDataUrl != "" {
		submitWU, wuErr := s.wuRepo.GetByID(ctx, workUnitID)
		if wuErr != nil {
			apiErr, ok := wuErr.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				return nil, status.Errorf(codes.NotFound, "work unit not found")
			}
			s.logger.Error("failed to load work unit for output gate", "error", wuErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		submitLeaf, leafErr := s.leafRepo.GetByID(ctx, submitWU.LeafID)
		if leafErr != nil {
			s.logger.Error("failed to load leaf for output gate", "error", leafErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// Gate 1: external reference. Three ordered checks, each failing closed (§10.2):
		// leaf opt-in, then the head knob, then the D10 URL/allowlist shape. The URL
		// string is stored verbatim in output_data_ref; the head fetches and hashes the
		// served bytes before the result may vote.
		if req.OutputDataUrl != "" {
			if !submitLeaf.ValidationConfig.AllowExternalOutput {
				return nil, status.Errorf(codes.InvalidArgument,
					"this leaf accepts inline output_data only; external output_data_url is not permitted")
			}
			if !s.contentFetchEnabled {
				return nil, status.Errorf(codes.FailedPrecondition,
					"external output verification is disabled on this head; submit inline output_data")
			}
			if err := leaf.ValidateExternalOutputURL(req.OutputDataUrl, submitLeaf.ValidationConfig.ExternalOutputHosts); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
		}

		// Gate 2: inline output size cap. MaxOutputSizeBytes is always > 0 for a stored
		// leaf (ValidateDataConfig requires > 0 and ApplyDataConfigDefaults fills 0 with
		// a 100MB default), but we still guard on > 0 so a max of 0 is treated as
		// "unlimited" and never rejects a legitimate submission.
		if len(req.OutputData) > 0 {
			maxOut := submitLeaf.DataConfig.MaxOutputSizeBytes
			if maxOut > 0 && int64(len(req.OutputData)) > maxOut {
				return nil, status.Errorf(codes.InvalidArgument,
					"output_data size %d bytes exceeds leaf max_output_size_bytes %d", len(req.OutputData), maxOut)
			}
		}

		// Gate 3: inline output content validation (design §4.3). Enforce at the door
		// exactly what this leaf's own comparator will later require of an inline output:
		// non-empty well-formed JSON for every leaf, plus — for a NUMERIC_TOLERANCE leaf —
		// the same numeric flatten under the leaf's configured ignore_fields/compare_fields.
		// A malformed, empty, or float64-overflow (1e400) output used to abort the whole
		// comparison and park its unit COMPLETED forever (BG-21a); refusing it here surfaces
		// the problem to the submitting client immediately as InvalidArgument instead of as a
		// mystery DISAGREED at validation time. Inline only: a ref-only submission
		// (output_data_url) carries no inline bytes and is owned by the content-verification
		// pipeline. The empty-inline-and-no-URL case is already refused near the top of the
		// handler ("either output_data or output_data_url must be provided").
		if req.OutputDataUrl == "" {
			if verr := validation.ValidateSubmittedOutput(submitLeaf.ValidationConfig, req.OutputData); verr != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", verr.Error())
			}
		}
	}

	// Every pool-backed read the rest of this handler needs happens BEFORE the
	// transaction opens (BG-17). A transaction holds one pool connection until
	// commit/rollback; a pool-backed repository call inside it acquires a SECOND
	// connection, and under a submit storm N handlers holding N tx connections all
	// wait for an (N+1)th that only another stuck handler could release — the pool
	// self-deadlock RequestWorkUnit already fixed (see requestWorkUnitFromDB's
	// getLeaf). These reads depend only on workUnitID/volunteerID, both known here,
	// so hoisting them is behavior-neutral; the one read that follows tx state (the
	// completion leaf) runs on the tx connection via leaf.GetByIDTx below.

	// Stamp the result with the artifact version the volunteer ran (TODO #38): the
	// unit's pinned version (redundancy>1) else the leaf's current version. Drives
	// version-homogeneous validation and per-result provenance. Best-effort: a resolve
	// failure (or an unversioned leaf) leaves it nil — legacy behavior.
	var artifactVersionID *types.ID
	if s.artifactVersionRepo != nil {
		if vid, verr := s.artifactVersionRepo.ResolveWorkUnitVersion(ctx, workUnitID); verr == nil {
			artifactVersionID = vid
		}
	}

	// Stamp the account-level trust snapshot (see internal/trust): the subject resolved for
	// this volunteer and its quorum-power score AT SUBMIT time. Loading the volunteer row here
	// is an extra read on the bounded shed ctx, but trust must never block work: a load failure
	// falls back to the sentinel subject with score 0 rather than failing the submission.
	var trustVol *volunteer.Volunteer
	if v, verr := s.volunteerRepo.GetByID(ctx, volunteerID); verr != nil {
		s.logger.Warn("failed to load volunteer for trust snapshot; stamping sentinel subject with score 0",
			"volunteer_id", volunteerID, "error", verr)
	} else {
		trustVol = v
	}
	trustSubject, trustScore, standingAtSubmit := stampTrustSnapshot(ctx, s.trustRepo, trustVol, volunteerID, s.now(), s.logger)

	// Begin transaction (acquire bounded — the BG-17 backstop).
	tx, err := beginTxBounded(ctx, s.pool)
	if err != nil {
		s.logger.Error("failed to begin transaction", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer tx.Rollback(ctx)

	txAssignRepo := assignment.NewPgxRepository(tx)
	txResultRepo := result.NewPgxRepository(tx)
	txWURepo := workunit.NewPgxWorkUnitRepository(tx)

	// Per-copy dispatch: with N copies of a unit running in PARALLEL, several results
	// can land concurrently. Serialize submits for the SAME unit by locking its row, so
	// the PENDING-result count used to decide COMPLETED is accurate (the last result to
	// meet redundancy reliably triggers completion, with no lost-completion race).
	if _, lerr := tx.Exec(ctx, `SELECT 1 FROM work_units WHERE id = $1 FOR UPDATE`, workUnitID); lerr != nil {
		s.logger.Error("failed to lock work unit for submit", "error", lerr)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Load the unit ONCE under the row lock (design §4.1, review #2b). We hold this lock
	// for the whole transaction, so the unit row cannot change under us: the result insert
	// does not touch it, and the COMPLETED write below only advances a QUEUED/ASSIGNED/
	// RUNNING unit — never a terminal one. This single read serves (i) the live-copy
	// fast-path terminal check below, (ii) the no-copy grace-path terminal check, and
	// (iii) the later completion-quorum resolution (no second GetByID). A missing unit is
	// NotFound, preserving the grace path's prior behavior.
	submitUnit, wuErr := txWURepo.GetByID(ctx, workUnitID)
	if wuErr != nil {
		if wuAPIErr, ok := wuErr.(*apierror.APIError); ok && wuAPIErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "work unit not found")
		}
		s.logger.Error("failed to load work unit for submit", "error", wuErr)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Verify an OPEN copy exists for this volunteer.
	activeAssignment, err := txAssignRepo.FindActiveByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if !ok || apiErr.HTTPStatus != 404 {
			s.logger.Error("failed to check assignment", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// LATE-RESULT GRACE: there is no open copy for this volunteer. The common
		// cause is a copy whose deadline lapsed and was closed by the fault monitor
		// while the volunteer was still finishing the unit (for example across a
		// scheduled pause). Rather than discard finished work, accept it under grace
		// when BOTH hold:
		//   (1) the unit is not yet finalized (not VALIDATED/FAILED) — a finalized
		//       unit was already credited/assimilated, so a late result is useless; and
		//   (2) this volunteer's most recent copy was closed by timeout or
		//       abandonment (EXPIRED/ABANDONED), not by a prior COMPLETED submission.
		// The result then corroborates like any other; uq_results_work_unit_volunteer
		// prevents a double count. The unit row is already locked FOR UPDATE above, so
		// this races safely against a concurrent finalize.
		// Reuse the unit loaded once under the row lock above. The grace path keeps its
		// existing terminal-state refusal (there is no live copy to close here).
		if submitUnit.State == workunit.WorkUnitStateValidated || submitUnit.State == workunit.WorkUnitStateFailed {
			return nil, status.Errorf(codes.FailedPrecondition, "work unit already finalized; result is too late to accept")
		}

		latest, latestErr := txAssignRepo.FindLatestByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
		if latestErr != nil || latest.Outcome == nil ||
			(*latest.Outcome != assignment.OutcomeExpired && *latest.Outcome != assignment.OutcomeAbandoned) {
			return nil, status.Errorf(codes.FailedPrecondition, "no active assignment for this volunteer and work unit")
		}

		activeAssignment = latest
		s.logger.Info("late result accepted under grace (copy deadline had lapsed)",
			"work_unit_id", workUnitID,
			"volunteer_id", volunteerID,
			"copy_id", latest.ID,
			"prior_outcome", string(*latest.Outcome),
		)
	}

	// Submit-door terminal-state check — the live-copy fast path (design §4.1, review #2b).
	// If the unit already finalized (VALIDATED/FAILED), close the caller's still-live copy
	// SUPERSEDED (non-punitive), COMMIT so the supersede persists, and refuse — inserting NO
	// result row that would otherwise sit PENDING under a terminal unit and never be
	// adjudicated (★E1-6). Both the accept transaction and this submit transaction hold the
	// unit row lock, so whichever commits second sees the truth; the door no longer relies on
	// the post-commit ExpireLiveCopies supersede winning a race. The grace path above already
	// refused terminal units (it has no live copy to close), so reaching this branch with a
	// terminal unit means we hold a live copy to supersede.
	if submitUnit.State == workunit.WorkUnitStateValidated || submitUnit.State == workunit.WorkUnitStateFailed {
		if err := txAssignRepo.UpdateOutcome(ctx, activeAssignment.ID, assignment.OutcomeSuperseded, nil); err != nil {
			s.logger.Error("failed to supersede live copy on finalized unit", "work_unit_id", workUnitID, "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		if err := tx.Commit(ctx); err != nil {
			s.logger.Error("failed to commit supersede on finalized unit", "work_unit_id", workUnitID, "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		return nil, status.Errorf(codes.FailedPrecondition, "work unit already finalized; result is too late to accept")
	}

	// Count existing PENDING results to determine if work unit should transition to COMPLETED.
	// Must count only PENDING (not DISAGREED from prior rounds) so reassigned work units
	// still transition on their first new result.
	existingCount, err := txResultRepo.CountPendingByWorkUnit(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to check existing results", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Build result.
	var outputData json.RawMessage
	if len(req.OutputData) > 0 {
		outputData = json.RawMessage(req.OutputData)
	}
	var outputDataRef *string
	if req.OutputDataUrl != "" {
		outputDataRef = &req.OutputDataUrl
	}

	// A ref-only submission (external output_data_url) is HELD, not PENDING (§10.2): the
	// head must fetch the URL and hash the served bytes before the result may vote, so
	// (1) the row lands AWAITING_CONTENT_VERIFICATION with a fetch scheduled now, and
	// (2) it contributes +0 to the quorum that flips the unit COMPLETED — a held result
	// must not complete a unit it cannot yet corroborate. The claimed checksum stays in
	// output_checksum as a recorded claim; verified_output_checksum stays nil until the
	// head hashes real bytes. Inline submissions are unchanged (PENDING, +1, no fetch).
	validationStatus := result.ValidationPending
	pendingDelta := 1
	var contentFetchNextAttemptAt *time.Time
	if req.OutputDataUrl != "" {
		validationStatus = result.ValidationAwaitingContentVerification
		pendingDelta = 0
		heldAt := s.now()
		contentFetchNextAttemptAt = &heldAt
	}

	r := &result.Result{
		WorkUnitID:        workUnitID,
		VolunteerID:       volunteerID,
		OutputData:        outputData,
		OutputDataRef:     outputDataRef,
		OutputChecksum:    req.OutputChecksumSha256,
		ExecutionMetadata: result.ExecutionMetadataFromProto(req.Metadata),
		ValidationStatus:  validationStatus,
		ArtifactVersionID: artifactVersionID,
		// Attribute the result to the MACHINE that produced it by copying the host id off
		// the live copy row (TODO #19), rather than trusting a separately-sent field — so
		// the result's host is exactly the host the work was reserved/run under. nil for a
		// volunteer that reported no host (per-account fallback).
		HostID: activeAssignment.HostID,
		// Account-level trust snapshot (see internal/trust): validation acceptance reads the
		// submission-time subject + score, not a later re-read a slash/accrual could change.
		TrustSubject:       &trustSubject,
		TrustScoreAtSubmit: &trustScore,
		// Effective account standing at submit (see internal/standing): validation counts
		// only OK-stamped results toward quorum and redundancy coverage.
		StandingAtSubmit: &standingAtSubmit,
		// A ref-only submission is held for content verification: schedule the first fetch
		// attempt now. nil for inline results, which never enter the fetch pipeline.
		ContentFetchNextAttemptAt: contentFetchNextAttemptAt,
	}

	// Insert result.
	if err := txResultRepo.Create(ctx, r); err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 409 {
			return &lettucev1.SubmitResultResponse{
				Accepted: false,
				Message:  "duplicate submission",
			}, nil
		}
		s.logger.Error("failed to create result", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Update assignment outcome to COMPLETED with result_id.
	if err := txAssignRepo.UpdateOutcome(ctx, activeAssignment.ID, assignment.OutcomeCompleted, &r.ID); err != nil {
		s.logger.Error("failed to update assignment outcome", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Determine when to transition to COMPLETED, reusing the unit loaded once under the row
	// lock above (submitUnit): our lock holds for the whole transaction and the result insert
	// does not touch the unit row, so a second GetByID here would be redundant. For a leaf
	// with redundancy > 1 the unit waits for all results; a spot-check unit requires 2.

	// The COMPLETED threshold is the unit's effective QUORUM (TODO #50) — how many results
	// are needed to attempt validation. Resolved through the single source (ResolvePolicy):
	// for any leaf that only sets redundancy_factor this equals redundancy_factor (2 for a
	// spot-check unit), identical to before. The actual validate/reject/wait/dead-letter
	// decision is delegated to the transitioner below. Read on THIS handler's tx
	// connection (leaf.GetByIDTx), NOT via the pool-backed s.leafRepo — a pool read here
	// would acquire a second connection while the tx holds one (BG-17).
	quorum := 1
	completionLeaf, clErr := leaf.GetByIDTx(ctx, tx, submitUnit.LeafID)
	if clErr == nil {
		quorum = transition.ResolvePolicy(completionLeaf, submitUnit).MinQuorum
	}

	if existingCount+pendingDelta >= quorum {
		// Redundancy met: this submit completed the last needed copy. Mark the unit
		// COMPLETED (a submitted result implies the work ran). The unit was QUEUED
		// throughout (its copies ran in parallel); the ASSIGNED/RUNNING values are
		// tolerated only for any legacy in-flight unit during migration.
		_, err := tx.Exec(ctx, `
			UPDATE work_units SET
				state = 'COMPLETED',
				started_at = COALESCE(started_at, NOW()),
				completed_at = NOW()
			WHERE id = $1 AND state IN ('QUEUED', 'ASSIGNED', 'RUNNING')`,
			workUnitID,
		)
		if err != nil {
			s.logger.Error("failed to transition work unit to COMPLETED", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	}
	// Redundancy not yet met: no unit-state change is needed. This volunteer's copy is
	// closed (UpdateOutcome above) and its PENDING result holds a redundancy slot; the
	// unit stays QUEUED so its REMAINING copies — already dispatched in parallel to
	// other volunteers — continue and corroborate. (No requeue-on-partial: copies are
	// parallel, not serial.)

	// Commit transaction.
	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("failed to commit result submission", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Dispatch cache: a completed unit must not linger in the in-memory ledger or
	// ready pool. Evicting on full completion clears any stale snapshot/holder; a
	// partial (redundancy>1) submit converted its hold to a history row at run-start,
	// so there is nothing to release here for that case (the reconcile owns it).
	if s.dispatchCache != nil && existingCount+pendingDelta >= quorum {
		s.dispatchCache.onUnitDone(workUnitID)
	}

	// Best-effort updates outside the transaction.
	_ = s.volunteerRepo.UpdateLastSeen(ctx, volunteerID)

	// Increment batch completed counter when the work unit transitioned to COMPLETED.
	// Reuse submitUnit from the transaction — no need for a second DB fetch.
	if existingCount+pendingDelta >= quorum && s.batchRepo != nil {
		if submitUnit.BatchID != nil {
			_ = s.batchRepo.IncrementCompleted(ctx, *submitUnit.BatchID)
		}
	}

	// Delegate the redundancy decision to the SINGLE transitioner (TODO #50): it loads the
	// unit + copies + results + policy, runs the pure Decide, and applies the one outcome
	// (validate / reject / wait / dead-letter) under a per-unit lock. This replaces the inline
	// TryValidate call — the transitioner is now the sole decider of work-unit state.
	validationOutcome := ""
	if s.transitioner != nil {
		outcome, tErr := s.transitioner.Evaluate(ctx, workUnitID)
		if tErr != nil {
			s.logger.Error("transition evaluation failed after result submission",
				"work_unit_id", workUnitID,
				"error", tErr,
			)
		} else {
			validationOutcome = string(outcome)
		}
	}

	// Clean up checkpoints for completed work units (VALIDATED or FAILED).
	if s.checkpointRepo != nil {
		postWU, postErr := s.wuRepo.GetByID(ctx, workUnitID)
		if postErr == nil && (postWU.State == workunit.WorkUnitStateValidated || postWU.State == workunit.WorkUnitStateFailed) {
			if postWU.LastCheckpointSequence > 0 {
				if cpErr := s.checkpointRepo.Delete(ctx, workUnitID); cpErr != nil {
					s.logger.Error("failed to clean up checkpoint after completion",
						"work_unit_id", workUnitID,
						"state", postWU.State,
						"error", cpErr,
					)
				}
			}
		}
	}

	// Per-WU-lifecycle Info (one per accepted result): restores per-WU visibility now
	// that the generic gRPC access log is demoted to Debug. `completed` reports whether
	// this submit met redundancy and transitioned the unit to COMPLETED.
	s.logger.Info("result accepted",
		"work_unit_id", workUnitID,
		"result_id", r.ID,
		"volunteer_id", volunteerID,
		"completed", existingCount+pendingDelta >= quorum,
		"validation_outcome", validationOutcome)

	resp := &lettucev1.SubmitResultResponse{
		ResultId: r.ID.String(),
		Accepted: true,
	}
	// A held (ref-only) result is accepted but not yet votable — surface that in the
	// response so the volunteer knows the URL will be fetched and verified (§10.2 step 6).
	if req.OutputDataUrl != "" {
		resp.Message = "result accepted and held pending external output content verification"
	}
	return resp, nil
}

// StartWork marks a buffered (reserved) work unit as run-started: the volunteer
// slot has begun executing a unit it pulled from its client work buffer. This is
// the relocated run-start that used to ride the FIRST RUNNING heartbeat. It flips
// the still-QUEUED reserved unit to ASSIGNED via the normal Assign (which sets
// assigned_at = now and clears the reservation columns) and writes the active
// assignment_history row in ONE transaction, starting the deadline clock at run
// time — not at buffer-fill. With per-task heartbeats removed, liveness is
// deadline-based from here on: SubmitResult / AbandonWorkUnit / the fault monitor
// all key off the active history row created here.
//
// Redundancy > 1: each distinct holder calls StartWork and gets its own history
// row, serialized by the single reservation column being freed on each Assign —
// exactly the Layer-1 run-start path, which already supports multiple holders.
//
// GRACEFUL SHEDDING: StartWork is not on the dispatch hot path (one call per unit
// actually executed, not per request), but it does touch Postgres, so it acquires
// the dispatch-cache admission semaphore and runs under a short shed-context
// timeout. If the pool is saturated it returns ResourceExhausted immediately and
// the volunteer backs off, rather than piling up "context deadline exceeded".
func (s *volunteerService) StartWork(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on.
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "StartWork"); err != nil {
		return nil, err
	}

	// GRACEFUL SHEDDING: bound StartWork's DB touch by the dispatch admission
	// semaphore and a short shed-context timeout so a saturated pool fails fast.
	if s.dispatchCache != nil {
		shedCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
		defer cancel()
		release, ok := s.dispatchCache.acquire(shedCtx)
		if !ok {
			return nil, status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry StartWork")
		}
		defer release()
		ctx = shedCtx
	}

	// Load work unit.
	wu, err := s.wuRepo.GetByID(ctx, workUnitID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "work unit not found")
		}
		s.logger.Error("failed to load work unit", "method", "StartWork", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Per-copy dispatch: a work unit stays QUEUED while its copies run, so a terminal
	// (COMPLETED/VALIDATED/REJECTED/FAILED) unit has nothing to run-start.
	if wu.State != workunit.WorkUnitStateQueued {
		return &lettucev1.StartWorkResponse{Ok: false, Message: "work unit no longer dispatchable"}, nil
	}

	// Flush race (Major 3): a copy handed out from the dispatch cache is held IN MEMORY
	// immediately, but its async copy-insert may not have landed yet. If the cache
	// holds this volunteer's reservation, force the pending flush so the RESERVED copy
	// row is durable (or voided on conflict) before the run-start.
	if s.dispatchCache != nil && s.dispatchCache.hasInMemReservation(workUnitID, volunteerID) {
		s.dispatchCache.flushPendingFor(ctx)
	}

	// Run-start: flip THIS volunteer's reserved copy to RUNNING (started_at = NOW),
	// starting the per-copy deadline clock. Idempotent (Assign uses COALESCE), so a
	// retried StartWork is a no-op success. The WORK UNIT stays QUEUED so its other
	// redundancy copies keep dispatching in parallel. 0 rows -> this volunteer holds no
	// live copy (its reservation lapsed / was voided): tell it to drop the unit.
	if _, aerr := s.wuRepo.Assign(ctx, workUnitID, volunteerID); aerr != nil {
		return &lettucev1.StartWorkResponse{Ok: false, Message: "work unit no longer reserved for this volunteer"}, nil
	}

	// Dispatch cache: the in-memory reservation hold is now a live RUNNING copy; stop
	// tracking the hold while keeping the unit dispatchable for its remaining copies.
	if s.dispatchCache != nil {
		s.dispatchCache.onRunStart(workUnitID, volunteerID)
	}

	// Best-effort liveness bookkeeping (last_seen drives the stale-volunteer monitor).
	if _, uerr := s.pool.Exec(ctx,
		"UPDATE volunteers SET last_seen_at = NOW(), is_active = true WHERE id = $1", volunteerID); uerr != nil {
		s.logger.Warn("failed to update volunteer liveness on StartWork", "volunteer_id", volunteerID, "error", uerr)
	}

	// Per-WU-lifecycle Info (one per run-start): restores per-WU visibility now that the
	// generic gRPC access log is demoted to Debug.
	s.logger.Info("run-start", "work_unit_id", workUnitID, "volunteer_id", volunteerID, "leaf_id", wu.LeafID)

	return &lettucev1.StartWorkResponse{Ok: true}, nil
}

func (s *volunteerService) SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on.
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "SaveCheckpoint"); err != nil {
		return nil, err
	}

	// Load work unit.
	wu, err := s.wuRepo.GetByID(ctx, workUnitID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "work unit not found")
		}
		s.logger.Error("failed to load work unit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Authorize against the volunteer's OPEN copy, not the single
	// work_units.assigned_volunteer_id. Dispatch is per-copy parallel (a
	// redundancy>1 unit has multiple distinct concurrent holders) and the lease
	// lifecycle rewrites assigned_volunteer_id, so a volunteer running a perfectly
	// valid copy must not be refused just because it does not hold that one slot.
	activeCopy, err := s.assignRepo.FindActiveByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
	if err != nil {
		if apiErr, ok := err.(*apierror.APIError); ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
		}
		s.logger.Error("failed to look up active assignment", "method", "SaveCheckpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if activeCopy == nil {
		return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}

	// Load leaf and check checkpointing is enabled.
	lf, err := s.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		s.logger.Error("failed to load leaf", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if !lf.FaultToleranceConfig.CheckpointingEnabled {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpointing is not enabled for this leaf")
	}

	// Validate sequence is advancing within THIS volunteer's own checkpoint chain.
	// Checkpoints are scoped per (work_unit, volunteer): each redundancy copy is an
	// independent computation with its own sequence, so two copies saving sequence 1
	// no longer collide on a single shared per-WU counter.
	volunteerLastSeq, err := s.checkpointRepo.LatestSequenceForVolunteer(ctx, workUnitID, volunteerID)
	if err != nil {
		s.logger.Error("failed to load volunteer checkpoint sequence", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if int(req.CheckpointSequence) <= volunteerLastSeq {
		return nil, status.Errorf(codes.AlreadyExists,
			"checkpoint sequence must be greater than %d", volunteerLastSeq)
	}

	// Validate data size.
	maxSize := lf.FaultToleranceConfig.MaxCheckpointSizeBytes
	if maxSize == 0 {
		maxSize = 104857600 // 100 MB default
	}
	if int64(len(req.CheckpointData)) > maxSize {
		return nil, status.Errorf(codes.ResourceExhausted,
			"checkpoint data size %d exceeds maximum %d bytes", len(req.CheckpointData), maxSize)
	}

	// Compute SHA-256 checksum.
	hash := sha256.Sum256(req.CheckpointData)
	checksum := hex.EncodeToString(hash[:])

	// Build and save checkpoint.
	cp := &checkpoint.Checkpoint{
		LeafID:             wu.LeafID,
		WorkUnitID:         workUnitID,
		VolunteerID:        volunteerID,
		CheckpointSequence: int(req.CheckpointSequence),
		SizeBytes:          int64(len(req.CheckpointData)),
		ChecksumSHA256:     checksum,
	}

	if err := s.checkpointRepo.Save(ctx, cp, req.CheckpointData); err != nil {
		s.logger.Error("failed to save checkpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	s.logger.Info("checkpoint saved",
		"work_unit_id", workUnitID,
		"volunteer_id", volunteerID,
		"sequence", req.CheckpointSequence,
		"size_bytes", len(req.CheckpointData),
	)

	return &lettucev1.SaveCheckpointResponse{
		Accepted: true,
		Message:  "checkpoint saved",
	}, nil
}

func (s *volunteerService) GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// GetCheckpointRequest carries no volunteer_id, so we resolve the caller from the
	// authenticated public key and require that they are (or were) assigned to this
	// work unit before returning checkpoint data. This prevents an authenticated
	// volunteer from reading another volunteer's in-progress checkpoint.
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	caller, err := s.volunteerRepo.GetByPublicKey(ctx, authedKey)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.PermissionDenied, "authenticated volunteer not found")
		}
		s.logger.Error("failed to look up authenticated volunteer", "method", "GetCheckpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	// Verify the caller is/was assigned to this work unit (mirrors the assignment
	// check SubmitResult uses, but accepts any assignment in history — including a
	// completed one — so a reassigned volunteer can still recover its checkpoint).
	assignments, err := s.assignRepo.ListByWorkUnit(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to list assignments", "method", "GetCheckpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	assigned := false
	for _, a := range assignments {
		if a.VolunteerID == caller.ID {
			assigned = true
			break
		}
	}
	if !assigned {
		return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}

	// Prefer the caller's OWN latest checkpoint (its own resumable chain). Only
	// when it has none do we consider another volunteer's checkpoint, and only for
	// genuinely single-copy units: handing one corroborator another's in-progress
	// state would defeat the independence that corroboration relies on.
	cp, data, err := s.checkpointRepo.GetLatestForVolunteer(ctx, workUnitID, caller.ID)
	if err != nil {
		s.logger.Error("failed to get checkpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if cp == nil {
		// A unit is safe to share a checkpoint across volunteers ONLY when it is
		// single-copy. The corroboration signal is the RESOLVED redundancy policy —
		// the same source dispatch and quorum read (transition.ResolvePolicy) — NOT
		// redundancy_factor: a leaf can require corroboration via target_copies /
		// min_quorum (or a live spot-check promotion) while redundancy_factor is still
		// 1, so reading redundancy_factor would leak one volunteer's in-progress state
		// to a distinct corroborator on the same unit. Default to corroborated (never
		// share) on any lookup failure.
		corroborated := true
		if wu, wuErr := s.wuRepo.GetByID(ctx, workUnitID); wuErr == nil {
			if lf, lfErr := s.leafRepo.GetByID(ctx, wu.LeafID); lfErr == nil && lf != nil {
				corroborated = transition.ResolvePolicy(lf, wu).TargetCopies > 1
			}
		}
		if !corroborated {
			cp, data, err = s.checkpointRepo.GetLatest(ctx, workUnitID)
			if err != nil {
				s.logger.Error("failed to get checkpoint", "error", err)
				return nil, status.Errorf(codes.Internal, "internal error")
			}
		}
	}

	if cp == nil {
		return &lettucev1.GetCheckpointResponse{
			HasCheckpoint: false,
		}, nil
	}

	return &lettucev1.GetCheckpointResponse{
		HasCheckpoint:        true,
		CheckpointData:       data,
		CheckpointSequence:   int32(cp.CheckpointSequence),
		CreatedByVolunteerId: cp.VolunteerID.String(),
		CreatedAt:            cp.CreatedAt.Format(time.RFC3339),
	}, nil
}

func (s *volunteerService) AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	if req.WorkUnitId == "" || req.VolunteerId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "work_unit_id and volunteer_id are required")
	}

	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on (FIX 2: bounded).
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "AbandonWorkUnit"); err != nil {
		return nil, err
	}

	// FIX 3 — GRACEFUL SHEDDING: bound AbandonWorkUnit's DB work by the dispatch
	// admission semaphore + a short shed ctx so an abandon storm fails fast
	// (ResourceExhausted) instead of collapsing the pool. Placed after bounded auth,
	// immediately before the first pool touch (FindActiveByWorkUnitAndVolunteer). ctx
	// is reassigned so both the buffered branch (ClearReservation + releaseInMem) and
	// the active branch (UpdateOutcome, TransitionToExpired, Reassign, onUnitDone) run
	// on the bounded shedCtx. The !ok return happens BEFORE any pool touch.
	if s.dispatchCache != nil {
		shedCtx, cancel := context.WithTimeout(ctx, dispatchDBTimeout)
		defer cancel()
		release, ok := s.dispatchCache.acquire(shedCtx)
		if !ok {
			return nil, status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry AbandonWorkUnit")
		}
		defer release()
		ctx = shedCtx
	}

	// Per-copy dispatch: abandoning a unit just closes THIS volunteer's live copy
	// (RESERVED or RUNNING) as ABANDONED. The work unit stays QUEUED and redispatches
	// a fresh copy to a distinct volunteer — no per-unit expire/reassign, no cap.
	if cerr := s.wuRepo.CloseCopyByVolunteer(ctx, workUnitID, volunteerID, string(assignment.OutcomeAbandoned), nil); cerr != nil {
		if cApiErr, ok := cerr.(*apierror.APIError); ok && cApiErr.HTTPStatus == 409 {
			// No live copy for this volunteer (already lapsed/closed): stale abandon.
			return nil, status.Errorf(codes.FailedPrecondition, "no live copy found for this volunteer and work unit")
		}
		s.logger.Error("abandon: failed to close copy", "work_unit_id", req.WorkUnitId, "error", cerr)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Dispatch cache: drop this volunteer's in-memory hold so the cache stops counting
	// it; the unit stays QUEUED and is re-stageable for a fresh distinct volunteer.
	if s.dispatchCache != nil {
		s.dispatchCache.releaseInMem(workUnitID, volunteerID)
	}

	// Delegate the post-close decision to the single transitioner (TODO #50): requeue (stay
	// QUEUED) vs dead-letter (retry ceiling exhausted). Falls back to the direct
	// DeadLetterIfExhausted when no transitioner is wired (gRPC-plumbing tests).
	requeued := true
	if s.transitioner != nil {
		if outcome, terr := s.transitioner.Evaluate(ctx, workUnitID); terr == nil && outcome == transition.OutcomeDeadLettered {
			requeued = false
		}
	} else if failed, derr := s.wuRepo.DeadLetterIfExhausted(ctx, workUnitID); derr == nil && failed {
		requeued = false
	}

	s.logger.Info("work unit copy abandoned by volunteer",
		"work_unit_id", req.WorkUnitId,
		"volunteer_id", req.VolunteerId,
		"reason", req.Reason,
		"requeued", requeued,
	)

	msg := "work unit requeued"
	if !requeued {
		msg = "work unit dead-lettered (retry ceiling exhausted)"
	}
	return &lettucev1.AbandonWorkUnitResponse{
		Requeued: requeued,
		Message:  msg,
	}, nil
}

// GetMyContribution returns the CALLER's own credit contribution, aggregated
// across every leaf and every machine the account runs. The caller is identified
// ONLY by the cryptographically verified public key set by the gRPC auth
// interceptor (GetMyContribution is NOT in grpcPublicMethods, so the interceptor
// has already verified the per-request signature); the request carries no identity
// field. So a volunteer can only ever see its own credit. Credit is keyed to the
// ACCOUNT (the Ed25519 identity key), not the host, so this already sums the
// caller's machines into one account-wide total — the self-service counterpart to
// the operator-only REST endpoint GET /api/v1/volunteers/{id}/credit/breakdown.
func (s *volunteerService) GetMyContribution(ctx context.Context, _ *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}

	vol, err := s.volunteerRepo.GetByPublicKey(ctx, []byte(authedKey))
	if err != nil {
		// An authenticated key with no volunteer row simply has no contribution
		// yet (e.g. it has not registered on this head). Report an empty, zero
		// breakdown rather than an error.
		if apiErr, ok := err.(*apierror.APIError); ok && apiErr.HTTPStatus == 404 {
			return &lettucev1.GetMyContributionResponse{}, nil
		}
		s.logger.Error("GetMyContribution: failed to resolve volunteer", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	bd, err := credit.ComputeVolunteerBreakdown(ctx, s.pool, vol.ID)
	if err != nil {
		s.logger.Error("GetMyContribution: failed to compute breakdown", "volunteer_id", vol.ID, "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	return contributionResponseFromBreakdown(bd), nil
}

// contributionResponseFromBreakdown maps the shared credit.VolunteerBreakdown into
// the GetMyContribution gRPC response.
func contributionResponseFromBreakdown(bd *credit.VolunteerBreakdown) *lettucev1.GetMyContributionResponse {
	resp := &lettucev1.GetMyContributionResponse{
		VolunteerId:    bd.VolunteerID.String(),
		TotalCredit:    bd.TotalCredit,
		ByLeaf:         make([]*lettucev1.LeafContribution, 0, len(bd.ByLeaf)),
		ByResourceType: make([]*lettucev1.ResourceTypeContribution, 0, len(bd.ByResourceType)),
		Daily:          make([]*lettucev1.DailyContribution, 0, len(bd.Timeline.Daily)),
		Weekly:         make([]*lettucev1.WeeklyContribution, 0, len(bd.Timeline.Weekly)),
	}
	for _, lc := range bd.ByLeaf {
		resp.ByLeaf = append(resp.ByLeaf, &lettucev1.LeafContribution{
			LeafId:     lc.LeafID.String(),
			LeafName:   lc.LeafName,
			Credit:     lc.Credit,
			WorkUnits:  int32(lc.WorkUnits),
			CpuSeconds: lc.CPUSeconds,
			GpuSeconds: lc.GPUSeconds,
		})
	}
	// Stable order for the resource-type split: cpu_only then gpu.
	for _, rt := range []string{"cpu_only", "gpu"} {
		v, ok := bd.ByResourceType[rt]
		if !ok {
			continue
		}
		resp.ByResourceType = append(resp.ByResourceType, &lettucev1.ResourceTypeContribution{
			ResourceType: rt,
			Credit:       v.Credit,
			WorkUnits:    int32(v.WorkUnits),
		})
	}
	for _, dc := range bd.Timeline.Daily {
		resp.Daily = append(resp.Daily, &lettucev1.DailyContribution{Date: dc.Date, Credit: dc.Credit})
	}
	for _, wc := range bd.Timeline.Weekly {
		resp.Weekly = append(resp.Weekly, &lettucev1.WeeklyContribution{WeekStart: wc.WeekStart, Credit: wc.Credit})
	}
	return resp
}

// resolveAuthedVolunteer verifies that the request was authenticated and that the
// cryptographically proven public key (set by the gRPC auth interceptor) matches the
// public key on record for the volunteer identified by volunteerID. This binds the
// proven identity to the volunteer being acted on.
//
// FIX 2: it resolves the volunteer pubkey from the admission-bounded, shed-aware
// identity snapshot (dispatchCache.resolveIdentity) instead of an UNBOUNDED
// s.volunteerRepo.GetByID on the request ctx. In steady state (RegisterVolunteer
// pre-warms the snapshot) this touches ZERO DB; only a cold miss reads Postgres, and
// that read is bounded by the admission semaphore + a short shed timeout, so under a
// write storm the write path SHEDS (ResourceExhausted) instead of saturating the DB
// pool with "context deadline exceeded".
//
// Codes: Unauthenticated (no proven key), PermissionDenied (key mismatch), NotFound
// (unknown volunteer) are preserved. NEW on a cold miss, intentionally matching the
// RequestWorkUnit precedent: admission saturation OR a transient (non-404) DB error
// both return ResourceExhausted (the transient case was previously Internal). This is
// the whole point — the client backs off and the collapse vector is removed; do NOT
// "fix" the transient case back to Internal.
//
// This resolution must run BEFORE / OUTSIDE any per-handler shed gate (resolveIdentity
// acquires its OWN admission slot internally on a cold miss; a handler already holding
// a slot could otherwise double-acquire and self-deadlock at a small admissionCap).
// method is used only for logging.
func (s *volunteerService) resolveAuthedVolunteer(ctx context.Context, volunteerID types.ID, method string) error {
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	var pub []byte
	if s.dispatchCache != nil {
		ident, notFound, shed := s.dispatchCache.resolveIdentity(volunteerID)
		if shed {
			return status.Errorf(codes.ResourceExhausted, "dispatch overloaded; back off and retry")
		}
		if notFound {
			return status.Errorf(codes.NotFound, "volunteer not found")
		}
		pub = ident.publicKey
	} else {
		vol, err := s.volunteerRepo.GetByID(ctx, volunteerID)
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				return status.Errorf(codes.NotFound, "volunteer not found")
			}
			s.logger.Error("failed to look up volunteer", "method", method, "error", err)
			return status.Errorf(codes.Internal, "internal error")
		}
		pub = vol.PublicKey
	}
	if !bytes.Equal(pub, authedKey) {
		return status.Errorf(codes.PermissionDenied, "authenticated key does not match volunteer record")
	}
	return nil
}

// requireAuthedVolunteer is a thin alias for resolveAuthedVolunteer (FIX 2), kept so
// the StartWork / SaveCheckpoint / AbandonWorkUnit call sites are unchanged.
func (s *volunteerService) requireAuthedVolunteer(ctx context.Context, volunteerID types.ID, method string) error {
	return s.resolveAuthedVolunteer(ctx, volunteerID, method)
}

// derefString returns the dereferenced string or empty string if nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
