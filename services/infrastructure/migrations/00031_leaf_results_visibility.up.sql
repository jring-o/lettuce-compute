-- 00031_leaf_results_visibility.up.sql
-- Per-leaf results-visibility opt-in (design §4.7): leafs.results_visibility.
--
-- A leaf's results (output_data) are leaf CONTENTS — readable by the leaf's owner and
-- admins only, regardless of the leaf's catalog visibility (design §1.3, BG-07). The
-- dashboard's visualize page and replay-results API enforce that policy, and until now
-- there was no supported way to make one leaf's visualization publicly viewable; the
-- BG-07 comments explicitly reserved "the additive per-leaf results_visibility opt-in
-- from §4.7" as the intended escape hatch. This migration adds it.
--
-- OWNER_ONLY (the default every existing and future leaf gets) keeps exact prior
-- behavior. PUBLIC opts this one leaf's visualization page and replay results into
-- anonymous viewing. The policy lives on the leaf row — not in head or dashboard
-- config — so an edit flows through the leaf-update audit line (TB-38, field-level
-- before/after diff + actor), and the setting cannot outlive the leaf or silently
-- attach to a future leaf that reuses a freed slug.
--
-- Additive and instant: ADD COLUMN with a constant default is a catalog-only change
-- (no table rewrite) on PostgreSQL 11+; the head runs 16.
CREATE TYPE leaf_results_visibility AS ENUM ('OWNER_ONLY', 'PUBLIC');

ALTER TABLE leafs ADD COLUMN results_visibility leaf_results_visibility NOT NULL DEFAULT 'OWNER_ONLY';
