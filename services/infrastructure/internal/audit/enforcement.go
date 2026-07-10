package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// This file pins the slice-3 enforcement contracts (design doc §9). The EnforcementWorker
// is the leader-gated sweep that executes consequences on a confirmed audit MISMATCH:
// slash every agreeing trust subject, claw back the unit's credit plus all unmatured
// credit of those accounts (emitting revocation attestations), decrement RAC, flip the
// fraudulent results, retroactively repair honest dissenters, and dispose of the unit.
// Every step is idempotent or bookkeeping-guarded, so a crashed pass converges on re-run.

// EnforcementInterval is the sweep cadence. Enforcement latency is bounded by the credit
// maturation window (days), so the RevocationReconciler's 10 minutes is plenty.
const EnforcementInterval = 10 * time.Minute

// EnforcementBatchLimit bounds actionable roots examined per sweep pass.
const EnforcementBatchLimit = 20

// RepairRequest carries everything validation needs to adjudicate and repair a unit's
// DISAGREED results against ground truth (design doc §9.6, audit M3: candidate selection
// AND adjudication live INSIDE validation — this package cannot import the comparators).
type RepairRequest struct {
	RootAuditID types.ID
	WorkUnitID  types.ID
	// Snapshot is the ROOT audit's comparison_snapshot: validation-time semantics,
	// immune to later leaf-config edits.
	Snapshot ComparisonSnapshot
	// GroundTruths are the verbatim runner outputs the repair adjudicates against —
	// the root runner's bytes plus the confirming runner's bytes (a candidate matching
	// EITHER counts; the two agreed with each other in the §9.3 step-1 check).
	GroundTruths [][]byte
}

// RepairReport lists per-candidate outcomes for the pass's summary WARN and the
// ENFORCED bookkeeping.
type RepairReport struct {
	// Repaired is the number of DISAGREED results flipped AGREED + granted this pass.
	Repaired int
	// AlreadyRepaired counts candidates a prior pass had already repaired (idempotent
	// re-run convergence).
	AlreadyRepaired int
	// Skipped maps result id -> machine reason (REPAIR_INCONCLUSIVE, NO_MATCH,
	// REF_ONLY_NUMERIC, CANON_EMPTY, ...) for candidates not repaired.
	Skipped map[types.ID]string
	// AgreedAfter is the unit's AGREED-result count after repair — zero triggers the
	// §9.7 disposition (demote + refund + requeue).
	AgreedAfter int
}

// UnitRepairer is implemented by validation.Engine (wired in main.go — the adjudicator
// closure precedent): it selects the unit's DISAGREED candidates, adjudicates each
// against the ground truths under the snapshot semantics, and executes the full repair
// effects table (flip, grant, RAC, attestation, counter, standing, reliability, trust)
// with the audit_repairs claim guarding the non-idempotent effects.
type UnitRepairer interface {
	RepairUnit(ctx context.Context, req RepairRequest) (RepairReport, error)
}

// AgreementFunc adjudicates whether two verbatim runner outputs agree with each other
// under the snapshot semantics (design doc §9.3 step 1 — both sides are RAW runner
// bytes, a symmetric channel). Implemented by validation.AdjudicateGroundTruthAgreement;
// injected as a closure so this package never imports the comparators.
type AgreementFunc func(snap ComparisonSnapshot, a, b []byte) (bool, error)

// Slasher is the trust consequence seam (satisfied by trust.Repository.Slash).
type Slasher interface {
	Slash(ctx context.Context, subject string) error
}

// RevocationEmitter is the mandatory revocation seam (satisfied by
// attestation.RevocationEmitter — idempotency and the one-revocation-per-adjustment
// binding live there; ADDENDUM 12 pin c).
type RevocationEmitter interface {
	EmitForAdjustment(ctx context.Context, adjustmentID types.ID) error
}

// EnforcementAdjustment is the credit-side view of one clawback the worker needs for
// revocation emission and the RAC decrement (mirrors credit.Adjustment without the
// import — this package must stay import-light toward credit's consumers).
type EnforcementAdjustment struct {
	ID          types.ID
	VolunteerID types.ID
	LeafID      types.ID
	// Magnitude is the positive cancelled amount (-adjustment.amount).
	Magnitude float64
}

