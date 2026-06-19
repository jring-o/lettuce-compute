-- 00006_per_copy_dispatch.up.sql
-- Per-copy parallel dispatch + uncapped distinct requeue (design properties 6 & 7).
--
-- BEFORE: a work unit had a SINGLE reservation column (reserved_volunteer_id /
-- reserved_until) and run-start flipped the WHOLE unit QUEUED -> ASSIGNED. That made
-- redundancy SERIAL: a redundancy=N unit could only have one live holder at a time,
-- so the N corroborating copies ran one-after-another (on reassignment), never in
-- parallel; and a timed-out copy was capped by max_reassignments and then FAILED.
--
-- AFTER (this migration): each dispatched COPY of a work unit is its own row in
-- work_unit_assignment_history — the per-copy record, one independent instance of the
-- work. A redundancy=N unit can have up to N live copies AT ONCE, each held by a DISTINCT
-- volunteer, each with its own lease + deadline. The unit stays QUEUED (a pure
-- aggregate) until enough results accumulate, then COMPLETED -> VALIDATED/REJECTED.
-- The single per-unit reservation columns are retired. A timed-out copy just closes
-- (outcome=EXPIRED) and the unit keeps dispatching fresh copies until its redundancy
-- is met or it hits a head-owned dead-letter ceiling (max_total_copies).
--
-- This generalizes the model the spot-check path already used (a history row per
-- holder, created at reservation) to ALL redundancy.

-- 1. Promote work_unit_assignment_history rows to first-class COPIES.
--    A copy's lifecycle is entirely on its row:
--      RESERVED : outcome IS NULL, started_at IS NULL, reserved_until > NOW()
--                 (buffered in a volunteer's work buffer; held until reserved_until)
--      RUNNING  : outcome IS NULL, started_at IS NOT NULL
--                 (run-started; deadline clock = started_at + deadline_seconds)
--      closed   : outcome IS NOT NULL  (COMPLETED / EXPIRED / ABANDONED / REJECTED)
--    assigned_at is the copy's hand-out time. deadline_seconds is snapshotted from
--    the work unit at hand-out so the sweep needs no join.
ALTER TABLE public.work_unit_assignment_history
    ADD COLUMN reserved_until   timestamp with time zone,
    ADD COLUMN started_at       timestamp with time zone,
    ADD COLUMN deadline_seconds integer NOT NULL DEFAULT 0;

-- At most ONE live copy (outcome IS NULL) per (work_unit, volunteer): a volunteer can
-- never hold two concurrent copies of the same unit. This is the hard half of
-- "never two copies to the same volunteer" (property 7); the result-uniqueness
-- constraint (uq_results_work_unit_volunteer) is the other half.
CREATE UNIQUE INDEX uq_wuah_live_copy_per_volunteer
    ON public.work_unit_assignment_history (work_unit_id, volunteer_id)
    WHERE (outcome IS NULL);

-- Active-copy count per unit (the redundancy headroom probe) — kept tiny by the
-- partial predicate.
CREATE INDEX idx_wuah_active_by_unit
    ON public.work_unit_assignment_history (work_unit_id)
    WHERE (outcome IS NULL);

-- Buffered-lapse sweep: a RESERVED copy whose holder vanished before run-start is
-- reclaimed once reserved_until < NOW().
CREATE INDEX idx_wuah_buffered_lease
    ON public.work_unit_assignment_history (reserved_until)
    WHERE (outcome IS NULL AND started_at IS NULL);

-- Running-deadline sweep: a RUNNING copy past started_at + deadline_seconds has
-- timed out. (deadline_seconds is on the row, so the sweep is index-driven + a cheap
-- arithmetic filter.)
CREATE INDEX idx_wuah_running_deadline
    ON public.work_unit_assignment_history (started_at)
    WHERE (outcome IS NULL AND started_at IS NOT NULL);

-- Per-volunteer live-copy count (inflight cap reconcile).
CREATE INDEX idx_wuah_active_by_volunteer
    ON public.work_unit_assignment_history (volunteer_id)
    WHERE (outcome IS NULL);

-- 2. Dead-letter ceiling for property 6 (a cap on TOTAL copies ever made). A unit that
--    keeps timing out is redispatched with NO per-reassignment cap; only when the
--    TOTAL number of copies ever created for it reaches max_total_copies (and its
--    redundancy is still unmet, with no live copy outstanding) is it parked FAILED +
--    flagged-for-review, so one poison unit can't burn the volunteer pool forever.
--    0 = "derive a default from redundancy" in code (redundancy_factor + a margin).
--    Head-owned, floor only, NO upper cap.
ALTER TABLE public.work_units
    ADD COLUMN max_total_copies integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT work_units_max_total_copies_check CHECK ((max_total_copies >= 0));

-- 3. Clean cutover for any work in flight at migration time. Pre-existing open
--    assignment-history rows carry no per-copy lease (reserved_until / started_at /
--    deadline_seconds are NULL/0 for them), and any ASSIGNED/RUNNING/EXPIRED unit
--    predates the QUEUED-only aggregate model — neither fits the new dispatch path and
--    would otherwise strand. Abandon those open copies and return their units to the
--    queue so the new per-copy model redispatches them cleanly. Validated/completed
--    work (closed copies, terminal unit states) is untouched.
UPDATE public.work_unit_assignment_history
    SET outcome = 'ABANDONED', outcome_at = NOW()
    WHERE outcome IS NULL;
UPDATE public.work_units
    SET state = 'QUEUED', priority = 'HIGH',
        assigned_volunteer_id = NULL, assigned_at = NULL, started_at = NULL
    WHERE state IN ('ASSIGNED', 'RUNNING', 'EXPIRED');

-- 4. Retire the single-holder reservation model: live dispatch state is now per-copy
--    (the history rows above), so the per-unit reservation columns and their indexes
--    are gone. assigned_volunteer_id / assigned_at remain on work_units only as a
--    denormalized "most recent copy" convenience for observability; they are no
--    longer the dispatch source of truth.
DROP INDEX IF EXISTS public.idx_work_units_reserved;
ALTER TABLE public.work_units
    DROP COLUMN reserved_until,
    DROP COLUMN reserved_volunteer_id;
