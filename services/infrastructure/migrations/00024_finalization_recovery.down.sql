-- 00024_finalization_recovery down.
--
-- Additive-only up (two partial indexes), so the down is a clean pair of index drops; a code
-- rollback does not require running this (the indexes are inert to code that does not run the
-- recovery sweep).
DROP INDEX IF EXISTS idx_results_pending_by_unit;
DROP INDEX IF EXISTS idx_wu_stalled;
