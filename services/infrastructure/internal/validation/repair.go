package validation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// This file is the slice-3 retroactive-repair seam (design doc §9.6). The enforcement
// worker (internal/audit) cannot import this package, so it consumes RepairUnit through
// the audit.UnitRepairer interface and AdjudicateGroundTruthAgreement through the
// audit.AgreementFunc closure — both wired in main.go, the AdjudicateAudit precedent.

// RepairClaimer inserts the audit_repairs idempotency claim guarding the NON-idempotent
// repair effects (trust accrual, reliability/standing compensation, counter bump): one
// repair per result, ever. Satisfied by the audits repository; injected via
// WithRepairSupport.
type RepairClaimer interface {
	ClaimRepair(ctx context.Context, auditID, resultID types.ID) (claimed bool, err error)
}

// WithRepairSupport wires the repair claim seam (constructor option, matching the
// engine's other With* setters).
func (e *Engine) WithRepairSupport(rc RepairClaimer) *Engine {
	e.repairClaimer = rc
	return e
}

// AdjudicateGroundTruthAgreement reports whether two VERBATIM runner outputs agree with
// each other under the snapshot semantics (design doc §9.3 step 1 — the Q1-B mutual
// ground-truth check). Both sides are RAW runner bytes: a SYMMETRIC channel with no
// jsonb-normalization boundary. It dispatches on the SNAPSHOT's comparison mode (unlike
// AdjudicateAudit, which dispatches on a stored accepted key's shape — there is no stored
// key on either side here):
//
//   - EXACT with NO effective ignore_fields → raw sha256 equality. Adjudicable even for
//     non-JSON runner bytes.
//   - EXACT WITH effective ignore_fields (the same predicate comparisonKey uses to decide
//     canon: ignore_fields present, and both sides always carry inline bytes here) →
//     flatten BOTH sides under the snapshot ignore/compare fields and compare value-level
//     with numericMatch(epsilon = 0). Value-level, never key-string: the F-H3 exponent-token
//     hazard cannot bite on a symmetric raw channel, but the value compare is the honest
//     semantics and stays consistent with the canon audit path.
//   - NUMERIC_TOLERANCE → flatten both under the snapshot ignore/compare fields and compare
//     with numericMatch(snapshot epsilon).
//
// Unparseable bytes where a value compare is required yield (false, error): the caller treats
// that as non-agreement (evidence of non-determinism) without fabricating agreement. It is
// assignable to audit.AgreementFunc.
func AdjudicateGroundTruthAgreement(snap audit.ComparisonSnapshot, a, b []byte) (bool, error) {
	switch snap.ComparisonMode {
	case leaf.ComparisonExact:
		// "Effective canon" mirrors comparisonKey's predicate: canon applies when
		// ignore_fields strip volatile provenance (both sides carry bytes by construction,
		// so the inline-bytes half of the predicate is always satisfied here). With no
		// ignore_fields the channel is a raw byte compare — the historical EXACT semantics.
		if len(snap.IgnoreFields) == 0 {
			return sha256.Sum256(a) == sha256.Sum256(b), nil
		}
		fa, err := flattenOutput(json.RawMessage(a), snap.IgnoreFields, snap.CompareFields)
		if err != nil {
			return false, fmt.Errorf("flatten side A: %w", err)
		}
		fb, err := flattenOutput(json.RawMessage(b), snap.IgnoreFields, snap.CompareFields)
		if err != nil {
			return false, fmt.Errorf("flatten side B: %w", err)
		}
		return numericMatch(fa, fb, 0), nil
	case leaf.ComparisonNumericTolerance:
		fa, err := flattenOutput(json.RawMessage(a), snap.IgnoreFields, snap.CompareFields)
		if err != nil {
			return false, fmt.Errorf("flatten side A: %w", err)
		}
		fb, err := flattenOutput(json.RawMessage(b), snap.IgnoreFields, snap.CompareFields)
		if err != nil {
			return false, fmt.Errorf("flatten side B: %w", err)
		}
		return numericMatch(fa, fb, snap.NumericTolerance), nil
	default:
		// CUSTOM is unsampleable and therefore never audited, so a ground-truth check can
		// never carry it; fail closed rather than fabricate agreement on an unknown mode.
		return false, apierror.Internal("unsupported comparison mode for ground-truth agreement: "+snap.ComparisonMode, nil)
	}
}

// Repair skip reasons recorded in RepairReport.Skipped (machine codes, §9.6).
const (
	repairSkipInconclusive = "REPAIR_INCONCLUSIVE"
	repairSkipNoMatch      = "NO_MATCH"
	repairSkipRefOnly      = "REF_ONLY_NUMERIC"
	repairSkipCanonEmpty   = "CANON_EMPTY"
)

