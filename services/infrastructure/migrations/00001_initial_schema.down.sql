-- 00001_initial_schema.down.sql
-- Revert the initial schema.

DROP TABLE IF EXISTS public.accounts CASCADE;
DROP TABLE IF EXISTS public.api_keys CASCADE;
DROP TABLE IF EXISTS public.batches CASCADE;
DROP TABLE IF EXISTS public.credit_attestations CASCADE;
DROP TABLE IF EXISTS public.credit_ledger CASCADE;
DROP TABLE IF EXISTS public.file_uploads CASCADE;
DROP TABLE IF EXISTS public.health_metrics_history CASCADE;
DROP TABLE IF EXISTS public.identity_challenges CASCADE;
DROP TABLE IF EXISTS public.leaf_stats_snapshots CASCADE;
DROP TABLE IF EXISTS public.leafs CASCADE;
DROP TABLE IF EXISTS public.research_areas CASCADE;
DROP TABLE IF EXISTS public.results CASCADE;
DROP TABLE IF EXISTS public.sessions CASCADE;
DROP TABLE IF EXISTS public.users CASCADE;
DROP TABLE IF EXISTS public.verification_tokens CASCADE;
DROP TABLE IF EXISTS public.volunteer_leaf_preferences CASCADE;
DROP TABLE IF EXISTS public.volunteer_rac CASCADE;
DROP TABLE IF EXISTS public.volunteers CASCADE;
DROP TABLE IF EXISTS public.work_unit_assignment_history CASCADE;
DROP TABLE IF EXISTS public.work_units CASCADE;

DROP TYPE IF EXISTS public.assignment_outcome CASCADE;
DROP TYPE IF EXISTS public.comparison_mode CASCADE;
DROP TYPE IF EXISTS public.leaf_state CASCADE;
DROP TYPE IF EXISTS public.leaf_visibility CASCADE;
DROP TYPE IF EXISTS public.runtime_type CASCADE;
DROP TYPE IF EXISTS public.task_pattern CASCADE;
DROP TYPE IF EXISTS public.validation_status CASCADE;
DROP TYPE IF EXISTS public.work_unit_priority CASCADE;
DROP TYPE IF EXISTS public.work_unit_state CASCADE;
