-- 00032_leaf_deadline_two_levels.up.sql
-- Keep every stored leaf's work-unit deadline unchanged as its sources shrink to two.
--
-- A unit's deadline now comes from the leaf's fault_tolerance_config.deadline_seconds
-- when set, else the head's default deadline (head.default_deadline_seconds, 6 h
-- unless the operator changes it). The head no longer reads the two retired keys:
-- deadline_multiplier (a multiple of a fixed one-hour guess, used when
-- deadline_seconds was unset) and no_deadline (which meant "stamp the head's
-- reclaim ceiling", the same 6 h, and ignored deadline_seconds).
--
-- So that no leaf's deadline changes by surprise:
--   * a leaf that resolved from its multiplier (a multiplier worth at least one
--     second, no positive deadline_seconds, no_deadline not true) gets
--     deadline_seconds = trunc(3600 x multiplier), the number the previous release
--     computed;
--   * a no_deadline leaf keeps no deadline_seconds, so its units get the head
--     default, the ceiling they were stamped with before. A deadline_seconds such a
--     leaf carried was ignored before, so it is removed rather than start applying;
--   * a leaf with a positive deadline_seconds and no_deadline not true is unchanged;
--   * a leaf never configured (a zero or absent multiplier, which the previous
--     release's validation would not activate) is left alone and gets the head
--     default.
--
-- The retired keys themselves stay in the stored JSON, so a head still running the
-- previous release during a rolling deploy, or after a code rollback, resolves
-- every leaf to the same number (for a backfilled leaf the previous release prefers
-- the deadline_seconds it now carries). A leaf's next config update rewrites the
-- block without them.
--
-- Each guard is a CASE so a malformed value is skipped, never cast (a failed cast
-- would leave the schema dirty at boot). Instant: leafs is a small metadata table
-- (tens of rows), so the unbatched UPDATEs are safe at boot.
UPDATE leafs
SET fault_tolerance_config = jsonb_set(
        fault_tolerance_config,
        '{deadline_seconds}',
        to_jsonb(trunc(3600 * (fault_tolerance_config->>'deadline_multiplier')::float8)::bigint),
        true)
WHERE CASE WHEN jsonb_typeof(fault_tolerance_config->'deadline_multiplier') = 'number'
           THEN (fault_tolerance_config->>'deadline_multiplier')::float8 * 3600 >= 1
           ELSE false END
  AND fault_tolerance_config->'no_deadline' IS DISTINCT FROM 'true'::jsonb
  AND CASE WHEN jsonb_typeof(fault_tolerance_config->'deadline_seconds') = 'number'
           THEN (fault_tolerance_config->>'deadline_seconds')::float8 <= 0
           ELSE true END;

UPDATE leafs
SET fault_tolerance_config = fault_tolerance_config - 'deadline_seconds'
WHERE fault_tolerance_config->'no_deadline' = 'true'::jsonb
  AND fault_tolerance_config->'deadline_seconds' IS NOT NULL;
