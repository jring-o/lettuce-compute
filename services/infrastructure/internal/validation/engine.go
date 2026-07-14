package validation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Outcome constants for ValidationResult.
const (
	OutcomeValidated = "VALIDATED"
	OutcomeRejected  = "REJECTED"
	OutcomePending   = "PENDING"
)

// ValidationResult describes the outcome of a validation attempt.
type ValidationResult struct {
	WorkUnitID      types.ID
	Outcome         string
	AgreedResults   []types.ID
	RejectedResults []types.ID
	CreditEntries   []*credit.LedgerEntry
}

// runnerSubjectsProvider is the narrow, consumer-side view of the trusted-runner registry the
// engine uses to GATE the D9 accrual-witness upgrade (§7.6). Declared here (not imported from
// the audit package) so the engine depends on one method; audit.RunnersRepository satisfies it.
type runnerSubjectsProvider interface {
	// ActiveRunnerSubjects returns the current trust subjects of all ACTIVE trusted runners; an
	// empty slice means the registry has no active runner (accrual then stays on the legacy rule).
	ActiveRunnerSubjects(ctx context.Context) ([]string, error)
}

// artifactVersionResolver resolves a pinned artifact version's frozen ExecutionConfig for the
// sampling hook, so a sampled audit records the exact exec config the winner RAN (audit F-M4:
// leafs.execution_config is owner-mutable mid-window). Matched by leaf.PgxRepository.
type artifactVersionResolver interface {
	GetVersionByID(ctx context.Context, id types.ID) (*leaf.ArtifactVersion, error)
}

// Engine performs result validation by comparing redundant results,
// running quorum selection, granting credit, and tracking volunteer rejection rates.
type Engine struct {
	resultRepo      result.Repository
	workUnitRepo    workunit.WorkUnitRepository
	leafRepo        leaf.Repository
	creditRepo      credit.Repository
	racRepo         credit.RACRepository
	volunteerRepo   volunteer.Repository
	assignmentRepo  assignment.Repository
	attestationRepo attestation.Creator
	// reliabilityRepo feeds the per-host measured-reliability signal (TODO #54): an AGREED
	// result is a good outcome for the host that produced it, a DISAGREED result a bad one.
	// May be nil (tests / pre-#54) -> the signal is simply not recorded (best-effort).
	reliabilityRepo reliability.Repository
	// trustRepo accrues account-level trust for corroborated-clean work (see internal/trust):
	// after a unit validates, each distinct agreed subject earns +1 IFF it was corroborated by
	// a DISTINCT already-trusted subject. May be nil (tests / feature off) -> no accrual.
	trustRepo trust.Repository
	// trustPolicy resolves the effective trust floor (and gate K) per leaf. Its zero value is
	// the gate off; the floor is still resolved so accrual works before enforcement is enabled.
	trustPolicy transition.TrustPolicy
	// standingRecorder is the automatic rejection-rate backpressure machine (BG-24/BG-24b
	// PR-B): every adjudicated result (AGREED and DISAGREED) folds into the volunteer's
	// decayed rejection-rate signal, which drives AUTO-owned standing transitions with
	// hysteresis. Nil (the default — LETTUCE_HEAD_STANDING_BACKPRESSURE_ENABLED is false)
	// keeps today's behavior exactly: the lifetime-rate WARN for rejected results, no
	// signal recorded. Set via WithStandingBackpressure; consumed best-effort.
	standingRecorder standing.Recorder
	// emissionCapPerDay is the per-account rolling-24h credit ceiling enforced at the grant
	// choke point in acceptResults (design §5.3). <= 0 (the default, and the zero value) means
	// no cap: the grant loop stays on the byte-for-byte legacy Create path. When positive, an
	// agreed result whose account would exceed the cap is SUPPRESSED (no ledger row, no RAC, a
	// credit-0 attestation) while still counting as corroborated work. Set via WithEmissionCap.
	emissionCapPerDay float64
	// capWarnOnce fires the misconfiguration WARN at most once per engine lifetime, when a cap is
	// configured but the credit repository does not implement credit.CappedCreator (the grant
	// then falls back to uncapped Create — loud, but never a validation failure).
	capWarnOnce sync.Once
	// runnerSubjects GATES the D9 trust-accrual witness upgrade (§7.6/F-H1): nil (the default)
	// keeps the legacy single-trusted-witness rule so accrual is byte-identical to today until a
	// trusted runner is actually registered. Set via WithTrustedRunners.
	runnerSubjects runnerSubjectsProvider
	// auditEnq/auditEnabled/auditHeadRate/auditVersions configure the post-validation result-audit
	// sampling hook (§7.2). auditEnabled false or auditEnq nil (the default) disables sampling
	// entirely — no audit rows, no eligibility work. Set via WithResultAudits.
	auditEnq      audit.Enqueuer
	auditEnabled  bool
	auditHeadRate float64
	auditVersions artifactVersionResolver
	// auditIneligible counts validated-but-ineligible units per leaf id (string) for the
	// fault-monitor probe (§7.2 skip-lane visibility). Guarded by auditIneligibleMu; lazily
	// initialized on first ineligible unit. Read via AuditIneligibleCounts.
	auditIneligibleMu sync.Mutex
	auditIneligible   map[string]int64
	// repairClaimer guards the non-idempotent slice-3 repair effects (one repair per
	// result, ever — design doc §9.6). nil disables RepairUnit (it errors). Set via
	// WithRepairSupport.
	repairClaimer RepairClaimer
	// txRunner runs the money-bearing tx phase of accept/reject (marks + state flip +
	// credit/requeue) inside ONE serialized transaction so partial finalization is
	// unrepresentable (design §4.1, invariant E1-S). nil (the default) is the passthrough:
	// the closure runs over the engine's own pool-backed repos with no transaction, no
	// unit-row lock, and no stale-snapshot recheck — which keeps every mock-based engine test
	// working unchanged. Set via WithTxRunner (production wires NewPgxFinalizationTxRunner).
	txRunner FinalizationTxRunner
	signer   *attestation.Signer
	logger   *slog.Logger
}

// NewEngine creates a new validation Engine.
func NewEngine(
	resultRepo result.Repository,
	workUnitRepo workunit.WorkUnitRepository,
	leafRepo leaf.Repository,
	creditRepo credit.Repository,
	racRepo credit.RACRepository,
	volunteerRepo volunteer.Repository,
	assignmentRepo assignment.Repository,
	attestationRepo attestation.Creator,
	reliabilityRepo reliability.Repository,
	signer *attestation.Signer,
	logger *slog.Logger,
	trustRepo trust.Repository,
	trustPolicy transition.TrustPolicy,
) *Engine {
	return &Engine{
		resultRepo:      resultRepo,
		workUnitRepo:    workUnitRepo,
		leafRepo:        leafRepo,
		creditRepo:      creditRepo,
		racRepo:         racRepo,
		volunteerRepo:   volunteerRepo,
		assignmentRepo:  assignmentRepo,
		attestationRepo: attestationRepo,
		reliabilityRepo: reliabilityRepo,
		trustRepo:       trustRepo,
		trustPolicy:     trustPolicy,
		signer:          signer,
		logger:          logger,
	}
}

// WithStandingBackpressure sets the automatic rejection-rate backpressure recorder,
// returning the engine for chaining (the workunit.WithTrustDispatch pattern —
// avoiding another positional NewEngine parameter). Callers that skip it (or pass
// nil) keep the nil recorder: no signal is recorded and the legacy lifetime-rate
// WARN behavior is preserved byte-for-byte.
func (e *Engine) WithStandingBackpressure(rec standing.Recorder) *Engine {
	e.standingRecorder = rec
	return e
}

// WithEmissionCap sets the per-account daily credit emission cap enforced at the grant choke
// point, returning the engine for chaining (the WithStandingBackpressure pattern — avoiding
// another positional NewEngine parameter). A capPerDay <= 0 means NO cap: callers that skip it
// (or pass a non-positive value) keep today's grant path byte-for-byte — the zero-value field
// is the default — so no credit_ledger row is ever suppressed and CreateCapped is never called.
func (e *Engine) WithEmissionCap(capPerDay float64) *Engine {
	e.emissionCapPerDay = capPerDay
	return e
}

// WithTrustedRunners wires the trusted-runner registry provider that GATES the D9 accrual-witness
// upgrade (§7.6/F-H1), returning the engine for chaining. A nil provider (the default) keeps the
// legacy single-trusted-witness accrual rule, so newcomer accrual is byte-identical to today
// until the operator registers a runner — the G2-preserving gate that stops an un-gated rule
// change from freezing accrual on a head whose registry is (necessarily) empty on deploy day.
func (e *Engine) WithTrustedRunners(p runnerSubjectsProvider) *Engine {
	e.runnerSubjects = p
	return e
}

// WithResultAudits configures the post-validation result-audit sampling hook (§7.2), returning
// the engine for chaining. enabled false or enq nil (the default) disables sampling entirely.
// headRate is the head-default fraction the per-leaf audit_rate can only RAISE (F-H4); versions
// resolves a versioned winner's frozen ExecutionConfig snapshot (F-M4).
func (e *Engine) WithResultAudits(enq audit.Enqueuer, enabled bool, headRate float64, versions artifactVersionResolver) *Engine {
	e.auditEnq = enq
	e.auditEnabled = enabled
	e.auditHeadRate = headRate
	e.auditVersions = versions
	return e
}

