package validation

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/types"
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
// jsonb-normalization boundary. raw-EXACT: sha256 equality; canon-EXACT: both sides
// flattened under snapshot ignore_fields, numericMatch(epsilon = 0); NUMERIC: within the
// snapshot tolerance. Unparseable bytes where a value compare is required yield an
// error — the caller treats that as non-agreement without fabricating agreement.
//
// KEEL STUB — the validation implementer replaces the body (assignable to
// audit.AgreementFunc).
func AdjudicateGroundTruthAgreement(snap audit.ComparisonSnapshot, a, b []byte) (bool, error) {
	return false, apierror.Internal("AdjudicateGroundTruthAgreement not implemented (slice-3 keel stub)", nil)
}

// RepairUnit adjudicates the unit's DISAGREED results against the ground truths and
// executes the full repair-effects table per match (design doc §9.6): flip AGREED,
// grant the unit's per-result amount (Conflict = already granted = no-op; routed through
// the emission cap when configured), RAC upsert on a fresh grant, a NEW AGREED v2
// attestation (unique-index no-op on re-run), and — guarded by the audit_repairs claim —
// IncrementWorkUnitsCompleted, RecordAdjudicated(true), RecordOutcome(hostKey, true),
// and AccrueCleanUnit iff transition.StandingCountable(result) (audit M5). Candidate
// selection and every comparison live HERE (audit M3 — the worker cannot adjudicate).
//
// KEEL STUB — the validation implementer replaces the body (Engine satisfies
// audit.UnitRepairer).
func (e *Engine) RepairUnit(ctx context.Context, req audit.RepairRequest) (audit.RepairReport, error) {
	return audit.RepairReport{}, apierror.Internal("RepairUnit not implemented (slice-3 keel stub)", nil)
}