// CreditEnforcer is the settlement seam (satisfied by an adapter over the credit
// repositories, wired in main.go).
type CreditEnforcer interface {
	// ClawbackEntryForAudit full-cancels the ledger entry backing resultID (resolved
	// internally via GetByResultID), stamping created_by='AUDIT', the audit id, and the
	// given machine reason. Returns (nil, nil) when the result has no ledger entry
	// (cap-suppressed/legacy) or the entry is already fully adjusted (the F17
	// idempotent no-op).
	ClawbackEntryForAudit(ctx context.Context, resultID, auditID types.ID, reason string) (*EnforcementAdjustment, error)
	// ClawbackUnmaturedForAudit full-cancels every unmatured ledger entry of the
	// volunteer (granted_at > now() - maturationDays), same stamping; already-exhausted
	// entries no-op. Returns the adjustments actually written.
	ClawbackUnmaturedForAudit(ctx context.Context, volunteerID, auditID types.ID, maturationDays int, reason string) ([]*EnforcementAdjustment, error)
	// ApplyRACAdjustment applies the clamped RAC decrement for one adjustment
	// exactly-once (rac_applied_at stamp and RAC update in one transaction);
	// applied=false when a prior pass already applied it.
	ApplyRACAdjustment(ctx context.Context, adjustmentID types.ID) (applied bool, err error)
}

// FraudResult is one AGREED result on the mismatched unit (the sanction set).
type FraudResult struct {
	ResultID    types.ID
	VolunteerID types.ID
	// Subject is the submit-time stamped trust subject, with the vol:<uuid> sentinel
	// fallback for pre-00013 legacy rows.
	Subject string
}

// FraudSetLoader resolves the sanction set for a unit (satisfied by an adapter over the
// result repository, wired in main.go).
type FraudSetLoader interface {
	// LoadFraudSet returns the unit's AGREED results with their stamped subjects.
	LoadFraudSet(ctx context.Context, workUnitID types.ID) ([]FraudResult, error)
	// FlipToDisagreed marks the given results DISAGREED (idempotent re-run safe).
	FlipToDisagreed(ctx context.Context, resultIDs []types.ID) error
}

// UnitDisposer executes the §9.7 disposition when no result matched ground truth:
// demote VALIDATED→REJECTED, materialize the refunded copy budget, requeue (satisfied
// by an adapter over the workunit repository, wired in main.go).
type UnitDisposer interface {
	DemoteAndRequeue(ctx context.Context, workUnitID types.ID) error
}

// UnitLocker serializes an enforcement pass against transitioner activity on the same
// unit (best-effort belt — correctness rests on the guarded steps). Satisfied by the
// transition package's exported per-unit lock helper.
type UnitLocker interface {
	WithUnitLock(ctx context.Context, workUnitID types.ID, fn func() error) error
}

// EnforcementDeps wires the worker. All seams are narrow single-purpose interfaces so
// main.go composes them from the real repositories without import cycles.
type EnforcementDeps struct {
	Audits         AuditsRepository
	Slasher        Slasher
	Credit         CreditEnforcer
	Revocations    RevocationEmitter
	Results        FraudSetLoader
	Repairer       UnitRepairer
	Disposer       UnitDisposer
	Locker         UnitLocker
	Agreement      AgreementFunc
	MaturationDays int
	Logger         *slog.Logger
}

// EnforcementWorker is the leader-gated consequence sweep (design doc §9.2-§9.3).
// Started in main.go's leadership closure ONLY when the enforcement knob is on: with the
// knob off, verdicts stay observe-only byte-identically.
type EnforcementWorker struct {
	deps     EnforcementDeps
	interval time.Duration
}

// NewEnforcementWorker builds the sweep worker.
func NewEnforcementWorker(deps EnforcementDeps) *EnforcementWorker {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &EnforcementWorker{deps: deps, interval: EnforcementInterval}
}

// Start runs the sweep loop until ctx is cancelled (ReclaimWorker pattern: one sweep
// immediately on election, then on the ticker).
func (w *EnforcementWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}