// WithTxRunner wires the finalization transaction runner that makes accept/reject atomic
// (design §4.1), returning the engine for chaining (the WithStandingBackpressure pattern). A
// nil runner (the default — callers that skip it) keeps the passthrough: the accept/reject tx
// phase runs over the engine's own repos with no transaction, no unit-row lock, and no
// stale-snapshot recheck, so mock-based tests are byte-for-byte unchanged. Production wires
// NewPgxFinalizationTxRunner(pool) so the marks, the VALIDATED/REJECTED flip, the ledger rows
// (and, on reject, the requeue) commit or roll back together.
func (e *Engine) WithTxRunner(r FinalizationTxRunner) *Engine {
	e.txRunner = r
	return e
}

// HasTxRunner reports whether a finalization transaction runner is wired — i.e. whether
// accept/reject runs atomically rather than through the mock-friendly passthrough. The
// production transitioner constructor (server.NewVolunteerService) refuses a pool-backed
// engine without one, because a deployed head on the passthrough is exactly ★BG-21e: every
// atomicity guarantee silently absent while all the mechanism's own tests stay green.
func (e *Engine) HasTxRunner() bool {
	return e.txRunner != nil
}

// TryValidate checks if enough results have arrived for a work unit
// and runs the comparison algorithm if so.
// Returns nil if the work unit is not ready for validation or is already validated.
func (e *Engine) TryValidate(ctx context.Context, workUnitID types.ID) (*ValidationResult, error) {
	wu, err := e.workUnitRepo.GetByID(ctx, workUnitID)
	if err != nil {
		return nil, fmt.Errorf("load work unit: %w", err)
	}

	// Only validate work units in COMPLETED state.
	if wu.State != workunit.WorkUnitStateCompleted {
		return nil, nil
	}

	proj, err := e.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		return nil, fmt.Errorf("load leaf: %w", err)
	}

	results, err := e.resultRepo.ListByWorkUnit(ctx, workUnitID)
	if err != nil {
		return nil, fmt.Errorf("list results: %w", err)
	}

	// Filter to only PENDING results.
	var pending []*result.Result
	for _, r := range results {
		if r.ValidationStatus == result.ValidationPending {
			pending = append(pending, r)
		}
	}

	// The RAW pending count (BEFORE the version-homogeneity filter) anchors the finalization
	// transaction's stale-snapshot recheck, exactly as the transitioner threads it on the
	// production path (transitioner.go). The legacy path is tests-only, but it must pass the
	// TRUE raw count — not the post-filter len(pending) — so the recheck arithmetic is honest.
	rawPendingCount := len(pending)

	// Version-homogeneous validation (TODO #38, interacts with #12): never compare
	// results produced by DIFFERENT artifact versions — a version difference is not a
	// disagreement. Homogeneous-redundancy pinning means all replicas of a unit run one
	// version, so this normally leaves `pending` unchanged; it is the defensive guard
	// against a cross-version straggler (e.g. a result from before the leaf was
	// versioned). The redundancy gate below then applies to the single-version group.
	pending = versionHomogeneousGroup(pending)

	// Determine effective redundancy: spot-check WUs always require 2 results.
	effectiveRedundancy := proj.ValidationConfig.RedundancyFactor
	if wu.SpotCheck {
		effectiveRedundancy = 2
	}

	if len(pending) < effectiveRedundancy {
		// H-1: the #1 reason a work unit appears "stuck at PENDING" — not enough
		// corroborating results have arrived yet for the configured redundancy.
		e.logger.Debug("validation deferred: insufficient results",
			"work_unit_id", workUnitID, "pending", len(pending), "required", effectiveRedundancy)
		return nil, nil
	}

	cfg := proj.ValidationConfig
	switch cfg.ComparisonMode {
	case leaf.ComparisonExact:
		return e.validateExact(ctx, wu, proj, pending, rawPendingCount)
	case leaf.ComparisonNumericTolerance:
		return e.validateNumericTolerance(ctx, wu, proj, pending, rawPendingCount)
	case leaf.ComparisonCustom:
		return nil, fmt.Errorf("custom comparison mode is not implemented in Alpha")
	default:
		return nil, fmt.Errorf("unknown comparison mode: %s", cfg.ComparisonMode)
	}
}

// --- Transitioner-facing API (TODO #50) ---
//
// These expose the comparator and the accept/reject EFFECTS to internal/transition, which owns
// the decision (when to validate / reject / wait / dead-letter). The engine no longer decides
// the outcome on its own — TryValidate remains only for legacy/test callers; every live submit
// path routes through the transitioner: the gRPC SubmitResult, the browser/WASM REST submit
// (handleBrowserSubmitResult, TODO #66), and the fault monitor's post-copy-close re-evaluation.

// FilterPending returns the version-homogeneous subset of pending results (never compare across
// artifact versions). The transitioner calls this before counting + comparing so its quorum
// gate matches the legacy TryValidate gate.
func (e *Engine) FilterPending(pending []*result.Result) []*result.Result {
	return versionHomogeneousGroup(pending)
}

// Compare runs the leaf's comparator (READ-ONLY) over the pending results and returns the
// largest agreeing group. No state is written — the transitioner decides the outcome from the
// group + the resolved RedundancyPolicy. CUSTOM remains the Alpha stub (#47 out of scope).
func (e *Engine) Compare(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result) ([]*result.Result, error) {
	switch proj.ValidationConfig.ComparisonMode {
	case leaf.ComparisonExact:
		return e.compareExact(proj, pending)
	case leaf.ComparisonNumericTolerance:
		return e.compareNumericTolerance(proj, pending)
	case leaf.ComparisonCustom:
		return nil, fmt.Errorf("custom comparison mode is not implemented in Alpha")
	default:
		return nil, fmt.Errorf("unknown comparison mode: %s", proj.ValidationConfig.ComparisonMode)
	}
}

// ApplyAccept performs the validate effects for a unit whose results reached quorum agreement:
// mark AGREED/DISAGREED, transition COMPLETED -> VALIDATED, grant credit/RAC, sign
// attestations, update counters + reliability. The unit must already be COMPLETED (the
// transitioner marks it so first). The engine half of the transitioner's ActionValidate.
func (e *Engine) ApplyAccept(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending, majority []*result.Result, verdict *transition.ComparisonVerdict, policy transition.RedundancyPolicy, rawPendingCount int) error {
	_, err := e.acceptResults(ctx, wu, proj, pending, majority, verdict, policy, rawPendingCount)
	return err
}

// ApplyReject performs the reject effects: mark all pending DISAGREED, transition
// COMPLETED -> REJECTED, attest, and requeue (Reassign). The unit must already be COMPLETED.
// The engine half of the transitioner's ActionReject.
func (e *Engine) ApplyReject(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, verdict *transition.ComparisonVerdict, policy transition.RedundancyPolicy, rawPendingCount int) error {
	_, err := e.rejectAll(ctx, wu, proj, pending, verdict, policy, rawPendingCount)
	return err
}

// versionHomogeneousGroup returns the largest subset of pending results that all share
// one artifact version (nil/legacy is its own group), so validation never compares
// across artifact versions. Ties break deterministically by version key. With
// homogeneous-redundancy pinning there is normally a single group, so the input is
// returned unchanged.
func versionHomogeneousGroup(pending []*result.Result) []*result.Result {
	if len(pending) < 2 {
		return pending
	}
	groups := make(map[string][]*result.Result)
	for _, r := range pending {
		key := ""
		if r.ArtifactVersionID != nil {
			key = r.ArtifactVersionID.String()
		}
		groups[key] = append(groups[key], r)
	}
	if len(groups) == 1 {
		return pending
	}
	var best []*result.Result
	var bestKey string
	for k, g := range groups {
		if len(g) > len(best) || (len(g) == len(best) && (best == nil || k < bestKey)) {
			best = g
			bestKey = k
		}
	}
	return best
}

// validateExact groups results by output checksum and applies quorum selection.
func (e *Engine) validateExact(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, rawPendingCount int) (*ValidationResult, error) {
	majorityGroup, err := e.compareExact(proj, pending)
	if err != nil {
		return nil, err
	}
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup, rawPendingCount)
}

