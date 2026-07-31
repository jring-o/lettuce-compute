-- 00029_backfill_min_disk_mb.up.sql
-- Backfill resource_requirements.min_disk_mb on legacy leafs (TB-31).
--
-- The head stamps min_disk_mb = 1024 into every leaf at creation
-- (ApplyResourceRequirementsDefaults), but leafs created before that default
-- existed carry nothing. A volunteer receiving no declared need substitutes a
-- conservative client-side fallback, so an undeclared leaf looks more
-- disk-hungry to every volunteer than it is — the fallback (10 GB at the time)
-- exactly equalled the default max_disk_gb allowance, silently disk-gating
-- default-configured volunteers on those leafs. Stamping the same 1024 MB the
-- creation path uses makes every legacy leaf's metadata honest fleet-wide and
-- leaves the client fallback for heads that publish no requirements at all.
--
-- Additive in effect (fills a missing JSONB key; declared values untouched)
-- and instant: leafs is a small metadata table (tens of rows), so the
-- unbatched UPDATE is safe at boot.
UPDATE leafs
SET resource_requirements = jsonb_set(resource_requirements, '{min_disk_mb}', to_jsonb(1024), true)
WHERE COALESCE((resource_requirements->>'min_disk_mb')::bigint, 0) <= 0;
