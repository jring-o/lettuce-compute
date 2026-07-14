-- 00024_finalization_recovery: partial indexes for the finalization recovery sweep
-- (design doc E1 §4.2/§7.1; BG-21b liveness).
--
-- The recovery sweep is a leader-gated reconciler that re-drives finalization-stalled work
-- units through the idempotent transitioner. It runs two candidate queries per tick over pure
-- state predicates; these two partial indexes keep both queries near-empty scans on a healthy
-- head (the sweep exists for correctness, not throughput, so it must be cheap at rest).
--
-- Shape 1 — stalled COMPLETED/REJECTED units, aged on the finalization clock (completed_at for
-- COMPLETED). The partial predicate keeps the index at exactly the finalizing/rejected set,
-- which is tiny relative to the whole table (REJECTED rows are rare residue; their per-row
-- updated_at is read from the heap).
CREATE INDEX IF NOT EXISTS idx_wu_stalled ON public.work_units (completed_at)
    WHERE state IN ('COMPLETED', 'REJECTED');

-- Shape 2 — QUEUED units already holding a quorum's worth of PENDING results, aged on the
-- NEWEST pending result (results.created_at) rather than work_units.updated_at, which
-- dispatch-claim churn bumps with zero progress. The index is (work_unit_id, created_at) so the
-- results-side GROUP BY / MAX(created_at) drive rides it, and its size is bounded by in-flight
-- work rather than lazy-generation backlog.
CREATE INDEX IF NOT EXISTS idx_results_pending_by_unit ON public.results (work_unit_id, created_at)
    WHERE validation_status = 'PENDING';
