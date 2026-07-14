package transition

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// RecoveryStore is the narrow finder surface the recovery sweep needs: the three strand-shape
// candidate queries (design §4.2). PgxWorkUnitRepository satisfies it.
type RecoveryStore interface {
	// FindStalledFinalizationUnits returns shape-1 candidates: COMPLETED/REJECTED units aged
	// past olderThan on their finalization clock, EXCLUDING the pre-fix credit-residue shape.
	FindStalledFinalizationUnits(ctx context.Context, olderThan time.Duration, limit int) ([]types.ID, error)
	// FindStalledQueuedAtQuorum returns shape-2 candidates: QUEUED units holding a quorum's
	// worth of PENDING results whose newest PENDING result is aged past olderThan.
	FindStalledQueuedAtQuorum(ctx context.Context, olderThan time.Duration, limit int) ([]types.ID, error)
	// FindFinalizationResidueUnits returns the pre-fix credit-residue shape (COMPLETED with
	// zero PENDING and >= 1 AGREED result) — reported, never re-driven.
	FindFinalizationResidueUnits(ctx context.Context, limit int) ([]types.ID, error)
}

// Evaluator is the re-drive surface — one idempotent Evaluate per candidate unit. *Transitioner
// satisfies it. Narrowed to just Evaluate so the sweep depends on nothing else.
type Evaluator interface {
	Evaluate(ctx context.Context, workUnitID types.ID) (Outcome, error)
}

// RecoverySweeper is a leader-gated singleton that re-drives finalization-stalled work units by
// calling the transitioner's idempotent Evaluate over pure state predicates — the standing
// re-scan half of finalization liveness (E1-L, design §4.2). It matches the revocation
// reconciler / content-verify sweep shape: one sweep immediately on election, then a fixed
// ticker until the context is cancelled (leadership lost or head shutdown).
//
// It is UNCONDITIONAL (no enable knob), matching the revocation-reconciler precedent: it is a
// correctness reconciler, and on a healthy head it is two indexed near-empty queries per
// interval on the leader. Even two overlapping sweepers during a failover are safe — everything
// funnels into guarded, idempotent Evaluate.
type RecoverySweeper struct {
	store     RecoveryStore
	evaluator Evaluator
	interval  time.Duration
	grace     time.Duration
	batch     int
	logger    *slog.Logger
}

// NewRecoverySweeper wires the sweeper. interval is the ticker cadence; grace is the minimum age
// before a stalled unit is re-driven (headroom for in-flight natural Evaluates); batch caps the
// units re-driven per tick. logger may be nil (a discard logger is used).
func NewRecoverySweeper(store RecoveryStore, evaluator Evaluator, interval, grace time.Duration, batch int, logger *slog.Logger) *RecoverySweeper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &RecoverySweeper{store: store, evaluator: evaluator, interval: interval, grace: grace, batch: batch, logger: logger}
}

// Run performs one reconciliation sweep immediately, then on the interval ticker until ctx is
// cancelled (leadership lost or head shutdown).
func (w *RecoverySweeper) Run(ctx context.Context) {
	w.logger.Info("finalization recovery sweeper started",
		"interval", w.interval.String(), "grace", w.grace.String(), "batch", w.batch)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("finalization recovery sweeper stopping")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// RunOnce performs a single sweep pass and returns. Exported so a test (or an operator one-shot
// trigger) can drive exactly one pass deterministically; Run calls the same sweep on its ticker.
func (w *RecoverySweeper) RunOnce(ctx context.Context) { w.sweep(ctx) }

// sweep runs one pass: report the pre-fix residue, then re-drive the two strand shapes. A query
// failure is logged and never aborts the pass (the next tick retries); a per-unit Evaluate
// failure is logged and the sweep continues to the next unit.
func (w *RecoverySweeper) sweep(ctx context.Context) {
	// (a) Pre-fix residue: report, never repair. Re-Evaluate cannot help a COMPLETED unit whose
	// results are already adjudicated (the marks committed without the flip/credit in the
	// pre-atomic accept path) — one WARN per unit for the operator, then skip.
	residue, err := w.store.FindFinalizationResidueUnits(ctx, w.batch)
	if err != nil {
		w.logger.Error("finalization recovery: residue query failed", "error", err)
	}
	for _, id := range residue {
		w.logger.Warn("pre-fix finalization residue: COMPLETED unit with adjudicated results; cannot be repaired by re-evaluation — operator adjudication required (see E1 closeout census)",
			"work_unit_id", id)
	}

	// (b) Strand shapes 1 then 2: re-drive each via the idempotent Evaluate.
	shape1, err := w.store.FindStalledFinalizationUnits(ctx, w.grace, w.batch)
	if err != nil {
		w.logger.Error("finalization recovery: stalled-finalization query failed", "error", err)
	}
	shape2, err := w.store.FindStalledQueuedAtQuorum(ctx, w.grace, w.batch)
	if err != nil {
		w.logger.Error("finalization recovery: queued-at-quorum query failed", "error", err)
	}

	candidates := len(shape1) + len(shape2)
	if candidates == 0 {
		// Quiet on a healthy head: no re-drive candidates, no per-sweep log line.
		return
	}
	drive := func(ids []types.ID) {
		for _, id := range ids {
			if _, e := w.evaluator.Evaluate(ctx, id); e != nil {
				// Best-effort: the next tick re-selects and retries this unit.
				w.logger.Warn("finalization recovery: re-evaluate failed; will retry next tick",
					"work_unit_id", id, "error", e)
			}
		}
	}
	drive(shape1)
	drive(shape2)
	w.logger.Info("finalization recovery sweep re-drove stalled units",
		"stalled_finalization", len(shape1), "queued_at_quorum", len(shape2), "residue", len(residue))
}
