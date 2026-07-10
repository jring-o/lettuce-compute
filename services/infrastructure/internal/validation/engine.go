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
	attestationRepo attestation.Repository
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
	signer      *attestation.Signer
	logger      *slog.Logger
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
	attestationRepo attestation.Repository,
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
		return e.validateExact(ctx, wu, proj, pending)
	case leaf.ComparisonNumericTolerance:
		return e.validateNumericTolerance(ctx, wu, proj, pending)
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
func (e *Engine) ApplyAccept(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending, majority []*result.Result) error {
	_, err := e.acceptResults(ctx, wu, proj, pending, majority)
	return err
}

// ApplyReject performs the reject effects: mark all pending DISAGREED, transition
// COMPLETED -> REJECTED, attest, and requeue (Reassign). The unit must already be COMPLETED.
// The engine half of the transitioner's ActionReject.
func (e *Engine) ApplyReject(ctx context.Context, wu *workunit.WorkUnit, pending []*result.Result) error {
	_, err := e.rejectAll(ctx, wu, pending)
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
func (e *Engine) validateExact(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result) (*ValidationResult, error) {
	majorityGroup, err := e.compareExact(proj, pending)
	if err != nil {
		return nil, err
	}
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup)
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

	// Group results by (canonical) checksum.
	groups := make(map[string][]*result.Result)
	for _, r := range pending {
		key, err := comparisonKey(r, ignoreFields)
		if err != nil {
			return nil, fmt.Errorf("canonicalize output for result %s: %w", r.ID, err)
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
func (e *Engine) validateNumericTolerance(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result) (*ValidationResult, error) {
	majorityGroup, err := e.compareNumericTolerance(proj, pending)
	if err != nil {
		return nil, err
	}
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup)
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

	// Flatten all result output data into path -> value maps. Nested objects/arrays
	// are flattened to dotted/indexed paths; numeric leaves compare within epsilon and
	// non-numeric leaves compare for equality. ignore_fields are dropped; if
	// compare_fields is non-empty only matching paths are kept (so a chaotic sim can be
	// validated on its aggregate science while its raw per-fight trajectory is excluded).
	parsed := make([]map[string]flatVal, len(pending))
	for i, r := range pending {
		m, err := flattenOutput(r.OutputData, ignoreFields, compareFields)
		if err != nil {
			return nil, fmt.Errorf("parse output_data for result %s: %w", r.ID, err)
		}
		parsed[i] = m
	}

	// Build compatibility matrix.
	n := len(pending)
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

	// Build majority group from clique indices.
	majorityGroup := make([]*result.Result, len(clique))
	for i, idx := range clique {
		majorityGroup[i] = pending[idx]
	}

	return majorityGroup, nil
}

// applyThreshold applies the agreement gates and performs the validation outcome.
//
// This is the LEGACY path (TryValidate), retained for tests only — every production submit
// routes through internal/transition. It is deliberately kept in lockstep with transition.Decide
// (per the PR #80 discipline): it builds the SAME subject-level verdict and applies the SAME
// four gates the production decider does, so the two can never disagree about what validates.
func (e *Engine) applyThreshold(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result) (*ValidationResult, error) {
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
	if v.MajorityCount >= quorum && 2*v.MajorityCount > v.Total && v.Ratio >= threshold && v.TrustedMajorityCount >= k {
		return e.acceptResults(ctx, wu, proj, pending, majorityGroup)
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
	return e.rejectAll(ctx, wu, pending)
}

// acceptResults marks majority results as AGREED, minority as DISAGREED,
// transitions the work unit, grants credit, and updates volunteer counters.
func (e *Engine) acceptResults(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result) (*ValidationResult, error) {
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

	// Mark agreed results.
	if err := e.resultRepo.BatchUpdateValidationStatus(ctx, agreedIDs, result.ValidationAgreed); err != nil {
		return nil, fmt.Errorf("mark results AGREED: %w", err)
	}

	// Mark disagreed results.
	if len(rejectedIDs) > 0 {
		if err := e.resultRepo.BatchUpdateValidationStatus(ctx, rejectedIDs, result.ValidationDisagreed); err != nil {
			return nil, fmt.Errorf("mark results DISAGREED: %w", err)
		}
	}

	// Transition work unit: COMPLETED → VALIDATED.
	_, err := e.workUnitRepo.UpdateState(ctx, wu.ID, workunit.WorkUnitStateCompleted, workunit.WorkUnitStateValidated)
	if err != nil {
		return nil, fmt.Errorf("transition work unit to VALIDATED: %w", err)
	}

	// Grant credit for each agreed result.
	creditAmount := proj.CreditConfig.CreditPerValidatedWorkUnit
	if creditAmount <= 0 {
		creditAmount = 1.0
	}

	// attestedAmounts carries the credit ACTUALLY granted per result into createAttestations: a
	// granted result attests the leaf amount; a result whose grant the emission cap suppressed is
	// ABSENT (resolving to 0). DISAGREED results are never keyed here, so they attest 0 as before.
	attestedAmounts := make(map[types.ID]float64, len(agreedResults))

	// When an emission cap is configured, route grants through CreateCapped so a suppression is a
	// non-error branch (design §5.3, audit F3). cappedCreator returns (nil, false) both when no
	// cap is set — the default, keeping the byte-for-byte legacy Create path below — and when a
	// cap is set but the repo lacks the capability (it WARNs once and falls back to Create).
	cc, capEnforced := e.cappedCreator()

	var creditEntries []*credit.LedgerEntry
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
				return nil, fmt.Errorf("create credit entry for result %s: %w", r.ID, err)
			}
			if !inserted {
				// Suppression branch (design §5.3, audit F3/F10): the account's rolling-24h
				// grants plus this amount would exceed the cap. Grant NOTHING — no ledger row,
				// no RAC upsert, no attested credit — but leave the result AGREED so every
				// work-quality effect below (counters, standing, reliability, trust) still
				// fires. The cap bounds emission, not merit.
				e.logger.Warn("credit suppressed by daily emission cap",
					"volunteer_id", r.VolunteerID,
					"work_unit_id", wu.ID,
					"result_id", r.ID,
					"amount", creditAmount,
					"cap", e.emissionCapPerDay)
				continue
			}
		} else if err := e.creditRepo.Create(ctx, entry); err != nil {
			return nil, fmt.Errorf("create credit entry for result %s: %w", r.ID, err)
		}
		// Update RAC for this volunteer+project. H-7: best-effort — a failure does not
		// fail validation (credit is already granted), so log at Warn, not Error.
		if e.racRepo != nil {
			if err := e.racRepo.Upsert(ctx, r.VolunteerID, wu.LeafID, creditAmount); err != nil {
				e.logger.Warn("failed to update RAC",
					"volunteer_id", r.VolunteerID, "leaf_id", wu.LeafID, "result_id", r.ID, "error", err)
			}
		}
		creditEntries = append(creditEntries, entry)
		attestedAmounts[r.ID] = creditAmount
	}

	// Create attestations for agreed results. The amount attested is the credit actually granted
	// (attestedAmounts): a cap-suppressed AGREED result is absent from the map and attests 0.
	e.createAttestations(ctx, wu, agreedResults, attestation.OutcomeAgreed, attestedAmounts)

	// Create attestations for disagreed results (credit_amount = 0 via the nil map).
	e.createAttestations(ctx, wu, rejectedResults, attestation.OutcomeDisagreed, nil)

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

	return &ValidationResult{
		WorkUnitID:      wu.ID,
		Outcome:         OutcomeValidated,
		AgreedResults:   agreedIDs,
		RejectedResults: rejectedIDs,
		CreditEntries:   creditEntries,
	}, nil
}

