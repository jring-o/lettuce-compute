-- 00025_remove_max_success_copies: drop the dead max_success_copies surface (BG-27 sub,
-- design §4.9).
--
-- max_success_copies was added in 00010 as a per-unit "over-dispatch" cap, but it was read by
-- NOTHING: not the transitioner's Decide, not the dispatch predicate, not any SQL. Its intended
-- semantics are also incoherent — below quorum it can never validate, between quorum and target
-- validate-at-quorum already supersedes the extras, and a success-count dispatch stop below
-- target would only bind exactly when cutting off corroboration is harmful. So it is removed
-- rather than wired.
--
-- Deliberately NON-additive (drops a column + its check): the alpha no-vestigial-columns policy
-- yields the usual additive preference here, an explicit trade-off. Removing the resolved knob
-- changes ZERO behavior because nothing read the value. The stored leaf-config key is stripped
-- in the same migration so the config surface matches the schema (leaf create/update also
-- rejects the key going forward — config honesty, E1-C).
ALTER TABLE public.work_units
    DROP CONSTRAINT IF EXISTS work_units_max_success_copies_check,
    DROP COLUMN IF EXISTS max_success_copies;

UPDATE public.leafs
    SET validation_config = validation_config - 'max_success_copies'
    WHERE validation_config ? 'max_success_copies';
