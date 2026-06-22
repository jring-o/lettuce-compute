-- 00010_redundancy_target_quorum_caps.up.sql
-- Redundancy state machine (TODO #50): split the conflated redundancy_factor into an
-- explicit TARGET (how many copies to dispatch) and QUORUM (how many agreeing results
-- validate), and add hard per-unit CAPS that bound a non-converging unit.
--
-- ADDITIVE / non-breaking: head-only. No proto / volunteer change. Every column defaults
-- to 0, and 0 means "derive from the leaf's validation_config exactly as today":
--   target_copies      0 -> redundancy_factor          (dispatch this many copies)
--   min_quorum         0 -> redundancy_factor          (this many agreeing results validate)
--   max_error_copies   0 -> unlimited (today: only the total ceiling bounds errors)
--   max_success_copies 0 -> target_copies              (today: dispatch stops at target)
-- so a leaf that only sets redundancy_factor (every live leaf) behaves byte-for-byte as
-- before. These mirror max_total_copies (00006): a per-unit integer stamped at generation,
-- floor-checked >= 0, resolved through an Effective*/Resolve* helper when 0.
--
-- Stamping per-unit (rather than always reading the leaf config at decision time) matches
-- deadline_seconds / max_total_copies / hr_class: a mid-flight leaf config edit then applies
-- only to NEWLY generated units, never silently re-targets in-flight ones.
ALTER TABLE public.work_units
    ADD COLUMN target_copies      integer NOT NULL DEFAULT 0,
    ADD COLUMN min_quorum         integer NOT NULL DEFAULT 0,
    ADD COLUMN max_error_copies   integer NOT NULL DEFAULT 0,
    ADD COLUMN max_success_copies integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT work_units_target_copies_check      CHECK ((target_copies >= 0)),
    ADD CONSTRAINT work_units_min_quorum_check         CHECK ((min_quorum >= 0)),
    ADD CONSTRAINT work_units_max_error_copies_check   CHECK ((max_error_copies >= 0)),
    ADD CONSTRAINT work_units_max_success_copies_check CHECK ((max_success_copies >= 0));
