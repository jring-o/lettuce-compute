-- 00029_backfill_min_disk_mb down.
--
-- Deliberately a no-op, and LOSSY in the record-keeping sense: which leafs
-- lacked min_disk_mb before the backfill is not recorded, so the value cannot
-- be selectively removed — and removing it (from leafs that legitimately
-- declare 1024, say) would break declared metadata to restore a defect. The
-- backfilled value is the same default the head stamps at leaf creation, so
-- every prior release runs correctly with it in place.
SELECT 1;
