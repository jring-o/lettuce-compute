-- 00007_hr_class.up.sql
-- Homogeneous Redundancy (HR): pin every copy of a work unit to ONE hardware class.
--
-- For engines that are not portably deterministic (e.g. a MuJoCo/C++ sim whose
-- floating-point results differ across CPU vendor / SIMD / OS), bitwise redundant
-- verification is impossible across heterogeneous volunteers. HR sidesteps that:
-- once the first copy of a work unit is handed out, the unit is pinned to that
-- volunteer's hardware "class" (CPU vendor + OS + CPU arch) and all remaining copies
-- go only to volunteers of the SAME class — so their outputs ARE bit-comparable, and
-- divergence is sidestepped rather than fought.
--
-- This mirrors the artifact-version pin from 00005 (Homogeneous App Version): a
-- nullable column stamped first-writer-wins at first dispatch. The difference is that
-- HR additionally GATES eligibility (later copies are filtered to the pinned class),
-- which the dispatch path enforces; the column here is the durable pin.
--
-- NULL = unpinned (no copy handed out yet) OR the leaf does not enable
-- homogeneous_redundancy (validation_config.homogeneous_redundancy=false) — either way,
-- no class restriction applies.
ALTER TABLE public.work_units
    ADD COLUMN hr_class text;

-- The dispatch filter only ever reads hr_class for PINNED units; keep the index tiny
-- with a partial predicate (matches the 00005 pinned_artifact index shape).
CREATE INDEX idx_work_units_hr_class
    ON public.work_units (hr_class)
    WHERE (hr_class IS NOT NULL);