// compareExact is the read-only EXACT comparator: it returns the largest agreeing group of
// results WITHOUT writing any state. Shared by validateExact (legacy TryValidate path) and the
// transitioner (which decides the outcome from the group via transition.Decide).
//
// When the leaf declares ignore_fields, the grouping key is recomputed canonically from the
// stored output (volatile fields stripped + object keys sorted) so that a wall-clock field
// like compute_time_ms no longer prevents agreement; otherwise the raw submitted checksum is
// used (historical behavior, unchanged).
func (e *Engine) compareExact(proj *leaf.Leaf, pending []*result.Result) ([]*result.Result, error) {
	ignoreFields := proj.ValidationConfig.IgnoreFields

	// Group results by (canonical) checksum. §4.3: the comparator is TOTAL over content — a
	// result whose key cannot be computed (empty / malformed / unreadable output) no longer
	// aborts the whole comparison (BG-21a). It is EXCLUDED from all groups, so it can never
	// form or join a majority (a unique singleton key would otherwise let garbage validate on
	// a quorum-1 leaf). It stays in `pending`, so it lands DISAGREED at accept and counts
	// toward the verdict Total — one bad input degraded to one bad vote, never a stalled unit.
	groups := make(map[string][]*result.Result)
	for _, r := range pending {
		key, err := comparisonKey(r, ignoreFields)
		if err != nil {
			e.logger.Warn("exact comparison: excluding result with unreadable output from grouping",
				"work_unit_id", r.WorkUnitID, "result_id", r.ID, "error", err)
			continue
		}
		groups[key] = append(groups[key], r)
	}

	// Find the largest group (majority). A tie for the largest size means there is no unique
	// majority — two or more distinct content groups are equally supported — so no group can
	// be trusted as the winner. Return an empty group so the caller treats a tie as "no
	// agreement" (flowing into its extend-copies-or-reject path). A deterministic content
	// tie-break would be grindable: an attacker could shape a checksum to win a tie. A genuine
	// largest group still wins outright.
	var majorityChecksum string
	var majorityCount int
	tie := false
	for checksum, group := range groups {
		switch {
		case len(group) > majorityCount:
			majorityCount = len(group)
			majorityChecksum = checksum
			tie = false
		case len(group) == majorityCount:
			tie = true
		}
	}
	if tie || majorityCount == 0 {
		return nil, nil
	}
	return groups[majorityChecksum], nil
}

// validateNumericTolerance compares numeric output data within epsilon tolerance.
func (e *Engine) validateNumericTolerance(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, rawPendingCount int) (*ValidationResult, error) {
	majorityGroup, err := e.compareNumericTolerance(proj, pending)
	if err != nil {
		return nil, err
	}
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup, rawPendingCount)
}

// compareNumericTolerance is the read-only NUMERIC_TOLERANCE comparator: it returns the
// largest mutually-compatible clique of results WITHOUT writing state. Shared by
// validateNumericTolerance (legacy path) and the transitioner.
func (e *Engine) compareNumericTolerance(proj *leaf.Leaf, pending []*result.Result) ([]*result.Result, error) {
	epsilon := float64(0)
	if proj.ValidationConfig.NumericTolerance != nil {
		epsilon = *proj.ValidationConfig.NumericTolerance
	}
	ignoreFields := proj.ValidationConfig.IgnoreFields
	compareFields := proj.ValidationConfig.CompareFields

	// Flatten each result's output into a path -> value map. Nested objects/arrays flatten to
	// dotted/indexed paths; numeric leaves compare within epsilon and non-numeric leaves for
	// equality. ignore_fields are dropped; if compare_fields is non-empty only matching paths
	// are kept (so a chaotic sim can be validated on its aggregate science while its raw
	// per-fight trajectory is excluded).
	//
	// §4.3: the comparator is TOTAL over content. A result whose output cannot be flattened
	// (empty / malformed / non-finite) no longer aborts the whole comparison (BG-21a); it is
	// EXCLUDED from clique candidacy entirely — never in the compatibility matrix, so it can
	// neither join nor form the returned group (a lone excluded row must not validate on a
	// quorum-1 leaf). It stays in `pending`, lands DISAGREED at accept, and counts toward the
	// verdict Total. The clique search proceeds over the flattenable rest.
	var candidates []*result.Result
	var parsed []map[string]flatVal
	for _, r := range pending {
		m, err := flattenOutput(r.OutputData, ignoreFields, compareFields)
		if err != nil {
			e.logger.Warn("numeric comparison: excluding result with unreadable output from grouping",
				"work_unit_id", r.WorkUnitID, "result_id", r.ID, "error", err)
			continue
		}
		candidates = append(candidates, r)
		parsed = append(parsed, m)
	}

	// Build compatibility matrix over the flattenable candidates only.
	n := len(candidates)
	compatible := make([][]bool, n)
	for i := range compatible {
		compatible[i] = make([]bool, n)
		compatible[i][i] = true
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if numericMatch(parsed[i], parsed[j], epsilon) {
				compatible[i][j] = true
				compatible[j][i] = true
			}
		}
	}

	// Find the largest clique (all mutually compatible results).
	clique := findLargestClique(n, compatible)

	// Build majority group from clique indices (mapped back to the candidate results).
	majorityGroup := make([]*result.Result, len(clique))
	for i, idx := range clique {
		majorityGroup[i] = candidates[idx]
	}

	return majorityGroup, nil
}

// applyThreshold applies the agreement gates and performs the validation outcome.
//
// This is the LEGACY path (TryValidate), retained for tests only — every production submit
// routes through internal/transition. It is deliberately kept in lockstep with transition.Decide
// (per the PR #80 discipline): it builds the SAME subject-level verdict and applies the SAME
// four gates the production decider does, so the two can never disagree about what validates.
func (e *Engine) applyThreshold(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result, rawPendingCount int) (*ValidationResult, error) {
	threshold := proj.ValidationConfig.AgreementThreshold

	// min_quorum resolves as in transition.ResolvePolicy (spot-check forces a 2-of-2
	// corroboration); the trust gate K and floor resolve as in transition.ResolvePolicyWithTrust.
	quorum := proj.ValidationConfig.EffectiveMinQuorum()
	if wu.SpotCheck {
		quorum = 2
	}
	k, floor := e.trustPolicy.ResolveTrust(proj.ValidationConfig, quorum)

	// Build the verdict in DISTINCT SUBJECTS (copies from one principal corroborate as one; a
	// self-contradicting principal corroborates as none) so this legacy path counts exactly as
	// the transitioner does. Behavior-preserving for the existing tests, whose results are all
	// unstamped with distinct volunteers (subject counts == result counts) under a zero-value
	// TrustPolicy (K == 0, so the trust gate is a vacuous auto-pass).
	v := transition.BuildComparisonVerdict(pending, majorityGroup, floor)

	// A unit VALIDATES only if all FOUR gates hold, mirroring transition.Decide:
	//   (1) v.Ratio >= threshold                      — the configured agreement fraction, and
	//   (2) v.MajorityCount >= quorum                 — the agreeing group is itself quorum-sized
	//                                                   (the floor is on the WINNERS), and
	//   (3) 2*v.MajorityCount > v.Total               — a STRICT majority of the compared set, so
	//                                                   no config validates a minority/plurality, and
	//   (4) v.TrustedMajorityCount >= k               — enough DISTINCT, TRUSTED subjects (0 when
	//                                                   the gate is off: a vacuous auto-pass).
	// A tie leaves MajorityCount == 0, which fails (2) and (3), so an ambiguous largest group
	// can never validate.
	// The resolved policy for the attestation quorum descriptor — the same pure resolution
	// the transitioner performs (its quorum/gate numbers match the k/floor/quorum above by
	// construction; both derive from ResolveTrust + the leaf config under the same lock).
	policy := transition.ResolvePolicyWithTrust(proj, wu, e.trustPolicy)

	if v.MajorityCount >= quorum && 2*v.MajorityCount > v.Total && v.Ratio >= threshold && v.TrustedMajorityCount >= k {
		return e.acceptResults(ctx, wu, proj, pending, majorityGroup, v, policy, rawPendingCount)
	}

	// Agreement not reached (ratio, floor, or strict-majority gate failed). Check if there
	// are still active assignments.
	activeCount, err := e.assignmentRepo.CountActiveByWorkUnit(ctx, wu.ID)
	if err != nil {
		return nil, fmt.Errorf("count active assignments: %w", err)
	}

	if activeCount > 0 {
		// H-3: the majority did not reach the agreement threshold, but copies are still
		// running — defer rather than reject, and say so (otherwise this PENDING hold is
		// silent).
		e.logger.Debug("validation pending: threshold unmet, assignments active",
			"work_unit_id", wu.ID,
			"majority", v.MajorityCount,
			"total", v.Total,
			"trusted_majority", v.TrustedMajorityCount,
			"threshold", threshold,
			"active", activeCount)
		return &ValidationResult{
			WorkUnitID: wu.ID,
			Outcome:    OutcomePending,
		}, nil
	}

	// All assignments completed, no agreement. Reject all.
	return e.rejectAll(ctx, wu, proj, pending, v, policy, rawPendingCount)
}

