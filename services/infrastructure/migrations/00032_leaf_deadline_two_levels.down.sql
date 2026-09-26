-- 00032_leaf_deadline_two_levels down.
--
-- Deliberately a no-op, and LOSSY in the record-keeping sense. Which leafs were
-- given a deadline_seconds by the backfill is not recorded, and a no_deadline leaf's
-- ignored deadline_seconds value is gone. Neither matters to the previous release:
-- a backfilled deadline_seconds is the number it computes from the multiplier left
-- in place, and a no_deadline leaf's deadline_seconds was never read. So every
-- prior release resolves every leaf to the same deadline with the backfill in place.
SELECT 1;
