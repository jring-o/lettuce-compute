-- 00010_redundancy_target_quorum_caps.down.sql
ALTER TABLE public.work_units
    DROP CONSTRAINT IF EXISTS work_units_target_copies_check,
    DROP CONSTRAINT IF EXISTS work_units_min_quorum_check,
    DROP CONSTRAINT IF EXISTS work_units_max_error_copies_check,
    DROP CONSTRAINT IF EXISTS work_units_max_success_copies_check,
    DROP COLUMN IF EXISTS target_copies,
    DROP COLUMN IF EXISTS min_quorum,
    DROP COLUMN IF EXISTS max_error_copies,
    DROP COLUMN IF EXISTS max_success_copies;