// RepairUnit adjudicates the unit's DISAGREED results against the ground truths and
// executes the full repair-effects table per match (design doc §9.6): flip AGREED, grant
// the unit's per-result amount (Conflict = already granted = no-op; routed through the
// emission cap when configured), RAC upsert on a fresh grant, a NEW AGREED v2 attestation
// (unique-index no-op on re-run), and — guarded by the audit_repairs claim —
// IncrementWorkUnitsCompleted, RecordAdjudicated(true), RecordOutcome(hostKey, true), and
// AccrueCleanUnit iff transition.StandingCountable(result) (audit M5). Candidate selection
// and every comparison live HERE (audit M3 — the worker cannot adjudicate).
//
// The flip/grant/attestation are idempotent by their own constraints and run every pass; the
// non-idempotent trio + counter run ONLY when this pass wins the per-result audit_repairs
// claim, so a crashed pass converges on re-run (a claimed==false candidate contributes to
// AlreadyRepaired and fires no reputational effect twice).
func (e *Engine) RepairUnit(ctx context.Context, req audit.RepairRequest) (audit.RepairReport, error) {
	report := audit.RepairReport{Skipped: make(map[types.ID]string)}

	// Guard: the claim seam gates the non-idempotent effects; unwired, RepairUnit must not
	// silently apply them without the one-repair-per-result claim.
	if e.repairClaimer == nil {
		return report, apierror.Internal("RepairUnit called without a repair claimer wired (WithRepairSupport)", nil)
	}

	wu, err := e.workUnitRepo.GetByID(ctx, req.WorkUnitID)
	if err != nil {
		return report, fmt.Errorf("load work unit %s: %w", req.WorkUnitID, err)
	}
	proj, err := e.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		return report, fmt.Errorf("load leaf %s: %w", wu.LeafID, err)
	}
	allResults, err := e.resultRepo.ListByWorkUnit(ctx, req.WorkUnitID)
	if err != nil {
		return report, fmt.Errorf("list results for work unit %s: %w", req.WorkUnitID, err)
	}

	// The quorum descriptor's DEMANDED side is the resolved policy (immune to later leaf
	// edits within this pass); its DELIVERED side is the post-repair truth, rebuilt per match.
	policy := transition.ResolvePolicyWithTrust(proj, wu, e.trustPolicy)

	// The per-result grant amount is uniform per unit and immutable under adjustment
	// (§9.6): read it lazily from any existing ledger entry (the clawed fraud-set entries
	// persist append-only with their original credit_amount), falling back to the leaf's
	// derivation only if the unit never granted (e.g. every grant was cap-suppressed).
	grantAmount := 0.0
	grantResolved := false
	resolveGrantAmount := func() float64 {
		if grantResolved {
			return grantAmount
		}
		grantResolved = true
		for _, r := range allResults {
			entry, gErr := e.creditRepo.GetByResultID(ctx, r.ID)
			if gErr == nil && entry != nil {
				grantAmount = entry.CreditAmount
				return grantAmount
			}
		}
		grantAmount = proj.CreditConfig.CreditPerValidatedWorkUnit
		if grantAmount <= 0 {
			grantAmount = 1.0
		}
		e.logger.Warn("repair grant amount falling back to leaf credit config (no prior ledger entry on unit)",
			"work_unit_id", wu.ID, "leaf_id", wu.LeafID, "amount", grantAmount)
		return grantAmount
	}

	// repaired accumulates the candidates flipped AGREED this pass; it is the post-repair
	// AGREED (majority) set fed to the attestation descriptor as each match is applied.
	var repaired []*result.Result

	for _, cand := range allResults {
		if cand.ValidationStatus != result.ValidationDisagreed {
			continue // only DISAGREED results are repair candidates
		}

		candidateKey, acceptedOutputs, skip := e.repairCandidateKey(cand, req.Snapshot)
		if skip != "" {
			report.Skipped[cand.ID] = skip
			continue
		}

		// A candidate matches ground truth iff it agrees with EITHER runner's bytes (the two
		// agreed with each other in §9.3 step 1); INCONCLUSIVE on all → REPAIR_INCONCLUSIVE
		// (never fabricate a repair), MISMATCH on all → NO_MATCH.
		matched := false
		sawInconclusive := false
		for _, gt := range req.GroundTruths {
			v, _, _ := AdjudicateAudit(req.Snapshot, candidateKey, acceptedOutputs, gt)
			switch v {
			case audit.VerdictMatch:
				matched = true
			case audit.VerdictInconclusive:
				sawInconclusive = true
			}
			if matched {
				break
			}
		}
		if !matched {
			if sawInconclusive {
				report.Skipped[cand.ID] = repairSkipInconclusive
			} else {
				report.Skipped[cand.ID] = repairSkipNoMatch
			}
			continue
		}

		if err := e.applyRepair(ctx, req.RootAuditID, wu, proj, cand, allResults, &repaired, policy, resolveGrantAmount, &report); err != nil {
			// An infrastructure error (not an adjudication skip) aborts the pass; the sweep
			// retries and every applied effect is idempotent or claim-guarded, so re-running
			// converges without double-application.
			return report, err
		}
	}

	// AgreedAfter drives the §9.7 disposition (empty => demote + requeue). Reloaded so it is
	// correct in production, where BatchUpdateValidationStatus writes the DB rather than the
	// in-memory result structs.
	finalResults, err := e.resultRepo.ListByWorkUnit(ctx, req.WorkUnitID)
	if err != nil {
		return report, fmt.Errorf("reload results for work unit %s: %w", req.WorkUnitID, err)
	}
	for _, r := range finalResults {
		if r.ValidationStatus == result.ValidationAgreed {
			report.AgreedAfter++
		}
	}
	return report, nil
}

