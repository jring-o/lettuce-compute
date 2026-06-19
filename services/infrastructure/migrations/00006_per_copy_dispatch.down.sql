-- 00006_per_copy_dispatch.down.sql — reverse of the per-copy dispatch migration.

-- 3 (reverse): restore the single-holder reservation columns + index.
ALTER TABLE public.work_units
    ADD COLUMN reserved_until timestamp with time zone,
    ADD COLUMN reserved_volunteer_id uuid;
CREATE INDEX idx_work_units_reserved
    ON public.work_units USING btree (reserved_volunteer_id, reserved_until)
    WHERE (reserved_until IS NOT NULL);

-- 2 (reverse): drop the dead-letter ceiling.
ALTER TABLE public.work_units
    DROP CONSTRAINT IF EXISTS work_units_max_total_copies_check,
    DROP COLUMN max_total_copies;

-- 1 (reverse): demote copies back to plain assignment-history rows.
DROP INDEX IF EXISTS public.idx_wuah_active_by_volunteer;
DROP INDEX IF EXISTS public.idx_wuah_running_deadline;
DROP INDEX IF EXISTS public.idx_wuah_buffered_lease;
DROP INDEX IF EXISTS public.idx_wuah_active_by_unit;
DROP INDEX IF EXISTS public.uq_wuah_live_copy_per_volunteer;
ALTER TABLE public.work_unit_assignment_history
    DROP COLUMN deadline_seconds,
    DROP COLUMN started_at,
    DROP COLUMN reserved_until;
