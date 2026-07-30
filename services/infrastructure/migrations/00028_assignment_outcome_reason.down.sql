-- 00028_assignment_outcome_reason down.
--
-- LOSSY: the persisted abandon reasons are dropped with the column and cannot be
-- recovered on re-upgrade — they exist nowhere else once the head's logs rotate.
ALTER TABLE work_unit_assignment_history DROP COLUMN IF EXISTS outcome_reason;
