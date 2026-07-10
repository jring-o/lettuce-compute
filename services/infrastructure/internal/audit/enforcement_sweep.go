package audit

import (
	"context"
)

// sweep runs one enforcement pass: select actionable roots (eligible MISMATCH originals
// in NONE/AWAITING_CONFIRMATION, oldest completed first, EnforcementBatchLimit), and run
// each through enforceRoot. Implementation lands with the enforcement implementer
// (design doc §9.3); the keel pins the surrounding contracts.
func (w *EnforcementWorker) sweep(ctx context.Context) {
	roots, err := w.deps.Audits.ListActionableRoots(ctx, EnforcementBatchLimit)
	if err != nil {
		w.deps.Logger.Error("enforcement sweep: failed to list actionable roots", "error", err)
		return
	}
	for _, root := range roots {
		if ctx.Err() != nil {
			return
		}
		if err := w.enforceRoot(ctx, root); err != nil {
			w.deps.Logger.Warn("enforcement pass failed; will retry next sweep",
				"audit_id", root.ID, "work_unit_id", root.WorkUnitID, "error", err)
		}
	}
}

// enforceRoot runs the ordered §9.3 pass for one actionable root. STUB — replaced by the
// enforcement implementer; the keel ships it inert so the package compiles.
func (w *EnforcementWorker) enforceRoot(ctx context.Context, root *Audit) error {
	_ = ctx
	w.deps.Logger.Warn("enforcement pass not yet implemented", "audit_id", root.ID)
	return nil
}
