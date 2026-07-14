-- Migration 00026: durable per-leaf generation cursor (design §4.8, BG-22c / E1-3).
--
-- A "leaf" is a computation; its work units are generated lazily, a batch at a time, as the
-- volunteer queue drains. The generation CURSOR records how far the head has already generated
-- into the leaf's declared parameter space (seed offset, combination offset, total generated,
-- whether a finite space is exhausted). Before this migration that cursor lived inside the
-- owner-editable data_config -> splitting_config JSONB under the "_cursor" key, and every advance
-- was persisted by rewriting the WHOLE leaf row. That made the cursor a lost-update hazard: a
-- concurrent owner config edit carrying a stale config snapshot could roll the cursor BACKWARD
-- (silently re-generating already-emitted work units), and a lazy tick could clobber a concurrent
-- owner edit.
--
-- This adds a dedicated generation_cursor column that ONLY the generation path writes, via a
-- guarded optimistic UPDATE keyed on total_generated (so a leadership-failover overlap or a
-- concurrent writer matches zero rows and aborts instead of double-emitting). Owner config edits
-- (leaf Update) no longer touch generation state at all. The existing _cursor is backfilled into
-- the new column and then stripped from splitting_config: no dual-read fallback, no vestigial key
-- (alpha no-vestigial policy).
--
-- DEPLOY CAVEAT (design §10 Q4): rolling the head CODE back WITHOUT running this migration's down
-- step leaves the old code reading the now-absent _cursor key and resuming from a zero cursor
-- ONCE — a single re-generation of the in-flight window (bounded, operator-accepted in alpha).
-- Roll code and schema back together, or accept the one-time cursor reset.

ALTER TABLE leafs ADD COLUMN generation_cursor jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Backfill the column from the old in-config cursor (adjusted to the real storage shape:
-- splitting_config lives inside the data_config jsonb).
UPDATE leafs
SET generation_cursor = COALESCE(data_config -> 'splitting_config' -> '_cursor', '{}'::jsonb);

-- Strip the now-migrated _cursor key from splitting_config.
UPDATE leafs
SET data_config = jsonb_set(
        data_config,
        '{splitting_config}',
        (data_config -> 'splitting_config') - '_cursor'
    )
WHERE data_config -> 'splitting_config' ? '_cursor';
