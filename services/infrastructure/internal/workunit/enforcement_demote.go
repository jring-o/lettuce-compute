package workunit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// DemoterRepo is the subset of the work-unit data layer the enforcement demoter needs,
// plus RefundCopyBudget (the §9.7 H3 budget rematerialization). It is deliberately
// NARROWER than WorkUnitRepository: RefundCopyBudget is a concrete method on
// *PgxWorkUnitRepository and is intentionally kept OFF the shared WorkUnitRepository
// interface, which is implemented in full by many packages' test fakes — widening it
// would force edits far outside this slice's scope. *PgxWorkUnitRepository satisfies
// this interface; the demoter's unit tests use a small fake that does too.
type DemoterRepo interface {
	GetByID(ctx context.Context, id types.ID) (*WorkUnit, error)
	// ExpireLiveCopies closes ALL live copies of a unit with the given outcome.
	ExpireLiveCopies(ctx context.Context, workUnitID types.ID, outcome string) (int, error)
	// UpdateState transitions from -> to with an optimistic WHERE state = from guard.
	UpdateState(ctx context.Context, id types.ID, from, to WorkUnitState) (*WorkUnit, error)
	// RefundCopyBudget materializes a fresh copy budget on top of everything already
	// consumed (audit H3), guarded WHERE state = 'REJECTED'. Returns whether a row moved.
	RefundCopyBudget(ctx context.Context, id types.ID, maxTotal, maxError int) (bool, error)
	// Reassign drives REJECTED -> QUEUED via TransitionToQueued.
	Reassign(ctx context.Context, id types.ID) (*WorkUnit, bool, error)
}

var _ DemoterRepo = (*PgxWorkUnitRepository)(nil)

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
// unit is QUEUED. Because the refund is an ABSOLUTE write keyed off the (stable, while
// REJECTED) copy count rather than an increment, re-running it on a resume pass is
// idempotent in value.
type EnforcementDemoter struct {
	repo DemoterRepo
	// resolveBudgets returns (freshTotalCeiling, resolvedErrorCeiling) for the unit —
	// the effective dead-letter ceilings a brand-new unit of this leaf would get.
	// Injected from main.go (the resolution walks unit override → leaf config →
	// derived default, which lives above this package).
	resolveBudgets func(ctx context.Context, wu *WorkUnit) (freshTotal int, errorCeiling int, err error)
	logger         *slog.Logger
}

// NewEnforcementDemoter wires the disposer.
func NewEnforcementDemoter(repo DemoterRepo, resolveBudgets func(ctx context.Context, wu *WorkUnit) (int, int, error), logger *slog.Logger) *EnforcementDemoter {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcementDemoter{repo: repo, resolveBudgets: resolveBudgets, logger: logger}
}

// DemoteAndRequeue runs the §9.7 disposition. Idempotent/resumable: an already-REJECTED
// unit resumes at the refund + requeue; an already-QUEUED unit is done.
//
// The pass only ever demotes VALIDATED units, so any other starting state (beyond the
// resumable QUEUED/REJECTED) is unexpected and errors — the enforcement sweep is the
// sole caller and it selects VALIDATED roots.
func (d *EnforcementDemoter) DemoteAndRequeue(ctx context.Context, workUnitID types.ID) error {
	wu, err := d.repo.GetByID(ctx, workUnitID)
	if err != nil {
		return err
	}

	switch wu.State {
	case WorkUnitStateQueued:
		// A prior pass already demoted + requeued this unit — nothing to do.
		return nil
	case WorkUnitStateRejected:
		// Resume: the demotion landed on an earlier (crashed) pass; the live copies
		// were already abandoned then and a REJECTED unit dispatches nothing, so there
		// are no fresh stragglers. Finish the refund + requeue below.
	case WorkUnitStateValidated:
		// Fresh disposition. Abandon any straggler live copies (best-effort, mirroring
		// the operator requeue handler's REJECTED/EXPIRED branch — handler.go:266), then
		// demote VALIDATED → REJECTED.
		_, _ = d.repo.ExpireLiveCopies(ctx, workUnitID, string(assignment.OutcomeAbandoned))
		if _, err := d.repo.UpdateState(ctx, workUnitID, WorkUnitStateValidated, WorkUnitStateRejected); err != nil {
			// The optimistic WHERE state = 'VALIDATED' guard missed: a concurrent pass
			// already moved the unit. Re-load and resume from wherever it now sits.
			if !isConflict(err) {
				return err
			}
			wu, err = d.repo.GetByID(ctx, workUnitID)
			if err != nil {
				return err
			}
			switch wu.State {
			case WorkUnitStateQueued:
				return nil
			case WorkUnitStateRejected:
				// fall through to refund + requeue
			default:
				return apierror.Internal(fmt.Sprintf(
					"enforcement demote: unit %s in unexpected state %s after demotion conflict",
					workUnitID, wu.State), nil)
			}
		}
	default:
		return apierror.Internal(fmt.Sprintf(
			"enforcement demote: unit %s in unexpected state %s (only VALIDATED units are demoted)",
			workUnitID, wu.State), nil)
	}

	// Budget refund (audit H3). resolveBudgets yields the fresh ceilings a brand-new unit
	// of this leaf would get; RefundCopyBudget materializes them ON TOP of everything
	// already consumed under a WHERE state = 'REJECTED' guard, so a failover double-sweep
	// cannot double-refund once the unit is QUEUED.
	freshTotal, errorCeiling, err := d.resolveBudgets(ctx, wu)
	if err != nil {
		return err
	}
	refunded, err := d.repo.RefundCopyBudget(ctx, workUnitID, freshTotal, errorCeiling)
	if err != nil {
		return err
	}
	if !refunded {
		// The unit is no longer REJECTED — a prior pass already refunded and is
		// requeuing (or has requeued) it. Treat as success; nothing more to do here.
		d.logger.InfoContext(ctx, "enforcement demote: unit already refunded/requeued by a prior pass",
			"work_unit_id", workUnitID)
		return nil
	}

	// Reassign REJECTED → QUEUED (existing primitive; clears validated_at, bumps priority
	// + reassignment_count). A concurrent pass may have requeued it between the refund and
	// here (Reassign 409s off a non-EXPIRED/REJECTED state) — treat that as done.
	_, requeued, err := d.repo.Reassign(ctx, workUnitID)
	if err != nil {
		if isConflict(err) {
			return nil
		}
		return err
	}

	d.logger.InfoContext(ctx, "enforcement demote: unit demoted and requeued",
		"work_unit_id", workUnitID,
		"fresh_total_copies", freshTotal,
		"error_ceiling", errorCeiling,
		"requeued", requeued,
	)
	return nil
}

// isConflict reports whether err is an apierror carrying a 409 Conflict — the optimistic
// state-guard signal from UpdateState / Reassign that a concurrent pass moved the unit.
func isConflict(err error) bool {
	var apiErr *apierror.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatus == 409
	}
	return false
}
