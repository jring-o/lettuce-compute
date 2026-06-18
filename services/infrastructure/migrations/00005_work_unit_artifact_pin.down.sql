-- 00005_work_unit_artifact_pin.down.sql
-- Reverse 00005.

DROP INDEX IF EXISTS public.idx_results_artifact_version;

ALTER TABLE ONLY public.results
    DROP CONSTRAINT IF EXISTS results_artifact_version_id_fkey;

ALTER TABLE public.results
    DROP COLUMN IF EXISTS artifact_version_id;

DROP INDEX IF EXISTS public.idx_work_units_pinned_artifact;

ALTER TABLE ONLY public.work_units
    DROP CONSTRAINT IF EXISTS work_units_pinned_artifact_version_id_fkey;

ALTER TABLE public.work_units
    DROP COLUMN IF EXISTS pinned_artifact_version_id;
