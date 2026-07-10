package workunit

import (
	"context"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// EnforcementDemoter executes the §9.7 disposition for a unit whose accepted output was
// refuted by trusted re-execution with nothing repairable: abandon stragglers, demote
// VALIDATED→REJECTED, materialize a refunded copy budget, requeue. It satisfies the
// enforcement worker's UnitDisposer seam (wired in main.go).
//
// Budget refund (audit H3 — a bare += on the default-0 max_total_copies column would
// MATERIALIZE an absolute ceiling smaller than the copies already consumed and the
// requeued unit would dead-letter before drawing one fresh copy): the demoter writes
// max_total_copies = CountTotalCopies(unit) + <a full fresh effective ceiling>, and
// max_error_copies = CountErrorCopies(unit) + <resolved ceiling> ONLY when the resolved
// error ceiling is non-zero (0 = unlimited stays 0). The refund UPDATE is guarded
// WHERE state = 'REJECTED', so a failover double-sweep cannot double-refund once the
// unit is QUEUED.
type EnforcementDemoter struct {
	repo WorkUnitRepository
	// resolveBudgets returns (freshTotalCeiling, resolvedErrorCeiling) for the unit —
	// the effective dead-letter ceilings a brand-new unit of this leaf would get.
	// Injected from main.go (the resolution walks unit override → leaf config →
	// derived default, which lives above this package).
	resolveBudgets func(ctx context.Context, wu *WorkUnit) (freshTotal int, errorCeiling int, err error)
	logger         *slog.Logger
}

// NewEnforcementDemoter wires the disposer.
func NewEnforcementDemoter(repo WorkUnitRepository, resolveBudgets func(ctx context.Context, wu *WorkUnit) (int, int, error), logger *slog.Logger) *EnforcementDemoter {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcementDemoter{repo: repo, resolveBudgets: resolveBudgets, logger: logger}
}

// DemoteAndRequeue runs the disposition. Idempotent/resumable: an already-REJECTED unit
// resumes at the refund + requeue; an already-QUEUED unit is done.
//
// KEEL STUB — the workunit implementer replaces the body.
func (d *EnforcementDemoter) DemoteAndRequeue(ctx context.Context, workUnitID types.ID) error {
	return apierror.Internal("DemoteAndRequeue not implemented (slice-3 keel stub)", nil)
}
