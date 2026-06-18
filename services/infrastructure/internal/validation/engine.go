package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
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
	leafRepo     leaf.Repository
	creditRepo      credit.Repository
	racRepo         credit.RACRepository
	volunteerRepo   volunteer.Repository
	assignmentRepo  assignment.Repository
	attestationRepo attestation.Repository
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
	signer *attestation.Signer,
	logger *slog.Logger,
) *Engine {
	return &Engine{
		resultRepo:      resultRepo,
		workUnitRepo:    workUnitRepo,
		leafRepo:     leafRepo,
		creditRepo:      creditRepo,
		racRepo:         racRepo,
		volunteerRepo:   volunteerRepo,
		assignmentRepo:  assignmentRepo,
		attestationRepo: attestationRepo,
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
	// Group results by checksum.
	groups := make(map[string][]*result.Result)
	for _, r := range pending {
		groups[r.OutputChecksum] = append(groups[r.OutputChecksum], r)
	}

	// Find the largest group (majority).
	// When groups are tied in size, pick the one with the lexicographically
	// smallest checksum so the outcome is deterministic regardless of map
	// iteration order.
	var majorityChecksum string
	var majorityCount int
	for checksum, group := range groups {
		if len(group) > majorityCount ||
			(len(group) == majorityCount && (majorityCount == 0 || checksum < majorityChecksum)) {
			majorityCount = len(group)
			majorityChecksum = checksum
		}
	}

	return e.applyThreshold(ctx, wu, proj, pending, groups[majorityChecksum], majorityCount)
}

// validateNumericTolerance compares numeric output data within epsilon tolerance.
func (e *Engine) validateNumericTolerance(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result) (*ValidationResult, error) {
	epsilon := float64(0)
	if proj.ValidationConfig.NumericTolerance != nil {
		epsilon = *proj.ValidationConfig.NumericTolerance
	}

	// Parse all result output data as map[string]float64.
	parsed := make([]map[string]float64, len(pending))
	for i, r := range pending {
		m, err := parseNumericOutput(r.OutputData)
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

	return e.applyThreshold(ctx, wu, proj, pending, majorityGroup, len(clique))
}

// applyThreshold applies the agreement threshold and performs the validation outcome.
func (e *Engine) applyThreshold(ctx context.Context, wu *workunit.WorkUnit, proj *leaf.Leaf, pending []*result.Result, majorityGroup []*result.Result, majorityCount int) (*ValidationResult, error) {
	total := len(pending)
	threshold := proj.ValidationConfig.AgreementThreshold
	ratio := float64(majorityCount) / float64(total)

	if ratio >= threshold {
		return e.acceptResults(ctx, wu, proj, pending, majorityGroup)
	}

	// Agreement threshold not met. Check if there are still active assignments.
	activeCount, err := e.assignmentRepo.CountActiveByWorkUnit(ctx, wu.ID)
	if err != nil {
		return nil, fmt.Errorf("count active assignments: %w", err)
	}

	if activeCount > 0 {
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
			LeafID:    wu.LeafID,
			WorkUnitID:   wu.ID,
			ResultID:     r.ID,
			CreditAmount: creditAmount,
		}
		if err := e.creditRepo.Create(ctx, entry); err != nil {
			return nil, fmt.Errorf("create credit entry for result %s: %w", r.ID, err)
		}
		// Update RAC for this volunteer+project.
		if e.racRepo != nil {
			if err := e.racRepo.Upsert(ctx, r.VolunteerID, wu.LeafID, creditAmount); err != nil {
				e.logger.Error("failed to update RAC",
					"volunteer_id", r.VolunteerID, "leaf_id", wu.LeafID, "error", err)
			}
		}
		creditEntries = append(creditEntries, entry)
	}

	// Create attestations for agreed results.
	e.createAttestations(ctx, wu, agreedResults, attestation.OutcomeAgreed, creditAmount)

	// Create attestations for disagreed results (credit_amount = 0).
	e.createAttestations(ctx, wu, rejectedResults, attestation.OutcomeDisagreed, 0)

	// Update volunteer counters.
	for _, r := range agreedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsCompleted(ctx, r.VolunteerID); err != nil {
			e.logger.Error("failed to increment work units completed",
				"volunteer_id", r.VolunteerID, "error", err)
		}
	}
	for _, r := range rejectedResults {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Error("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "error", err)
		}
		e.checkRejectionRate(ctx, r.VolunteerID)
	}

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

	// Update volunteer counters.
	for _, r := range pending {
		if err := e.volunteerRepo.IncrementWorkUnitsRejected(ctx, r.VolunteerID); err != nil {
			e.logger.Error("failed to increment work units rejected",
				"volunteer_id", r.VolunteerID, "error", err)
		}
		e.checkRejectionRate(ctx, r.VolunteerID)
	}

	// Log spot-check mismatch.
	if wu.SpotCheck {
		volIDs := make([]string, len(pending))
		for i, r := range pending {
			volIDs[i] = r.VolunteerID.String()
		}
		e.logger.Warn("spot-check mismatch: volunteers disagreed",
			"work_unit_id", wu.ID,
			"volunteers", volIDs,
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
				"volunteer_id", r.VolunteerID, "error", err)
			continue
		}

		// Convert execution metadata to map[string]any for raw_metrics.
		rawMetrics := executionMetadataToMap(r.ExecutionMetadata)

		att := &attestation.Attestation{
			LeafID:           wu.LeafID,
			VolunteerPublicKey:  vol.PublicKey,
			WorkUnitID:          wu.ID,
			RawMetrics:          rawMetrics,
			ValidationOutcome:   outcome,
			CreditAmount:        creditAmount,
			AttestationTimestamp: now,
		}

		sig, err := e.signer.Sign(att)
		if err != nil {
			e.logger.Error("failed to sign attestation",
				"work_unit_id", wu.ID, "volunteer_id", r.VolunteerID, "error", err)
			continue
		}
		att.Signature = sig

		if err := e.attestationRepo.Create(ctx, att); err != nil {
			e.logger.Error("failed to create attestation",
				"work_unit_id", wu.ID, "volunteer_id", r.VolunteerID, "error", err)
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

// parseNumericOutput parses JSON output data as a flat map of string keys to float64 values.
//
// Non-finite values (NaN, +Inf, -Inf) are rejected as invalid: a result whose
// numeric output contains any non-finite value is treated identically to a
// malformed/unparseable output (an error is returned). This is a security
// safeguard — comparing non-finite values with math.Abs(va-vb) > epsilon yields
// false for NaN, which would otherwise let two such results be judged "matching"
// and reach quorum. Note that JSON like 1e400 unmarshals to ±Inf without a parse
// error, so this finiteness check (not json.Unmarshal alone) is what catches it.
func parseNumericOutput(data json.RawMessage) (map[string]float64, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty output data")
	}
	var m map[string]float64
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal numeric output: %w", err)
	}
	for key, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("non-finite numeric output for key %q: %v", key, v)
		}
	}
	return m, nil
}

// numericMatch returns true if all shared keys have values within epsilon.
func numericMatch(a, b map[string]float64, epsilon float64) bool {
	// Both must have the same keys.
	if len(a) != len(b) {
		return false
	}
	for key, va := range a {
		vb, ok := b[key]
		if !ok {
			return false
		}
		// Defense in depth: never treat non-finite values as matching. NaN
		// comparisons (e.g. math.Abs(NaN-NaN) > epsilon) evaluate to false,
		// which would otherwise incorrectly mark the pair as compatible. Parse
		// rejection should already exclude these, but guard here regardless.
		if math.IsNaN(va) || math.IsInf(va, 0) || math.IsNaN(vb) || math.IsInf(vb, 0) {
			return false
		}
		if math.Abs(va-vb) > epsilon {
			return false
		}
	}
	return true
}

// findLargestClique finds the largest subset of nodes where all pairs are mutually compatible.
// Uses brute force enumeration — suitable for small N (≤ 5, which is the max redundancy factor).
func findLargestClique(n int, compatible [][]bool) []int {
	var bestClique []int

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

		if allCompat && len(members) > len(bestClique) {
			bestClique = members
		}
	}

	return bestClique
}
