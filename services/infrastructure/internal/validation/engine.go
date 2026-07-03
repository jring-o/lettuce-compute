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

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/result"
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
	signer          *attestation.Signer
	logger          *slog.Logger
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
		signer:          signer,
		logger:          logger,
	}
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
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup, len(majorityGroup))
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
	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup, len(majorityGroup))
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
func (e *Engine) applyThreshold(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result, majorityCount int) (*ValidationResult, error) {
	total := len(pending)
	threshold := proj.ValidationConfig.AgreementThreshold
	ratio := float64(majorityCount) / float64(total)

	// A unit VALIDATES only if all three gates hold, mirroring transition.Decide (the
	// production decider) so the two paths never disagree:
	//   (1) ratio >= threshold          — the configured agreement fraction, and
	//   (2) majorityCount >= min_quorum  — the agreeing group is itself quorum-sized (the
	//                                      floor is on the WINNERS, not just an attempt gate),
	//   (3) 2*majorityCount > total      — the agreeing group is a STRICT majority of the
	//                                      compared set, so no config can validate a minority
	//                                      or plurality.
	// min_quorum resolves as in transition.ResolvePolicy (spot-check forces a 2-of-2
	// corroboration). A tie leaves majorityCount == 0, which fails (2) and (3), so an
	// ambiguous largest group can never validate.
	quorum := proj.ValidationConfig.EffectiveMinQuorum()
	if wu.SpotCheck {
		quorum = 2
	}

	if majorityCount >= quorum && 2*majorityCount > total && ratio >= threshold {
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
			"majority", majorityCount,
			"total", total,
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

	var creditEntries []*credit.LedgerEntry
	for _, r := range agreedResults {
		entry := &credit.LedgerEntry{
			VolunteerID:  r.VolunteerID,
			LeafID:       wu.LeafID,
			WorkUnitID:   wu.ID,
			ResultID:     r.ID,
			CreditAmount: creditAmount,
		}
		if err := e.creditRepo.Create(ctx, entry); err != nil {
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
	}

	// Create attestations for agreed results.
	e.createAttestations(ctx, wu, agreedResults, attestation.OutcomeAgreed, creditAmount)

	// Create attestations for disagreed results (credit_amount = 0).
	e.createAttestations(ctx, wu, rejectedResults, attestation.OutcomeDisagreed, 0)

	// Update volunteer counters. H-7: best-effort counter bumps — a failure does not
	// fail validation, so log at Warn, not Error.
	for _, r := range agreedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsCompleted(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units completed",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
	}
	for _, r := range rejectedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
		e.checkRejectionRate(ctx, r.VolunteerID)
	}

	// TODO #54: feed the per-host reliability signal — an AGREED result is a good outcome
	// for the machine that produced it (grows its buffer), a DISAGREED one is wasted work
	// (shrinks it). Best-effort, after credit is already granted (never fails validation).
	e.recordReliability(ctx, agreedResults, true)
	e.recordReliability(ctx, rejectedResults, false)

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

	// Create attestations for all rejected results (credit_amount = 0).
	e.createAttestations(ctx, wu, pending, attestation.OutcomeDisagreed, 0)

	// Update volunteer counters. H-7: best-effort — a failure does not fail the
	// rejection, so log at Warn, not Error.
	for _, r := range pending {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Warn("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "result_id", r.ID, "error", err)
		}
		e.checkRejectionRate(ctx, r.VolunteerID)
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

// createAttestations creates signed credit attestations for each result.
func (e *Engine) createAttestations(ctx context.Context, wu *workunit.WorkUnit, results []*result.Result, outcome string, creditAmount float64) {
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
			CreditAmount:         creditAmount,
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

// checkRejectionRate logs a warning if a volunteer's rejection rate exceeds 20%.
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
	canon, err := canonicalizeJSON(r.OutputData, ignoreFields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "canon:" + hex.EncodeToString(sum[:]), nil
}

// canonicalizeJSON parses output JSON, strips ignoreFields, and re-marshals
// deterministically (json.Marshal sorts object keys and emits json.Number verbatim, so
// numeric tokens and key order are normalized identically for every result).
func canonicalizeJSON(data json.RawMessage, ignoreFields []string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse output as JSON: %w", err)
	}
	return json.Marshal(stripFields(v, "", ignoreFields))
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
