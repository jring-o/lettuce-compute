-- 00025_remove_max_success_copies down.
--
-- Restores the column + its check so a schema rollback re-materializes a valid work_units shape.
-- The per-unit VALUES are unrecoverable (they were dropped and nothing read them, so nothing was
-- lost): the column comes back at its original DEFAULT 0. The stripped leafs.validation_config
-- keys are likewise NOT restored — they too were read by nothing, and 0 = the historical default
-- for the resolved knob. A code rollback does not require running this down (the removed field is
-- inert to any code that does not reference it).
ALTER TABLE public.work_units
    ADD COLUMN IF NOT EXISTS max_success_copies integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT work_units_max_success_copies_check CHECK ((max_success_copies >= 0));
