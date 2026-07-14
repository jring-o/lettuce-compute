package generate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// ErrCursorConflict is returned by the transactional sink when a lazy batch's guarded cursor
// advance matches zero rows: another writer (a leadership-failover overlap, or a second leader's
// tick) advanced the cursor first. The whole batch transaction is rolled back — the losing tick
// aborts wholesale instead of double-emitting. The lazy manager surfaces it as a WARN and the
// next tick re-drives from the winner's committed cursor.
var ErrCursorConflict = errors.New("generation cursor advanced concurrently; batch aborted")

// GenerationStore is what the lazy manager depends on for durable generation persistence: the
// atomic per-batch BatchSink (which the generators call), plus the standalone guarded cursor
// write the manager uses to stamp GenerationExhausted after the final batch. The production
// implementation is PgxBatchSink; unit tests provide an in-memory fake.
type GenerationStore interface {
	workunit.BatchSink
	// UpdateGenerationCursor performs a standalone guarded cursor write (not inside a batch tx),
	// used for the exhaustion-flag stamp. Returns false when the guard (expectedPrevTotalGenerated
	// vs the row's current total_generated) does not match — a concurrent writer advanced first.
	UpdateGenerationCursor(ctx context.Context, leafID types.ID, cursor []byte, expectedPrevTotalGenerated int64) (bool, error)
}

// batchSinkAcquireTimeout bounds how long PersistBatch waits to ACQUIRE a pool connection and
// complete the BEGIN round-trip before failing (the BG-17 posture). Only the acquire is bounded;
// the batch's own statements run on the caller's context.
const batchSinkAcquireTimeout = 5 * time.Second

// PgxBatchSink is the production BatchSink / GenerationStore: it persists each generated batch —
// the batch row, its work units, their CREATED->QUEUED transition, and (on the lazy path) the
// guarded generation-cursor advance — inside ONE Postgres transaction. Neither a stranded-CREATED
// unit nor a committed-but-uncounted batch can exist (design §4.8, invariant E1-G). It is used by
// both the eager /generate handler and the lazy generation manager.
type PgxBatchSink struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	// decorateWU is a TEST-ONLY hook (set by integration tests in this package) that wraps the
	// tx-scoped work-unit repository before use, so a test can inject a failure INSIDE the batch
	// transaction and prove the whole tx rolls back atomically (nothing persisted). nil in
	// production.
	decorateWU func(workunit.WorkUnitRepository) workunit.WorkUnitRepository
}

// NewPgxBatchSink builds the production transactional sink over the pool.
func NewPgxBatchSink(pool *pgxpool.Pool, logger *slog.Logger) *PgxBatchSink {
	return &PgxBatchSink{pool: pool, logger: logger}
}

// NextSequenceNumber resolves the leaf's next batch sequence_number (max existing + 1) on the pool.
func (s *PgxBatchSink) NextSequenceNumber(ctx context.Context, leafID types.ID) (int, error) {
	var next int
	err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM batches WHERE leaf_id = $1", leafID,
	).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("resolve next batch sequence number for leaf %s: %w", leafID, err)
	}
	return next, nil
}

// PersistBatch commits the batch, its units, their transition, and (cursor != nil) the guarded
// cursor advance in one transaction. On a cursor-guard miss it returns ErrCursorConflict and the
// whole transaction rolls back.
func (s *PgxBatchSink) PersistBatch(ctx context.Context, batch *workunit.Batch, wus []*workunit.WorkUnit, cursor *workunit.GenerationCursorAdvance) error {
	acquireCtx, cancel := context.WithTimeout(ctx, batchSinkAcquireTimeout)
	tx, err := s.pool.Begin(acquireCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}
	// Roll back unless we reach an explicit Commit; a rollback after commit is a harmless no-op.
	defer func() { _ = tx.Rollback(context.Background()) }()

	batchRepo := workunit.NewPgxBatchRepository(tx)
	var wuRepo workunit.WorkUnitRepository = workunit.NewPgxWorkUnitRepository(tx)
	if s.decorateWU != nil {
		wuRepo = s.decorateWU(wuRepo)
	}

	if err := batchRepo.Create(ctx, batch); err != nil {
		return err
	}
	// The batch row now has an id; wire every unit to it before the bulk insert.
	for i := range wus {
		wus[i].BatchID = &batch.ID
	}
	if err := wuRepo.BulkCreate(ctx, wus); err != nil {
		return err
	}
	if _, err := wuRepo.BulkTransitionByBatch(ctx, batch.ID, workunit.WorkUnitStateCreated, workunit.WorkUnitStateQueued); err != nil {
		return err
	}
	if cursor != nil {
		ok, err := leaf.UpdateGenerationCursorTx(ctx, tx, cursor.LeafID, cursor.Cursor, cursor.ExpectedPrevTotalGenerated)
		if err != nil {
			return err
		}
		if !ok {
			return ErrCursorConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch tx: %w", err)
	}
	return nil
}

// UpdateGenerationCursor performs the standalone guarded cursor write on the pool (the
// exhaustion-flag stamp after the final batch). It is NOT inside a batch tx; the guard on
// total_generated still protects it from a concurrent advance.
func (s *PgxBatchSink) UpdateGenerationCursor(ctx context.Context, leafID types.ID, cursor []byte, expectedPrevTotalGenerated int64) (bool, error) {
	return leaf.UpdateGenerationCursorTx(ctx, s.pool, leafID, cursor, expectedPrevTotalGenerated)
}

// interface satisfaction checks.
var (
	_ workunit.BatchSink = (*PgxBatchSink)(nil)
	_ GenerationStore    = (*PgxBatchSink)(nil)
)
