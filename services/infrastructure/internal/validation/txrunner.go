package validation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// FinalizationStores bundles the three repositories the finalization closure writes through.
// On the production path each is scoped to ONE transaction (so the marks, the state flip, and
// the ledger rows commit or roll back together — design §4.1, invariant E1-S); on the
// passthrough path they are the engine's own pool-backed repos (no transaction), which keeps
// the mock-based engine tests working byte-for-byte.
type FinalizationStores struct {
	Results   result.Repository
	WorkUnits workunit.WorkUnitRepository
	Credits   credit.Repository
}

// FinalizationTxRunner runs the money-bearing half of accept/reject inside one serialized
// transaction. It is the seam that lets acceptResults/rejectAll be atomic in production while
// staying mock-friendly in tests: the engine hands it a closure (the tx phase — marks, state
// flip, credit/requeue) and does the best-effort effects (RAC, attestations, counters, ...)
// only after RunFinalization returns nil.
type FinalizationTxRunner interface {
	// RunFinalization opens the transaction for unitID, takes the hard unit-row serializer
	// (SELECT ... FOR UPDATE), re-validates the snapshot with an in-tx PENDING recheck
	// (aborting with transition.ErrStaleSnapshot when the transaction sees a different raw
	// PENDING count than the snapshot the decision was made from — review #2a), runs fn over
	// tx-scoped stores, and commits. On any error the transaction rolls back and the error is
	// returned; fn is NOT called when the recheck aborts.
	RunFinalization(ctx context.Context, unitID types.ID, rawPendingCount int, fn func(FinalizationStores) error) error
}

// finalizationTxAcquireTimeout bounds how long the production runner waits to ACQUIRE a pool
// connection (and complete the BEGIN round-trip) before failing — the BG-17 posture, mirrored
// locally so this package does not import internal/server. Only the acquire is bounded; the
// transaction's own statements run on the caller's context, so a legitimately slow finalization
// is never cut short by this backstop.
const finalizationTxAcquireTimeout = 5 * time.Second

// pgxFinalizationTxRunner is the production FinalizationTxRunner: one real Postgres transaction
// per finalization, with the unit-row lock and the stale-snapshot recheck.
type pgxFinalizationTxRunner struct {
	pool *pgxpool.Pool
	// decorate is a TEST-ONLY hook (installed via export_test.go) that wraps the tx-scoped
	// stores before fn runs, so an integration test can inject a failing Credits repo INSIDE
	// the real transaction and prove the whole tx rolls back (BG-21c/★E1-1). nil in production.
	decorate func(FinalizationStores) FinalizationStores
}

// NewPgxFinalizationTxRunner builds the production runner over the pool.
func NewPgxFinalizationTxRunner(pool *pgxpool.Pool) FinalizationTxRunner {
	return &pgxFinalizationTxRunner{pool: pool}
}

func (r *pgxFinalizationTxRunner) RunFinalization(ctx context.Context, unitID types.ID, rawPendingCount int, fn func(FinalizationStores) error) error {
	acquireCtx, cancel := context.WithTimeout(ctx, finalizationTxAcquireTimeout)
	tx, err := r.pool.Begin(acquireCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("begin finalization tx: %w", err)
	}
	// Roll back unless we reach an explicit Commit; a rollback after commit is a harmless no-op.
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The hard serializer (design §4.1): take the same unit-row lock both submit surfaces use,
	// so two concurrent accepts for one unit queue at the row and the loser's guarded flip
	// (inside fn) sees 0 rows -> Conflict -> its ENTIRE transaction, marks included, rolls back.
	// This stands in for the advisory lock, which is best-effort and may have degraded (★E1-4).
	if _, err := tx.Exec(ctx, "SELECT 1 FROM work_units WHERE id = $1 FOR UPDATE", unitID); err != nil {
		return fmt.Errorf("lock work unit %s: %w", unitID, err)
	}

	// In-tx stale-snapshot recheck (review #2a): the row lock serializes the WRITES; this
	// re-validates the READ. A submit that landed between the transitioner's snapshot load and
	// this lock changes the raw PENDING count, and adjudicating only the snapshot's rows would
	// orphan the new row PENDING under a terminal unit. Abort with ErrStaleSnapshot instead;
	// the transitioner retries once with a fresh snapshot. Raw-to-raw: version-heterogeneous
	// rows excluded by FilterPending exist in both counts, so they never trip a retry loop.
	var pendingNow int
	if err := tx.QueryRow(ctx,
		"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'PENDING'",
		unitID).Scan(&pendingNow); err != nil {
		return fmt.Errorf("recount pending results for %s: %w", unitID, err)
	}
	if pendingNow != rawPendingCount {
		return fmt.Errorf("%w: unit %s pending count %d != snapshot %d", transition.ErrStaleSnapshot, unitID, pendingNow, rawPendingCount)
	}

	stores := FinalizationStores{
		Results:   result.NewPgxRepository(tx),
		WorkUnits: workunit.NewPgxWorkUnitRepository(tx),
		Credits:   credit.NewPgxRepository(tx),
	}
	if r.decorate != nil {
		stores = r.decorate(stores)
	}

	if err := fn(stores); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finalization tx: %w", err)
	}
	return nil
}

// runFinalization dispatches to the injected FinalizationTxRunner when one is wired
// (WithTxRunner, the production path), else runs fn directly over the engine's own repos with
// NO transaction, NO unit-row lock, and NO stale-snapshot recheck — the passthrough default
// that keeps the mock-based engine tests unchanged. Compile-time proof the passthrough matches
// the interface is implicit: fn has the same shape either way.
func (e *Engine) runFinalization(ctx context.Context, unitID types.ID, rawPendingCount int, fn func(FinalizationStores) error) error {
	if e.txRunner != nil {
		return e.txRunner.RunFinalization(ctx, unitID, rawPendingCount, fn)
	}
	return fn(FinalizationStores{
		Results:   e.resultRepo,
		WorkUnits: e.workUnitRepo,
		Credits:   e.creditRepo,
	})
}

// interface satisfaction check.
var _ FinalizationTxRunner = (*pgxFinalizationTxRunner)(nil)