// repairCandidateKey builds the candidate's comparison key + accepted-output side under the
// snapshot, following comparisonKey's rules EXACTLY (this package owns the comparator, so it
// calls comparisonKey directly rather than through the worker-facing exported wrapper):
// raw stored checksum when there is no effective canon, the canonical form when ignore_fields
// strip and inline bytes exist, and the empty-key/value-level form for NUMERIC. A non-empty
// skip code means the candidate is unadjudicable and is recorded in RepairReport.Skipped.
func (e *Engine) repairCandidateKey(cand *result.Result, snap audit.ComparisonSnapshot) (key string, acceptedOutputs []json.RawMessage, skip string) {
	switch snap.ComparisonMode {
	case leaf.ComparisonNumericTolerance:
		// NUMERIC is value-level (empty key); a ref-only candidate has no inline bytes to
		// flatten, mirroring the §7.2 sampling-eligibility filter.
		if len(cand.OutputData) == 0 {
			return "", nil, repairSkipRefOnly
		}
		return "", []json.RawMessage{json.RawMessage(cand.OutputData)}, ""
	case leaf.ComparisonExact:
		k, err := comparisonKey(cand, snap.IgnoreFields)
		if err != nil {
			// Unparseable candidate output where the canon key needs to parse it — never a
			// fabricated repair (§7.13 channel rule inherited).
			e.logger.Warn("repair: candidate output unparseable for canon key; skipping",
				"result_id", cand.ID, "error", err)
			return "", nil, repairSkipInconclusive
		}
		if strings.HasPrefix(k, "canon-empty:") {
			// ignore_fields stripped every comparable leaf — unadjudicable against runner
			// bytes by construction (F-M2 mirror).
			return "", nil, repairSkipCanonEmpty
		}
		return k, []json.RawMessage{json.RawMessage(cand.OutputData)}, ""
	default:
		// CUSTOM / unknown: unadjudicable (never sampled), so never repaired.
		return "", nil, repairSkipInconclusive
	}
}

