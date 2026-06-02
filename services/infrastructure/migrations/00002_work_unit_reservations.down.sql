-- 00002_work_unit_reservations.down.sql
-- Revert the work-unit reservation columns.

DROP INDEX IF EXISTS public.idx_work_units_queue_order;
DROP INDEX IF EXISTS public.idx_work_units_reserved;

ALTER TABLE public.work_units
    DROP COLUMN IF EXISTS reserved_until,
    DROP COLUMN IF EXISTS reserved_volunteer_id;
