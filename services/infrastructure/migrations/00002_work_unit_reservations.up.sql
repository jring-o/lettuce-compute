-- 00002_work_unit_reservations.up.sql
-- Add lightweight reservation columns for the client work buffer (Layer 1).
--
-- A buffered (leased) work unit stays state='QUEUED' but carries a
-- reserved_until lease and the reserving volunteer. While the lease is live the
-- reservation guard in FindNextAssignable hides the unit from other volunteers;
-- the reclaim monitors (FindAbandoned/FindExpired) key off assignment-time
-- timestamps and so ignore a still-QUEUED reserved unit until it is actually
-- assigned at run-start. A lapsed lease (reserved_until < NOW()) is
-- automatically re-reservable with no manual transition.

ALTER TABLE public.work_units
    ADD COLUMN reserved_until timestamp with time zone,
    ADD COLUMN reserved_volunteer_id uuid;

-- Partial index supporting the reservation guard and the per-volunteer live
-- reservation count: only live reservations are interesting, so the index is
-- restricted to rows that currently carry one.
CREATE INDEX idx_work_units_reserved
    ON public.work_units USING btree (reserved_volunteer_id, reserved_until)
    WHERE (reserved_until IS NOT NULL);

-- Partial index supporting the FindNextAssignable / ReserveNextAssignable hot
-- query's global queue ordering. That query has no leaf_id equality predicate
-- when a volunteer requests "any matching leaf", so the existing
-- idx_work_units_queue (which LEADS with leaf_id) cannot satisfy its
-- "ORDER BY priority DESC, created_at ASC" — the planner is forced to read every
-- QUEUED row and external-sort them before LIMIT 1, which is pathological at
-- 100k+ QUEUED units (full scan + on-disk merge sort on every assignment).
--
-- This index materializes the queue order directly, so the planner walks it in
-- priority/FIFO order and stops at the first row that survives the capability /
-- redundancy / reservation predicates — turning the per-assignment cost from
-- O(QUEUED rows) into O(rows skipped before the first match). The partial
-- WHERE clause keeps it tiny (only currently-dispatchable rows) and lets it also
-- serve the leaf-scoped variant. created_at is ASC to match the FIFO tiebreak.
CREATE INDEX idx_work_units_queue_order
    ON public.work_units USING btree (priority DESC, created_at ASC)
    WHERE (state = 'QUEUED'::public.work_unit_state);