// applyRepair executes the §9.6 effects for one MATCHED candidate, in order: flip AGREED,
// grant, RAC (on a fresh insert), a NEW AGREED v2 attestation, then the audit_repairs claim
// guarding the reputational trio + counter. It returns an error only on an infrastructure
// failure (which aborts the pass for re-run); an already-granted Conflict and a cap
// suppression are non-error branches.
func (e *Engine) applyRepair(
	ctx context.Context,
	rootAuditID types.ID,
	wu *workunit.WorkUnit,
	proj *leaf.Leaf,
	cand *result.Result,
	allResults []*result.Result,
	repaired *[]*result.Result,
	policy transition.RedundancyPolicy,
	resolveGrantAmount func() float64,
	report *audit.RepairReport,
) error {
	// (1) Flip AGREED (idempotent — re-running sets AGREED again).
	if err := e.resultRepo.BatchUpdateValidationStatus(ctx, []types.ID{cand.ID}, result.ValidationAgreed); err != nil {
		return fmt.Errorf("flip result %s AGREED: %w", cand.ID, err)
	}
	*repaired = append(*repaired, cand)

	// (2) Grant the unit's per-result amount. Route through the emission cap when configured
	// (F10 consistency); a typed Conflict on uq_credit_ledger_result means a prior pass
	// already granted (no-op); a cap suppression grants nothing (attestation credit 0).
	amount := resolveGrantAmount()
	entry := &credit.LedgerEntry{
		VolunteerID:  cand.VolunteerID,
		LeafID:       wu.LeafID,
		WorkUnitID:   wu.ID,
		ResultID:     cand.ID,
		CreditAmount: amount,
	}
	inserted := false
	actualGranted := 0.0
	if cc, capEnforced := e.cappedCreator(); capEnforced {
		ins, err := cc.CreateCapped(ctx, entry, e.emissionCapPerDay)
		if err != nil {
			if !isAlreadyGrantedConflict(err) {
				return fmt.Errorf("repair grant (capped) for result %s: %w", cand.ID, err)
			}
			// already granted — no-op
		} else if ins {
			inserted = true
			actualGranted = amount
		} else {
			e.logger.Warn("repair credit suppressed by daily emission cap",
				"work_unit_id", wu.ID, "result_id", cand.ID, "volunteer_id", cand.VolunteerID,
				"amount", amount, "cap", e.emissionCapPerDay)
		}
	} else if err := e.creditRepo.Create(ctx, entry); err != nil {
		if !isAlreadyGrantedConflict(err) {
			return fmt.Errorf("repair grant for result %s: %w", cand.ID, err)
		}
		// already granted — no-op
	} else {
		inserted = true
		actualGranted = amount
	}

	// (3) RAC upsert iff the ledger insert HAPPENED this pass (grant-path parity). Best-effort.
	if inserted && e.racRepo != nil {
		if err := e.racRepo.Upsert(ctx, cand.VolunteerID, wu.LeafID, amount); err != nil {
			e.logger.Warn("repair: failed to update RAC",
				"volunteer_id", cand.VolunteerID, "leaf_id", wu.LeafID, "result_id", cand.ID, "error", err)
		}
	}

	// (4) NEW AGREED v2 attestation with the post-repair truth in the descriptor. The DELIVERED
	// counts come from the corrected AGREED set (repaired-so-far, this candidate included);
	// createAttestations logs-and-skips a uq_attestations_result_agreed conflict on re-run.
	verdict := transition.BuildComparisonVerdict(allResults, *repaired, policy.TrustFloor)
	desc := e.buildQuorumDescriptor(proj, verdict, policy)
	e.createAttestations(ctx, wu, []*result.Result{cand}, attestation.OutcomeAgreed,
		map[types.ID]float64{cand.ID: actualGranted}, desc)

	// (5) The audit_repairs claim guards the NON-idempotent effects: only the pass that wins
	// the one-repair-per-result claim fires the reputational trio + counter (+ trust, gated by
	// the standing rule). claimed==false means a prior pass already repaired this result.
	claimed, err := e.repairClaimer.ClaimRepair(ctx, rootAuditID, cand.ID)
	if err != nil {
		return fmt.Errorf("claim repair for result %s: %w", cand.ID, err)
	}
	if !claimed {
		report.AlreadyRepaired++
		return nil
	}

	// Counter (lifetime work; total_work_units_rejected is NOT decremented — monotonic pin).
	if err := e.volunteerRepo.IncrementWorkUnitsCompleted(ctx, cand.VolunteerID); err != nil {
		e.logger.Warn("repair: failed to increment work units completed",
			"volunteer_id", cand.VolunteerID, "result_id", cand.ID, "error", err)
	}
	// Standing: compensate the original false ding directionally (decayed accumulator).
	e.recordAdjudicated(ctx, cand.VolunteerID, true)
	// Reliability: same host-keying rule as the engine (HostID else VolunteerID).
	e.recordReliability(ctx, []*result.Result{cand}, true)
	// Trust: accrue ONLY for a standing-countable result (audit M5 — the honest path skips
	// non-countable submissions). The D9 witness gate is satisfied by construction (an active
	// trusted runner corroborated the ground truth). Best-effort, nil-safe.
	if e.trustRepo != nil && transition.StandingCountable(cand) {
		subj := transition.SubjectForResult(cand)
		if err := e.trustRepo.AccrueCleanUnit(ctx, subj); err != nil {
			e.logger.Warn("repair: failed to accrue trust for repaired subject",
				"subject", subj, "result_id", cand.ID, "error", err)
		}
	}

	report.Repaired++
	return nil
}

// isAlreadyGrantedConflict reports whether err is the typed 409 Conflict the credit repo
// returns on the uq_credit_ledger_result duplicate (23505) — the "already granted" idempotent
// no-op for the repair grant, distinct from a real DB failure.
func isAlreadyGrantedConflict(err error) bool {
	var apiErr *apierror.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "CONFLICT"
}