// acceptResults marks majority results as AGREED, minority as DISAGREED,
// transitions the work unit, grants credit, and updates volunteer counters. verdict and
// policy describe the quorum event the decision was gated on; they feed the signed v2
// attestation quorum descriptor (both are non-nil/resolved on every production path — the
// transitioner threads its own, and the legacy applyThreshold path self-resolves identical
// values from the same pure functions).
func (e *Engine) acceptResults(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result, verdict *transition.ComparisonVerdict, policy transition.RedundancyPolicy, rawPendingCount int) (*ValidationResult, error) {
	majorityIDs := make(map[types.ID]bool)
	for _, r := range majorityGroup {
		majorityIDs[r.ID] = true
	}

	var agreedIDs, rejectedIDs []types.ID
	var agreedResults, rejectedResults []*result.Result
	for _, r := range pending {
		if majorityIDs[r.ID] {
			agreedIDs = append(agreedIDs, r.ID)
			agreedResults = append(agreedResults, r)
		} else {
			rejectedIDs = append(rejectedIDs, r.ID)
			rejectedResults = append(rejectedResults, r)
		}
	}

	// Credit amount for each agreed result (the leaf's configured amount, floored at 1.0).
	creditAmount := proj.CreditConfig.CreditPerValidatedWorkUnit
	if creditAmount <= 0 {
		creditAmount = 1.0
	}

	// attestedAmounts carries the credit ACTUALLY granted per result into createAttestations: a
	// granted result attests the leaf amount; a result whose grant the emission cap suppressed is
	// ABSENT (resolving to 0). DISAGREED results are never keyed here, so they attest 0 as before.
	// creditEntries are the rows actually inserted (drives post-commit RAC + the returned value);
	// suppressedResults are the cap-suppressed AGREED rows whose WARN fires post-commit.
	attestedAmounts := make(map[types.ID]float64, len(agreedResults))
	var creditEntries []*credit.LedgerEntry
	var suppressedResults []*result.Result

	// TX PHASE (design §4.1): mark AGREED, mark DISAGREED, guarded flip COMPLETED->VALIDATED,
	// and the per-result credit writes all commit in ONE serialized transaction (production),
	// so partial finalization is unrepresentable — invariant E1-S. On any error the whole tx
	// rolls back: the unit stays COMPLETED with its results PENDING and the recovery sweep
	// re-drives it (the ★E1-1 marks-only strand is unrepresentable). The passthrough default
	// (no WithTxRunner) runs this over the engine's own repos untxed, so mock tests are unchanged.
	if err := e.runFinalization(ctx, wu.ID, rawPendingCount, func(stores FinalizationStores) error {
		if err := stores.Results.BatchUpdateValidationStatus(ctx, agreedIDs, result.ValidationAgreed); err != nil {
			return fmt.Errorf("mark results AGREED: %w", err)
		}
		if len(rejectedIDs) > 0 {
			if err := stores.Results.BatchUpdateValidationStatus(ctx, rejectedIDs, result.ValidationDisagreed); err != nil {
				return fmt.Errorf("mark results DISAGREED: %w", err)
			}
		}
		if _, err := stores.WorkUnits.UpdateState(ctx, wu.ID, workunit.WorkUnitStateCompleted, workunit.WorkUnitStateValidated); err != nil {
			return fmt.Errorf("transition work unit to VALIDATED: %w", err)
		}

		// The CreateCapped capability is resolved against the TX-SCOPED credits repo so the
		// rolling-24h SUM reads this transaction's own earlier inserts (design §4.1). cappedCreatorFor
		// returns (nil, false) both when no cap is set — the default, keeping the byte-for-byte legacy
		// Create path — and when a cap is set but the repo lacks the capability (WARNs once, falls back).
		cc, capEnforced := e.cappedCreatorFor(stores.Credits)
		for _, r := range agreedResults {
			entry := &credit.LedgerEntry{
				VolunteerID:  r.VolunteerID,
				LeafID:       wu.LeafID,
				WorkUnitID:   wu.ID,
				ResultID:     r.ID,
				CreditAmount: creditAmount,
			}
			if capEnforced {
				inserted, err := cc.CreateCapped(ctx, entry, e.emissionCapPerDay)
				if err != nil {
					return fmt.Errorf("create credit entry for result %s: %w", r.ID, err)
				}
				if !inserted {
					// Suppression branch (design §5.3, audit F3/F10): the account's rolling-24h
					// grants plus this amount would exceed the cap. Grant NOTHING — no ledger row,
					// no RAC upsert, no attested credit — but leave the result AGREED so every
					// work-quality effect (counters, standing, reliability, trust) still fires. The
					// cap bounds emission, not merit. The WARN moves post-commit (§4.1).
					suppressedResults = append(suppressedResults, r)
					continue
				}
			} else if err := stores.Credits.Create(ctx, entry); err != nil {
				return fmt.Errorf("create credit entry for result %s: %w", r.ID, err)
			}
			creditEntries = append(creditEntries, entry)
			attestedAmounts[r.ID] = creditAmount
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// --- POST-COMMIT best-effort effects (design §4.1): unchanged semantics + order, run over
	// the engine's own pool-backed repos only AFTER the money tx has committed. A failure here
	// never rolls back the grant — credit is already durable. ---

	// RAC upserts move here from the credit loop, ONLY for the entries actually granted. H-7:
	// best-effort — a failure does not fail validation (credit is already granted), so WARN.
	if e.racRepo != nil {
		for _, entry := range creditEntries {
			if err := e.racRepo.Upsert(ctx, entry.VolunteerID, wu.LeafID, entry.CreditAmount); err != nil {
				e.logger.Warn("failed to update RAC",
					"volunteer_id", entry.VolunteerID, "leaf_id", wu.LeafID, "result_id", entry.ResultID, "error", err)
			}
		}
	}

	// Relocated emission-cap suppression WARNs (were inside the tx phase — moved out so a rolled-
	// back tx does not emit a misleading "suppressed" line for a grant that never happened).
	for _, r := range suppressedResults {
		e.logger.Warn("credit suppressed by daily emission cap",
			"volunteer_id", r.VolunteerID,
			"work_unit_id", wu.ID,
			"result_id", r.ID,
			"amount", creditAmount,
			"cap", e.emissionCapPerDay)
	}

	// Create attestations for agreed results. The amount attested is the credit actually granted
	// (attestedAmounts): a cap-suppressed AGREED result is absent from the map and attests 0.
	// AGREED and DISAGREED members of the unit share the unit's quorum descriptor — both attest
	// the same quorum event.
	desc := e.buildQuorumDescriptor(proj, verdict, policy)
	e.createAttestations(ctx, wu, agreedResults, attestation.OutcomeAgreed, attestedAmounts, desc)

	// Create attestations for disagreed results (credit_amount = 0 via the nil map).
	e.createAttestations(ctx, wu, rejectedResults, attestation.OutcomeDisagreed, nil, desc)

	// Update volunteer counters. H-7: best-effort counter bumps — a failure does not
	// fail validation, so log at Warn, not Error.
	for _, r := range agreedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsCompleted(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units completed",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
		e.recordAdjudicated(ctx, r.VolunteerID, true)
	}
	for _, r := range rejectedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
		e.recordAdjudicated(ctx, r.VolunteerID, false)
	}

	// TODO #54: feed the per-host reliability signal — an AGREED result is a good outcome
	// for the machine that produced it (grows its buffer), a DISAGREED one is wasted work
	// (shrinks it). Best-effort, after credit is already granted (never fails validation).
	e.recordReliability(ctx, agreedResults, true)
	e.recordReliability(ctx, rejectedResults, false)

	// Account-level trust accrual (see internal/trust): reward the DISTINCT subjects behind a
	// corroborated-clean unit, but ONLY when the agreement was witnessed by an already-trusted
	// subject. Best-effort, after credit is already granted (never fails validation); nil-safe.
	e.accrueTrust(ctx, proj, wu, agreedResults)

	// H-2: a successful VALIDATED + credit-grant was previously silent — restore the
	// per-WU "this validated" signal now that the generic access log is demoted.
	e.logger.Info("work unit validated",
		"work_unit_id", wu.ID,
		"agreed", len(agreedIDs),
		"disagreed", len(rejectedIDs),
		"credit_amount", creditAmount)

	// Post-validation result-audit sampling (§7.2): the unit is fully validated + credited, so
	// selection is post-hoc and carries zero dispatch signal. Best-effort, never fails validation.
	e.maybeSampleForAudit(ctx, wu, proj, agreedResults)

	return &ValidationResult{
		WorkUnitID:      wu.ID,
		Outcome:         OutcomeValidated,
		AgreedResults:   agreedIDs,
		RejectedResults: rejectedIDs,
		CreditEntries:   creditEntries,
	}, nil
}

// cappedCreatorFor resolves whether the grant loop must enforce the per-account emission cap,
// type-asserting the given credit repository (the TX-SCOPED one during finalization, design
// §4.1). It returns (cc, true) ONLY when a cap is configured AND cr implements
// credit.CappedCreator. It returns (nil, false) — the uncapped legacy Create path — in two
// cases: no cap configured (the default, and the common production state), or a cap configured
// against a repository that cannot enforce it (a misconfiguration). The misconfiguration is
// surfaced LOUD but not fatal: it WARNs at most once per engine lifetime (capWarnOnce) and lets
// the grant proceed uncapped, so a mis-wired cap never silently drops or fails every grant.
func (e *Engine) cappedCreatorFor(cr credit.Repository) (credit.CappedCreator, bool) {
	if e.emissionCapPerDay <= 0 {
		return nil, false
	}
	cc, ok := cr.(credit.CappedCreator)
	if !ok {
		e.capWarnOnce.Do(func() {
			e.logger.Warn("emission cap configured but credit repository does not support capped creation")
		})
		return nil, false
	}
	return cc, true
}

// rejectAll marks all pending results as DISAGREED, transitions the work unit to REJECTED,
// and triggers reassignment (or failure if max reassignments reached). verdict carries the
// LOSING clique when the caller compared (the largest coherent agreeing group that failed
// the gates — the honest group_size for a rejected unit's attestations); a nil verdict
// (no production caller) degrades to the empty-majority shape, group_size 0.
func (e *Engine) rejectAll(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, verdict *transition.ComparisonVerdict, policy transition.RedundancyPolicy, rawPendingCount int) (*ValidationResult, error) {
	ids := make([]types.ID, len(pending))
	for i, r := range pending {
		ids[i] = r.ID
	}

	// TX PHASE (design §4.1): mark all DISAGREED, guarded flip COMPLETED->REJECTED, and the
	// requeue (Reassign, REJECTED->QUEUED) all commit in ONE serialized transaction. Folding the
	// requeue into the tx removes the pre-fix strand where a crash between the REJECTED flip and
	// Reassign left an unreachable REJECTED unit that Decide has no action for. A Reassign error
	// now ABORTS the whole tx (was log-and-continue): the reject rolls back and the sweep
	// re-drives it, never a committed-but-unrequeued REJECTED strand.
	var reassignUpdated *workunit.WorkUnit
	var reassignRequeued bool
	if err := e.runFinalization(ctx, wu.ID, rawPendingCount, func(stores FinalizationStores) error {
		if err := stores.Results.BatchUpdateValidationStatus(ctx, ids, result.ValidationDisagreed); err != nil {
			return fmt.Errorf("mark all results DISAGREED: %w", err)
		}
		if _, err := stores.WorkUnits.UpdateState(ctx, wu.ID, workunit.WorkUnitStateCompleted, workunit.WorkUnitStateRejected); err != nil {
			return fmt.Errorf("transition work unit to REJECTED: %w", err)
		}
		updated, requeued, err := stores.WorkUnits.Reassign(ctx, wu.ID)
		if err != nil {
			return fmt.Errorf("reassign rejected work unit: %w", err)
		}
		reassignUpdated = updated
		reassignRequeued = requeued
		return nil
	}); err != nil {
		return nil, err
	}

	// --- POST-COMMIT best-effort effects (design §4.1): unchanged semantics + order, run over
	// the engine's own repos only after the reject+requeue tx has committed. ---

	if verdict == nil {
		verdict = transition.BuildComparisonVerdict(pending, nil, policy.TrustFloor)
	}

	// Create attestations for all rejected results (credit_amount = 0 via the nil map).
	e.createAttestations(ctx, wu, pending, attestation.OutcomeDisagreed, nil,
		e.buildQuorumDescriptor(proj, verdict, policy))

	// Update volunteer counters. H-7: best-effort — a failure does not fail the
	// rejection, so log at Warn, not Error.
	for _, r := range pending {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
		e.recordAdjudicated(ctx, r.VolunteerID, false)
	}

	// TODO #54: every result on a fully-rejected unit is wasted work — a bad reliability
	// signal for the machine that produced it. Best-effort (never fails the rejection).
	e.recordReliability(ctx, pending, false)

	// Log spot-check mismatch.
	if wu.SpotCheck {
		volIDs := make([]string, len(pending))
		for i, r := range pending {
			volIDs[i] = r.VolunteerID.String()
		}
		e.logger.Warn("spot-check mismatch: volunteers disagreed",
			"work_unit_id", wu.ID,
			"volunteer_ids", volIDs,
		)
	}

	// Requeue outcome logs (the Reassign itself committed inside the tx above).
	if reassignRequeued {
		e.logger.Info("rejected work unit reassigned", "work_unit_id", wu.ID, "reassignment_count", reassignUpdated.ReassignmentCount)
	} else {
		e.logger.Warn("rejected work unit failed after max reassignments", "work_unit_id", wu.ID, "reassignment_count", reassignUpdated.ReassignmentCount)
	}

	return &ValidationResult{
		WorkUnitID:      wu.ID,
		Outcome:         OutcomeRejected,
		RejectedResults: ids,
	}, nil
}

// createAttestations creates signed v2 credit attestations for each result. The attested
// credit_amount is looked up per result in amounts (keyed by result ID); a result absent from
// the map attests 0. This records the credit ACTUALLY granted, not the nominal leaf amount: a
// DISAGREED result (callers pass nil) attests 0, and an AGREED result whose grant the emission
// cap suppressed is likewise absent from the map and attests 0 — the attestation states facts
// (outcome AGREED, credit 0), per the design. desc is the unit's quorum descriptor, shared by
// every result of the unit (they all attest the same quorum event).
func (e *Engine) createAttestations(ctx context.Context, wu *workunit.WorkUnit, results []*result.Result, outcome string, amounts map[types.ID]float64, desc *attestation.QuorumDescriptor) {
	if e.attestationRepo == nil || e.signer == nil {
		return
	}

	now := types.Now()
	policyVersion := attestation.PolicyVersion
	for _, r := range results {
		// Look up the volunteer's public key.
		vol, err := e.volunteerRepo.GetByID(ctx, r.VolunteerID)
		if err != nil {
			e.logger.Error("failed to get volunteer for attestation",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
			continue
		}

		// Convert execution metadata to map[string]any for raw_metrics.
		rawMetrics := executionMetadataToMap(r.ExecutionMetadata)

		// Only a well-formed checksum may enter the signed payload: lowercase 64-hex, or ""
		// when the result carries none. A malformed value is only reachable through a
		// ref-only submission's CLAIMED checksum (head-computed inline hashes are hex by
		// construction); attest "" and say so rather than signing arbitrary bytes.
		checksum, ok := attestation.NormalizeOutputChecksum(r.OutputChecksum)
		if !ok {
			e.logger.Warn("result output checksum is not attestable; attesting empty",
				"work_unit_id", wu.ID, "result_id", r.ID)
		}

		resultID := r.ID
		amount := amounts[r.ID]
		att := &attestation.Attestation{
			SchemaVersion:         attestation.SchemaVersionV2,
			LeafID:                wu.LeafID,
			VolunteerPublicKey:    vol.PublicKey,
			WorkUnitID:            wu.ID,
			ResultID:              &resultID,
			OutputChecksum:        &checksum,
			QuorumDescriptor:      desc,
			PolicyVersion:         &policyVersion,
			RawMetrics:            rawMetrics,
			ValidationOutcome:     outcome,
			CreditAmount:          amount,
			CreditAmountCanonical: attestation.CanonicalCreditString(amount),
			AttestationTimestamp:  now,
		}

		sig, err := e.signer.Sign(att)
		if err != nil {
			e.logger.Error("failed to sign attestation",
				"work_unit_id", wu.ID, "volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
			continue
		}
		att.Signature = sig

		if err := e.attestationRepo.Create(ctx, att); err != nil {
			e.logger.Error("failed to create attestation",
				"work_unit_id", wu.ID, "volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
	}
}

// buildQuorumDescriptor assembles the signed v2 quorum descriptor from the resolved policy
// (what was DEMANDED) and the comparison verdict (what was DELIVERED, in distinct-subject
// units). A nil verdict — no production path — leaves the delivered counts at zero.
func (e *Engine) buildQuorumDescriptor(proj *leaf.Leaf, verdict *transition.ComparisonVerdict, policy transition.RedundancyPolicy) *attestation.QuorumDescriptor {
	d := &attestation.QuorumDescriptor{
		AuditRatePPM:            e.effectiveAuditRatePPM(proj),
		MinQuorum:               policy.MinQuorum,
		MinTrustedCorroborators: policy.MinTrustedCorroborators,
		TargetCopies:            policy.TargetCopies,
		TrustFloor:              policy.TrustFloor,
	}
	if verdict != nil {
		d.GroupSize = verdict.MajorityCount
		d.PendingSize = verdict.Total
		d.TrustedCorroborators = verdict.TrustedMajorityCount
	}
	return d
}

// effectiveAuditRate is the post-hoc result-audit sampling rate in force for a leaf: the MAX
// of the leaf override and the head default (a leaf may only RAISE sampling, audit F-H4), or
// 0 when auditing is disabled. Shared by the sampling hook and the attestation descriptor so
// the signed rate is exactly the operative one.
func (e *Engine) effectiveAuditRate(proj *leaf.Leaf) float64 {
	if !e.auditEnabled {
		return 0
	}
	rate := e.auditHeadRate
	if proj.ValidationConfig.AuditRate > rate {
		rate = proj.ValidationConfig.AuditRate
	}
	if rate < 0 {
		return 0
	}
	if rate > 1 {
		return 1
	}
	return rate
}

// effectiveAuditRatePPM renders the effective audit rate in parts-per-million for the signed
// quorum descriptor (integers only in the canonical form).
func (e *Engine) effectiveAuditRatePPM(proj *leaf.Leaf) int {
	return int(math.Round(e.effectiveAuditRate(proj) * 1e6))
}

// executionMetadataToMap converts an ExecutionMetadata struct to a map for attestation raw_metrics.
func executionMetadataToMap(em result.ExecutionMetadata) map[string]any {
	m := map[string]any{
		"wall_clock_seconds": em.WallClockSeconds,
		"cpu_seconds_user":   em.CPUSecondsUser,
		"cpu_seconds_system": em.CPUSecondsSystem,
		"cpu_cores_used":     em.CPUCoresUsed,
		"gpu_seconds":        em.GPUSeconds,
		"gpu_vram_used_mb":   em.GPUVRAMUsedMB,
		"peak_memory_mb":     em.PeakMemoryMB,
		"disk_read_mb":       em.DiskReadMB,
		"disk_write_mb":      em.DiskWriteMB,
		"network_rx_mb":      em.NetworkRxMB,
		"network_tx_mb":      em.NetworkTxMB,
	}
	if em.GPUModel != "" {
		m["gpu_model"] = em.GPUModel
	}
	return m
}

// recordReliability folds a batch of results' outcomes into the per-host reliability
// signal (TODO #54): good=true for AGREED results (the host delivered validated work),
// good=false for DISAGREED / rejected ones (it wasted a unit). Keyed on the MACHINE that
// produced each result (host_id, folding onto the account id when the volunteer reported no
// host — the per-account fallback). Best-effort: a write failure is logged and skipped, it
// never affects validation (credit is already granted; this is pure dispatch shaping).
func (e *Engine) recordReliability(ctx context.Context, results []*result.Result, good bool) {
	if e.reliabilityRepo == nil {
		return
	}
	for _, r := range results {
		hostKey := r.VolunteerID
		if r.HostID != nil {
			hostKey = *r.HostID
		}
		if err := e.reliabilityRepo.RecordOutcome(ctx, hostKey, good); err != nil {
			e.logger.Warn("failed to record host reliability signal",
				"host_id", hostKey, "good", good, "result_id", r.ID, "error", err)
		}
	}
}

// accrueTrust credits account-level trust for a corroborated-clean unit (see internal/trust).
// It collapses the agreed results to DISTINCT subjects (two devices under one identity are ONE
// principal, accruing at most once per unit) and awards +1 to a subject only when the agreement
// was WITNESSED strongly enough that a Sybil farm cannot bootstrap itself by cross-validating its
// own answers.
//
// The floor is resolved even when the gate is DISABLED: trust must accumulate before enforcement
// is switched on, so accrual can recognize which subjects are trusted. ResolveTrust returns
// K == 0 when the gate is off but still returns the real floor (now clamped >= 1 — BG-01a).
//
// Witness rule (D9 / audit F-H1), GATED on trusted-runner registry state so it is byte-identical
// to the pre-D9 rule until the operator actually registers a runner (accrual is always active in
// production, so an un-gated change would FREEZE newcomer accrual on every head whose only trusted
// subject is the seeded corroborator — the registry is necessarily empty on deploy day — a G2
// violation). Per call:
//
//   - registry EMPTY, no provider wired, or the registry query ERRORS (best-effort; a transient
//     DB blip must not freeze newcomer bootstrap — WARN and fall back): the legacy rule — a
//     subject accrues iff its agreeing group contains >= 1 OTHER trusted subject.
//   - registry has >= 1 ACTIVE runner: a subject accrues iff its agreeing group contains (a) a
//     trusted OTHER subject that is an active trusted runner, OR (b) >= 2 distinct trusted OTHER
//     subjects. This keeps G2 (the head's own registered corroborator single-witnesses newcomers
//     under (a)) while denying two colluding trusted accounts the ability to cheaply mint a
//     third identity's trust.
//
// The registry is queried at most ONCE per call, and only when at least one subject is a
// candidate to accrue under the base (>= 1 trusted other) rule. A non-countable result (submitter
// effective standing not OK at submit) is skipped, so it neither ACCRUES nor WITNESSES — a
// probation account is invisible to trust just as it is to the verdict. Best-effort and nil-safe:
// a nil store or a write error is logged and skipped, never failing validation.
func (e *Engine) accrueTrust(ctx context.Context, proj *leaf.Leaf, wu *workunit.WorkUnit, agreedResults []*result.Result) {
	if e.trustRepo == nil {
		return
	}
	quorum := proj.ValidationConfig.EffectiveMinQuorum()
	if wu.SpotCheck {
		quorum = 2
	}
	_, floor := e.trustPolicy.ResolveTrust(proj.ValidationConfig, quorum)

	// Collapse to distinct subjects, keeping each subject's max submission-time score (equal per
	// subject in practice; max is defensive). Reuses the transitioner's subject/score fallbacks so
	// accrual and the acceptance verdict apply identical rules. Non-countable results are skipped.
	subjectScore := make(map[string]int)
	for _, r := range agreedResults {
		if !transition.StandingCountable(r) {
			continue
		}
		subj := transition.SubjectForResult(r)
		sc := transition.ScoreForResult(r)
		if cur, ok := subjectScore[subj]; !ok || sc > cur {
			subjectScore[subj] = sc
		}
	}

	trustedCount := 0
	for _, sc := range subjectScore {
		if sc >= floor {
			trustedCount++
		}
	}

	// Candidate under the base rule = some subject has >= 1 OTHER trusted subject. An untrusted
	// subject has trustedCount trusted others; a trusted one has trustedCount-1. So a candidate
	// exists iff trustedCount >= 2, or trustedCount == 1 with an untrusted subject present. If
	// none, nobody accrues under ANY rule — skip the registry query entirely.
	if !(trustedCount >= 2 || (trustedCount == 1 && len(subjectScore) > trustedCount)) {
		return
	}

	// Resolve the trusted-runner registry state ONCE. registryActive stays false (legacy rule)
	// when there is no provider, the registry is empty, or the query errors (G2: a transient blip
	// must not freeze newcomer bootstrap).
	registryActive := false
	var runnerSet map[string]bool
	if e.runnerSubjects != nil {
		subs, err := e.runnerSubjects.ActiveRunnerSubjects(ctx)
		if err != nil {
			e.logger.Warn("failed to resolve active trusted-runner subjects for trust accrual; using legacy witness rule",
				"work_unit_id", wu.ID, "error", err)
		} else if len(subs) > 0 {
			registryActive = true
			runnerSet = make(map[string]bool, len(subs))
			for _, s := range subs {
				runnerSet[s] = true
			}
		}
	}

	// Count trusted subjects that are active runners (rule (a)); only meaningful when active.
	trustedRunnerCount := 0
	if registryActive {
		for subj, sc := range subjectScore {
			if sc >= floor && runnerSet[subj] {
				trustedRunnerCount++
			}
		}
	}

	for subj, sc := range subjectScore {
		trustedOthers := trustedCount
		runnerOthers := trustedRunnerCount
		if sc >= floor {
			trustedOthers-- // a trusted subject cannot corroborate itself
			if runnerSet[subj] {
				runnerOthers-- // ... nor count itself toward the runner-witness rule
			}
		}
		if trustedOthers < 1 {
			continue // no trusted witness at all -> never accrues under any rule
		}
		// registry off -> legacy (>= 1 trusted other); registry on -> rule (a) a trusted-other
		// runner OR rule (b) >= 2 distinct trusted others.
		accrues := !registryActive || trustedOthers >= 2 || runnerOthers >= 1
		if !accrues {
			continue
		}
		if err := e.trustRepo.AccrueCleanUnit(ctx, subj); err != nil {
			e.logger.Warn("failed to accrue trust for agreed subject",
				"subject", subj, "work_unit_id", wu.ID, "error", err)
		}
	}
}

// maybeSampleForAudit is the post-validation result-audit sampling hook (spec §7.2). It runs at
// the TAIL of acceptResults — the unit is VALIDATED and fully credited — so selection is post-hoc
// and carries zero dispatch signal. Best-effort: any error is WARNed and never fails validation
// (a dropped sample is indistinguishable from an unlucky draw).
//
// Eligibility is applied BEFORE the draw so no runner compute is wasted on a verdict that would be
// INCONCLUSIVE by construction; every owner-selectable skip lane (NetworkAccess, CUSTOM, unpinned
// or ref-only NUMERIC, HR-without-pin, canon-empty) increments the per-leaf ineligible counter the
// fault-monitor probe reads (§7.2 skip-lane visibility — the lanes are made operator-VISIBLE in v1
// rather than closed, which is the deferred determinism_class work).
func (e *Engine) maybeSampleForAudit(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, agreedResults []*result.Result) {
	if !e.auditEnabled || e.auditEnq == nil {
		return
	}

	// Effective rate is the MAX of the leaf override and the head default (audit F-H4): leaf
	// creation is self-service and the owner is the primary adversary, so a per-leaf audit_rate may
	// only RAISE sampling above the head floor, never lower it. Shared with the attestation
	// quorum descriptor so the signed rate is exactly the operative one.
	rate := e.effectiveAuditRate(proj)

	// Representative winner: the AGREED member with the lexicographically smallest result UUID
	// (deterministic; all members were co-credited so the pick carries no incentive weight, and the
	// NUMERIC verdict reads ALL members anyway — F-M3).
	var winner *result.Result
	for _, r := range agreedResults {
		if winner == nil || r.ID.String() < winner.ID.String() {
			winner = r
		}
	}
	if winner == nil {
		return
	}

	leafID := wu.LeafID.String()
	mode := proj.ValidationConfig.ComparisonMode
	pin := ""
	if wu.HRClass != nil {
		pin = *wu.HRClass
	}

	// --- Eligibility filter (§7.2), in order; log Debug + count the ineligible lane on each skip. ---

	// CUSTOM is unshippable (unchanged) — checked first so an unshippable leaf never triggers an
	// artifact-version lookup.
	if mode == leaf.ComparisonCustom {
		e.recordAuditIneligible(leafID, wu.ID, "custom_comparison_mode")
		return
	}

	// Resolve the winner's effective ExecutionConfig SNAPSHOT (audit F-M4): leafs.execution_config
	// is owner-mutable mid-window, so the runner must execute the pinned artifact version's frozen
	// config when the winner ran one — never a claim-time resolution of owner-mutable config. It
	// drives both the NetworkAccess check below and the recorded ExecutionSnapshot.
	execSnap := proj.ExecutionConfig
	if winner.ArtifactVersionID != nil {
		if e.auditVersions == nil {
			e.logger.Warn("result-audit sampling: no artifact-version resolver wired for a versioned winner; skipping",
				"work_unit_id", wu.ID, "artifact_version_id", winner.ArtifactVersionID)
			return
		}
		av, err := e.auditVersions.GetVersionByID(ctx, *winner.ArtifactVersionID)
		if err != nil || av == nil {
			e.logger.Warn("result-audit sampling: failed to resolve winner artifact version; skipping",
				"work_unit_id", wu.ID, "artifact_version_id", winner.ArtifactVersionID, "error", err)
			return
		}
		execSnap = av.ExecutionConfig
	}

	// A network-touching leaf can be legitimately non-deterministic across time; a false MISMATCH
	// recorded now would poison slice-3 enforcement later.
	if execSnap.NetworkAccess {
		e.recordAuditIneligible(leafID, wu.ID, "network_access")
		return
	}

	// HR pinning (audit F-H2): HomogeneousRedundancy exists precisely because such leaves are NOT
	// portably deterministic across hardware classes, so a leaf that declares it but whose unit
	// somehow lacks a pin cannot be safely re-executed cross-class.
	if proj.ValidationConfig.HomogeneousRedundancy && pin == "" {
		e.recordAuditIneligible(leafID, wu.ID, "homogeneous_redundancy_without_pin")
		return
	}

	var acceptedKey *string
	switch mode {
	case leaf.ComparisonNumericTolerance:
		// NUMERIC needs the winner's inline bytes (flattenOutput needs raw bytes) AND an hr_class
		// pin (bit-for-bit numeric reproduction is only expected within one class).
		if len(winner.OutputData) == 0 || pin == "" {
			e.recordAuditIneligible(leafID, wu.ID, "numeric_unpinned_or_ref_only")
			return
		}
	case leaf.ComparisonExact:
		key, err := comparisonKey(winner, proj.ValidationConfig.IgnoreFields)
		if err != nil {
			e.logger.Warn("result-audit sampling: failed to compute accepted comparison key; skipping",
				"work_unit_id", wu.ID, "result_id", winner.ID, "error", err)
			e.recordAuditIneligible(leafID, wu.ID, "comparison_key_error")
			return
		}
		// canon-empty keys embed the winner's result UUID and are unadjudicable against runner bytes
		// (F-M2).
		if strings.HasPrefix(key, "canon-empty:") {
			e.recordAuditIneligible(leafID, wu.ID, "canon_empty_key")
			return
		}
		// unverified-ref keys embed the winner's result UUID (a ref not yet head-verified) and are
		// unadjudicable against runner bytes exactly like canon-empty (F4). Unreachable on current
		// paths — a sampled ref winner is promoted-verified, so its key is the 64-hex verified hash —
		// but this mirrors the canon-empty defense-in-depth at the enqueue seam.
		if strings.HasPrefix(key, "unverified-ref:") {
			e.recordAuditIneligible(leafID, wu.ID, "unverified_ref_key")
			return
		}
		k := key
		acceptedKey = &k
	default:
		e.logger.Warn("result-audit sampling: unknown comparison mode; skipping",
			"work_unit_id", wu.ID, "comparison_mode", mode)
		e.recordAuditIneligible(leafID, wu.ID, "unknown_comparison_mode")
		return
	}

	// The draw is LAST: eligibility is deterministic; only eligible units consume the sampling
	// probability, and the RNG is fail-safe (samples on rand error).
	if !audit.ShouldSample(rate) {
		return
	}

	snap := audit.ComparisonSnapshot{
		ComparisonMode: mode,
		IgnoreFields:   proj.ValidationConfig.IgnoreFields,
		CompareFields:  proj.ValidationConfig.CompareFields,
	}
	if mode == leaf.ComparisonNumericTolerance && proj.ValidationConfig.NumericTolerance != nil {
		snap.NumericTolerance = *proj.ValidationConfig.NumericTolerance
	}

	var requiredHRClass *string
	if pin != "" {
		p := pin
		requiredHRClass = &p // set for EVERY pinned unit regardless of mode (F-H2)
	}

	a := &audit.Audit{
		WorkUnitID:            wu.ID,
		LeafID:                wu.LeafID,
		AcceptedResultID:      winner.ID,
		AcceptedComparisonKey: acceptedKey, // EXACT only; nil for NUMERIC (value-level verdict)
		ComparisonSnapshot:    snap,
		RequiredHRClass:       requiredHRClass,
		ArtifactVersionID:     winner.ArtifactVersionID,
		ExecutionSnapshot:     execSnap,
	}
	if err := e.auditEnq.Enqueue(ctx, a); err != nil {
		// A unique-violation (one open audit per unit already) or any other error is best-effort:
		// WARN and drop, never fail validation.
		e.logger.Warn("result-audit sampling: failed to enqueue audit job",
			"work_unit_id", wu.ID, "leaf_id", wu.LeafID, "error", err)
		return
	}
	e.logger.Info("result audit sampled",
		"work_unit_id", wu.ID, "leaf_id", wu.LeafID, "accepted_result_id", winner.ID, "rate", rate)
}

// recordAuditIneligible logs a Debug skip and bumps the per-leaf validated-but-ineligible counter
// the fault-monitor probe reads. Lazily initializes the map under the lock.
func (e *Engine) recordAuditIneligible(leafID string, wuID types.ID, reason string) {
	e.logger.Debug("result-audit sampling: unit ineligible",
		"leaf_id", leafID, "work_unit_id", wuID, "reason", reason)
	e.auditIneligibleMu.Lock()
	if e.auditIneligible == nil {
		e.auditIneligible = make(map[string]int64)
	}
	e.auditIneligible[leafID]++
	e.auditIneligibleMu.Unlock()
}

// AuditIneligibleCounts returns a snapshot copy of the per-leaf count of validated-but-ineligible
// units the sampling hook has skipped (network access, CUSTOM, unpinned/ref-only NUMERIC,
// canon-empty, HR-without-pin, ...). The fault-monitor probe reads it to WARN when a leaf's
// ineligible share is anomalous (F-H4/F-M5: owner-steerable never-audited lanes made
// operator-visible in v1). Keyed by leaf id string; empty when nothing has been skipped.
func (e *Engine) AuditIneligibleCounts() map[string]int64 {
	e.auditIneligibleMu.Lock()
	defer e.auditIneligibleMu.Unlock()
	out := make(map[string]int64, len(e.auditIneligible))
	for k, v := range e.auditIneligible {
		out[k] = v
	}
	return out
}

// rejectionWarnRate is the decayed rejection rate above which the per-adjudication
// WARN fires — the same 20% line the legacy lifetime-rate check used.
const rejectionWarnRate = 0.20

// recordAdjudicated folds one adjudicated result outcome (AGREED or DISAGREED —
// EXPIRED/ABANDONED never reach the adjudication paths) into the volunteer's decayed
// rejection-rate backpressure signal (BG-24/BG-24b PR-B). Best-effort like every
// other post-credit effect here: errors are logged, never failing the validation
// outcome. With the machine disabled (nil recorder, the default) it preserves
// today's behavior exactly: the lifetime-rate WARN for rejected results, nothing
// for agreed ones.
func (e *Engine) recordAdjudicated(ctx context.Context, volunteerID types.ID, agreed bool) {
	if e.standingRecorder == nil {
		if !agreed {
			e.checkRejectionRate(ctx, volunteerID)
		}
		return
	}
	out, err := e.standingRecorder.RecordAdjudicated(ctx, volunteerID, agreed)
	if err != nil {
		e.logger.Warn("failed to record adjudicated outcome for standing backpressure",
			"volunteer_id", volunteerID, "agreed", agreed, "error", err)
		return
	}
	if !out.Applied {
		// OPERATOR-owned row (or the volunteer vanished): the machine never touches it.
		return
	}
	if out.OldStanding != out.NewStanding {
		e.logger.Warn("standing backpressure transition",
			"volunteer_id", volunteerID,
			"from", out.OldStanding,
			"to", out.NewStanding,
			"benched_until", out.BenchedUntil,
			"rejection_rate", fmt.Sprintf("%.1f%%", out.Rate*100),
			"decayed_sample", fmt.Sprintf("%.1f", out.Sample),
		)
		return
	}
	// The legacy WARN, now on the decayed rate: require at least one whole
	// adjudication of decayed weight so a stale, fully-decayed signal stays quiet.
	if out.Rate > rejectionWarnRate && out.Sample >= 1 {
		e.logger.Warn("volunteer rejection rate exceeds 20%",
			"volunteer_id", volunteerID,
			"rejection_rate", fmt.Sprintf("%.1f%%", out.Rate*100),
			"decayed_sample", fmt.Sprintf("%.1f", out.Sample),
			"standing", out.NewStanding,
		)
	}
}

// checkRejectionRate logs a warning if a volunteer's rejection rate exceeds 20%.
// It is the legacy lifetime-counter signal, kept for the backpressure-disabled
// path (nil standingRecorder): the counters are monotonic, so the rate can never
// recover — the decayed signal above supersedes it when the machine is enabled.
func (e *Engine) checkRejectionRate(ctx context.Context, volunteerID types.ID) {
	vol, err := e.volunteerRepo.GetByID(ctx, volunteerID)
	if err != nil {
		return
	}

	total := vol.TotalWorkUnitsCompleted + vol.TotalWorkUnitsRejected
	if total < 1 {
		return
	}

	rate := float64(vol.TotalWorkUnitsRejected) / float64(total)
	if rate > 0.20 {
		e.logger.Warn("volunteer rejection rate exceeds 20%",
			"volunteer_id", volunteerID,
			"rejection_rate", fmt.Sprintf("%.1f%%", rate*100),
			"completed", vol.TotalWorkUnitsCompleted,
			"rejected", vol.TotalWorkUnitsRejected,
		)
	}
}

// flatVal is one flattened JSON leaf: either a finite number (IsNum) or a stringified
// non-numeric scalar (string/bool/null). Numeric leaves compare within epsilon;
// non-numeric leaves compare for equality.
type flatVal struct {
	Num   float64
	IsNum bool
	Str   string
}

// flattenOutput parses JSON output data and flattens it to a path -> leaf map.
//
// Objects nest as dotted paths ("replay.dt"); array elements index as "fights.0.winner".
// ignoreFields paths are dropped; if compareFields is non-empty ONLY matching paths are
// kept. Path matching is exact or dot-boundary-prefix (subtree), so "fights" selects all
// of fights.* and "compute_time_ms" drops just that field.
//
// Non-finite numbers (NaN, ±Inf) are rejected as invalid — the same security safeguard as
// the original flat parser: comparing NaN with math.Abs(va-vb) > epsilon yields false,
// which would otherwise let two poisoned results be judged "matching" and reach quorum.
// JSON like 1e400 decodes to ±Inf, so this finiteness check (not the decoder alone) is
// what catches it.
func flattenOutput(data json.RawMessage, ignoreFields, compareFields []string) (map[string]flatVal, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty output data")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("unmarshal output: %w", err)
	}
	out := make(map[string]flatVal)
	if err := flattenInto(out, "", v, ignoreFields, compareFields); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenInto(out map[string]flatVal, path string, v interface{}, ignore, compare []string) error {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if err := flattenInto(out, p, val, ignore, compare); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, val := range t {
			p := path + "." + itoa(i)
			if path == "" {
				p = itoa(i)
			}
			if err := flattenInto(out, p, val, ignore, compare); err != nil {
				return err
			}
		}
	default: // scalar leaf
		if matchesFieldPath(ignore, path) {
			return nil
		}
		if len(compare) > 0 && !matchesFieldPath(compare, path) {
			return nil
		}
		switch x := v.(type) {
		case json.Number:
			f, err := x.Float64()
			if err != nil {
				return fmt.Errorf("non-numeric number at %q: %v", path, x)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Errorf("non-finite numeric output at %q: %v", path, x)
			}
			out[path] = flatVal{Num: f, IsNum: true}
		case string:
			out[path] = flatVal{Str: x}
		case bool:
			if x {
				out[path] = flatVal{Str: "true"}
			} else {
				out[path] = flatVal{Str: "false"}
			}
		case nil:
			out[path] = flatVal{Str: "null"}
		default:
			return fmt.Errorf("unsupported leaf type %T at %q", v, path)
		}
	}
	return nil
}

// numericMatch returns true if two flattened outputs agree: identical path sets, numeric
// leaves within epsilon, non-numeric leaves equal.
func numericMatch(a, b map[string]flatVal, epsilon float64) bool {
	// An EMPTY compared set is never agreement. If compare_fields selected no path that
	// exists in the output, or ignore_fields stripped every leaf, both maps are empty and
	// the loop below would vacuously return true — letting two results with DIFFERENT
	// content corroborate on nothing. Fail closed: nothing compared means no match.
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for key, va := range a {
		vb, ok := b[key]
		if !ok {
			return false
		}
		if va.IsNum != vb.IsNum {
			return false
		}
		if va.IsNum {
			// Defense in depth: never treat non-finite values as matching (parse
			// rejection should already exclude these, but guard regardless).
			if math.IsNaN(va.Num) || math.IsInf(va.Num, 0) || math.IsNaN(vb.Num) || math.IsInf(vb.Num, 0) {
				return false
			}
			if math.Abs(va.Num-vb.Num) > epsilon {
				return false
			}
		} else if va.Str != vb.Str {
			return false
		}
	}
	return true
}

// comparisonKey returns the EXACT-mode grouping key for a result. A volunteer-CLAIMED
// checksum can never be a comparison key (BG-02b, §10.8): a ref-only result (no inline
// output bytes) keys on the HEAD-computed verified_output_checksum once the head has
// fetched and hashed the external bytes itself, and until then gets a per-result
// non-grouping key ("unverified-ref:<uuid>") so two refs sharing a fabricated claimed
// checksum can never group — it is NOT an error (erroring would break Compare/rejectAll
// on legacy rows; this is the canon-empty:<uuid> precedent). An INLINE result with no
// ignore_fields keys on its output_checksum — head-verified at submit, identical to the
// historical behavior. With ignore_fields AND inline output present, the key is a
// canonical SHA-256 over the output with those fields stripped and object keys sorted, so
// volatile provenance (e.g. a wall-clock compute_time_ms) no longer prevents agreement.
func comparisonKey(r *result.Result, ignoreFields []string) (string, error) {
	if len(r.OutputData) == 0 { // ref-only result: no inline bytes, only an external output URL
		if r.VerifiedOutputChecksum != nil { // the head fetched + hashed these bytes itself
			return *r.VerifiedOutputChecksum, nil // the ONLY key a ref may ever vote on (§10.8)
		}
		// Not yet (or never) head-verified: a per-result key that groups with nothing, so a
		// volunteer-claimed checksum can never corroborate two refs (the BG-02b hole).
		return "unverified-ref:" + r.ID.String(), nil
	}
	if len(ignoreFields) == 0 {
		return r.OutputChecksum, nil // inline result: output_checksum is head-verified at submit
	}
	stripped, err := strippedValue(r.OutputData, ignoreFields)
	if err != nil {
		return "", err
	}
	// If ignore_fields strips every leaf, the canonical form collapses to the SAME empty
	// shape for every result — a vacuous agreement among differing outputs. Give such a
	// result a unique, non-grouping key so it can never corroborate on nothing.
	if !hasAnyLeaf(stripped) {
		return "canon-empty:" + r.ID.String(), nil
	}
	canon, err := json.Marshal(stripped)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "canon:" + hex.EncodeToString(sum[:]), nil
}

// strippedValue parses output JSON (numbers preserved via UseNumber) and removes
// ignoreFields, returning the decoded value with those paths stripped. comparisonKey then
// re-marshals it deterministically (json.Marshal sorts object keys and emits json.Number
// verbatim, so numeric tokens and key order normalize identically for every result).
func strippedValue(data json.RawMessage, ignoreFields []string) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse output as JSON: %w", err)
	}
	return stripFields(v, "", ignoreFields), nil
}

// hasAnyLeaf reports whether v contains at least one scalar leaf. An object or array whose
// descendants were all stripped by ignore_fields has no leaves, so there is nothing to
// compare — such a result must not group with any other (see comparisonKey).
func hasAnyLeaf(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, val := range t {
			if hasAnyLeaf(val) {
				return true
			}
		}
		return false
	case []interface{}:
		for _, val := range t {
			if hasAnyLeaf(val) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// stripFields recursively removes object fields whose dotted path matches an ignore
// pattern. Array indices are elided from the path, so "fights.compute_time_ms" drops that
// key from every element of the fights array.
func stripFields(v interface{}, prefix string, ignore []string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if matchesFieldPath(ignore, p) {
				continue
			}
			out[k] = stripFields(val, p, ignore)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = stripFields(val, prefix, ignore)
		}
		return out
	default:
		return v
	}
}

// matchesFieldPath reports whether path equals, or is a dot-boundary descendant of, any
// pattern.
func matchesFieldPath(patterns []string, path string) bool {
	for _, p := range patterns {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

// itoa is a tiny non-allocating-ish int formatter for flatten paths (avoids importing
// strconv solely for this).
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// findLargestClique finds the largest subset of nodes where all pairs are mutually compatible.
// Uses brute force enumeration — suitable for small N (≤ 5, which is the max redundancy factor).
//
// A tie for the largest size (two or more distinct-membership cliques of equal maximum size)
// means the largest agreeing group is ambiguous, so there is no unique majority: it returns
// nil ("no agreement"). The correct response to ambiguity is more data, never an arbitrary
// (grindable) pick among equally-large cliques.
func findLargestClique(n int, compatible [][]bool) []int {
	var bestClique []int
	tie := false

	for mask := 1; mask < (1 << n); mask++ {
		var members []int
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				members = append(members, i)
			}
		}

		allCompat := true
		for a := 0; a < len(members) && allCompat; a++ {
			for b := a + 1; b < len(members) && allCompat; b++ {
				if !compatible[members[a]][members[b]] {
					allCompat = false
				}
			}
		}
		if !allCompat {
			continue
		}

		switch {
		case len(members) > len(bestClique):
			bestClique = members
			tie = false
		case len(members) == len(bestClique):
			// Another clique of the current maximum size but different membership: the
			// largest clique is not unique.
			tie = true
		}
	}

	if tie {
		return nil
	}
	return bestClique
}
