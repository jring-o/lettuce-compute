package workunit

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// RepoBatchSink is a BatchSink backed by plain WorkUnit / Batch repositories. It performs the
// three per-batch writes sequentially and WITHOUT a transaction, and it does NOT support a
// generation-cursor advance (durable atomic cursor advance requires the transactional sink in
// package generate, which owns a pool). It exists for callers that persist over already-scoped
// repositories and for unit tests over mock repos; the production generation path (eager /generate
// and the lazy manager) uses the transactional sink so a crash cannot strand half a batch.
type RepoBatchSink struct {
	wuRepo    WorkUnitRepository
	batchRepo BatchRepository
}

// NewRepoBatchSink builds a non-transactional BatchSink over the given repositories.
func NewRepoBatchSink(wuRepo WorkUnitRepository, batchRepo BatchRepository) *RepoBatchSink {
	return &RepoBatchSink{wuRepo: wuRepo, batchRepo: batchRepo}
}

// NextSequenceNumber returns max(existing sequence_number) + 1 for the leaf's batches. It mirrors
// generate.ResolveNextSequenceNumber (inlined here to avoid the generate->workunit import cycle).
func (s *RepoBatchSink) NextSequenceNumber(ctx context.Context, leafID types.ID) (int, error) {
	batches, _, err := s.batchRepo.ListByLeaf(ctx, leafID, types.PaginationRequest{PageSize: 200})
	if err != nil {
		return 0, apierror.Internal("query existing batches", err)
	}
	maxSeq := 0
	for _, b := range batches {
		if b.SequenceNumber > maxSeq {
			maxSeq = b.SequenceNumber
		}
	}
	return maxSeq + 1, nil
}

// PersistBatch creates the batch, wires each work unit to it, bulk-inserts them, and transitions
// them CREATED->QUEUED. A non-nil cursor is rejected: this sink cannot apply a durable cursor
// advance (no transaction, no leaf repo) — the caller must use the transactional sink.
func (s *RepoBatchSink) PersistBatch(ctx context.Context, batch *Batch, wus []*WorkUnit, cursor *GenerationCursorAdvance) error {
	if cursor != nil {
		return apierror.Internal("RepoBatchSink cannot apply a generation cursor advance; use the transactional sink", nil)
	}
	if err := s.batchRepo.Create(ctx, batch); err != nil {
		return err
	}
	for i := range wus {
		wus[i].BatchID = &batch.ID
	}
	if err := s.wuRepo.BulkCreate(ctx, wus); err != nil {
		return err
	}
	if _, err := s.wuRepo.BulkTransitionByBatch(ctx, batch.ID, WorkUnitStateCreated, WorkUnitStateQueued); err != nil {
		return err
	}
	return nil
}

// interface satisfaction check.
var _ BatchSink = (*RepoBatchSink)(nil)
