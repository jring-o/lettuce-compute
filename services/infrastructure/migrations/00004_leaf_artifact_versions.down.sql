-- 00004_leaf_artifact_versions.down.sql
-- Reverse 00004. Drop the leaf -> current-version pointer first (it references the
-- registry table), then the registry table itself (its trigger/indexes/constraints
-- drop with it).

ALTER TABLE ONLY public.leafs
    DROP CONSTRAINT IF EXISTS leafs_current_artifact_version_id_fkey;

ALTER TABLE public.leafs
    DROP COLUMN IF EXISTS current_artifact_version_id;

DROP TABLE IF EXISTS public.leaf_artifact_versions;
