-- LOSSY DOWN: dropping the column forgets which leafs opted their results into
-- public visualization. Re-applying the up migration restores every leaf to the
-- OWNER_ONLY default; any PUBLIC opt-ins must be re-set by hand.
ALTER TABLE leafs DROP COLUMN results_visibility;

DROP TYPE leaf_results_visibility;
