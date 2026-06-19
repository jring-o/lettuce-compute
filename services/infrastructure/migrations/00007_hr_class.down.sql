-- 00007_hr_class.down.sql
DROP INDEX IF EXISTS public.idx_work_units_hr_class;
ALTER TABLE public.work_units
    DROP COLUMN IF EXISTS hr_class;