// cappedCreator resolves whether the grant loop must enforce the per-account emission cap.
// It returns (cc, true) ONLY when a cap is configured AND the credit repository implements
// credit.CappedCreator. It returns (nil, false) — the uncapped legacy Create path — in two
// cases: no cap configured (the default, and the common production state), or a cap configured
// against a repository that cannot enforce it (a misconfiguration). The misconfiguration is
// surfaced LOUD but not fatal: it WARNs at most once per engine lifetime (capWarnOnce) and lets
// the grant proceed uncapped, so a mis-wired cap never silently drops or fails every grant.
func (e *Engine) cappedCreator() (credit.CappedCreator, bool) {
	if e.emissionCapPerDay <= 0 {
		return nil, false
	}
	cc, ok := e.creditRepo.(credit.CappedCreator)
	if !ok {
		e.capWarnOnce.Do(func() {
			e.logger.Warn("emission cap configured but credit repository does not support capped creation")
		})
		return nil, false
	}
	return cc, true
}

// rejectAll marks all pending results as DISAGREED, transitions the work unit to REJECTED,
// and triggers reassignment (or failure if max reassignments reached).
func (e *Engine) rejectAll(ctx context.Context, wu *workunit.WorkUnit, pending []*result.Result) (*ValidationResult, error) {
	ids := make([]types.ID, len(pending))
	for i, r := range pending {
		ids[i] = r.ID
	}

	if err := e.resultRepo.BatchUpdateValidationStatus(ctx, ids, result.ValidationDisagreed); err != nil {
		return nil, fmt.Errorf("mark all results DISAGREED: %w", err)
	}

	_, err := e.workUnitRepo.UpdateState(ctx, wu.ID, workunit.WorkUnitStateCompleted, workunit.WorkUnitStateRejected)
	if err != nil {
		return nil, fmt.Errorf("transition work unit to REJECTED: %w", err)
	}

	// Create attestations for all rejected results (credit_amount = 0 via the nil map).
	e.createAttestations(ctx, wu, pending, attestation.OutcomeDisagreed, nil)

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

	// Reassign or fail the rejected work unit.
	updated, requeued, err := e.workUnitRepo.Reassign(ctx, wu.ID)
	if err != nil {
		e.logger.Error("failed to reassign rejected work unit", "work_unit_id", wu.ID, "error", err)
	} else if requeued {
		e.logger.Info("rejected work unit reassigned", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
	} else {
		e.logger.Warn("rejected work unit failed after max reassignments", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
	}

	return &ValidationResult{
		WorkUnitID:      wu.ID,
		Outcome:         OutcomeRejected,
		RejectedResults: ids,
	}, nil
}

// createAttestations creates signed credit attestations for each result. The attested
// credit_amount is looked up per result in amounts (keyed by result ID); a result absent from
// the map attests 0. This records the credit ACTUALLY granted, not the nominal leaf amount: a
// DISAGREED result (callers pass nil) attests 0, and an AGREED result whose grant the emission
// cap suppressed is likewise absent from the map and attests 0 — the attestation states facts
// (outcome AGREED, credit 0), per the design.
func (e *Engine) createAttestations(ctx context.Context, wu *workunit.WorkUnit, results []*result.Result, outcome string, amounts map[types.ID]float64) {
	if e.attestationRepo == nil || e.signer == nil {
		return
	}

	now := types.Now()
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

		att := &attestation.Attestation{
			LeafID:               wu.LeafID,
			VolunteerPublicKey:   vol.PublicKey,
			WorkUnitID:           wu.ID,
			RawMetrics:           rawMetrics,
			ValidationOutcome:    outcome,
			CreditAmount:         amounts[r.ID],
			AttestationTimestamp: now,
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
// It collapses the agreed results to DISTINCT subjects (two devices under one identity are
// ONE principal, accruing at most once per unit) and awards +1 to a subject ONLY when a
// DIFFERENT agreed subject was already trusted (its submission-time score met the floor).
//
// The floor is resolved even when the gate is DISABLED: trust must accumulate before
// enforcement is switched on, so accrual can recognize which subjects are trusted. ResolveTrust
// returns K == 0 when the gate is off but still returns the real floor.
//
// The "witnessed by a trusted subject" rule is the Sybil rationale: agreement purely among
// untrusted newcomers earns credit but ZERO trust, so a Sybil farm cannot bootstrap itself by
// cross-validating its own answers — trust only ever spreads outward from a subject the
// operator seeded via the admin API. It is asymmetric: an untrusted subject corroborated by a
// trusted one accrues, but that lone trusted subject does NOT accrue off untrusted peers (it
// has no OTHER trusted witness), so a single trusted account cannot mint trust for a second
// identity it also controls. Best-effort and nil-safe: a nil store or a write error is logged
// and skipped, never failing validation.
func (e *Engine) accrueTrust(ctx context.Context, proj *leaf.Leaf, wu *workunit.WorkUnit, agreedResults []*result.Result) {
	if e.trustRepo == nil {
		return
	}
	quorum := proj.ValidationConfig.EffectiveMinQuorum()
	if wu.SpotCheck {
		quorum = 2
	}
	_, floor := e.trustPolicy.ResolveTrust(proj.ValidationConfig, quorum)

	// Collapse to distinct subjects, keeping each subject's max submission-time score (equal
	// per subject in practice; max is defensive). Reuses the transitioner's subject/score
	// fallbacks so accrual and the acceptance verdict apply identical rules.
	//
	// A non-countable result (submitter's effective standing at submit was not OK — see
	// transition.StandingCountable) is skipped, so it neither ACCRUES trust nor WITNESSES it:
	// its subject earns no +1, and its score never counts toward whether some OTHER subject had
	// a trusted witness. A probation account is invisible to trust just as it is to the verdict,
	// closing the loophole where a benched-then-agreeing account could seed trust outward.
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

	for subj, sc := range subjectScore {
		trustedOthers := trustedCount
		if sc >= floor {
			trustedOthers-- // a trusted subject cannot corroborate itself
		}
		if trustedOthers < 1 {
			continue
		}
		if err := e.trustRepo.AccrueCleanUnit(ctx, subj); err != nil {
			e.logger.Warn("failed to accrue trust for agreed subject",
				"subject", subj, "work_unit_id", wu.ID, "error", err)
		}
	}
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

// comparisonKey returns the EXACT-mode grouping key for a result. With no ignore_fields
// (or no inline output to canonicalize) it is the raw submitted checksum — identical to
// the historical behavior. With ignore_fields AND inline output present, it is a canonical
// SHA-256 over the output with those fields stripped and object keys sorted, so volatile
// provenance (e.g. a wall-clock compute_time_ms) no longer prevents agreement.
func comparisonKey(r *result.Result, ignoreFields []string) (string, error) {
	if len(ignoreFields) == 0 || len(r.OutputData) == 0 {
		return r.OutputChecksum, nil
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
