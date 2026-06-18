-- 00005_work_unit_artifact_pin.up.sql
-- Pin each work unit to ONE artifact version, and stamp each result with the version
-- that produced it. Together these make "homogeneous redundancy" real (TODO #38 #3).
--
-- THE PROBLEM: redundancy_factor > 1 needs N independent results for ONE work unit to
-- be COMPARED against each other. Corroboration today is serial (on reassignment), so
-- replica 1 may run at T1 and replica 2 at T2 > T1. Once the head can change a leaf's
-- artifact between T1 and T2 (which fixing #38 #1 deliberately enables), the two
-- replicas could execute DIFFERENT artifact versions and produce legitimately
-- different output — a spurious DISAGREE, not a real one.
--
-- THE FIX:
--   * work_units.pinned_artifact_version_id  Stamped at a unit's FIRST dispatch with
--     the then-current leaf_artifact_versions row. EVERY later assignment for that
--     unit (reassignment / corroboration) builds from the pinned version, not the
--     live leaf. So all replicas of one unit run the SAME version. A unit that sits
--     QUEUED across a publish still picks up whatever is current at first dispatch
--     (pin-at-dispatch, not pin-at-generation), so new work tracks the latest version
--     while in-flight redundancy stays homogeneous. NULL = unpinned (legacy /
--     redundancy-1 leaves that never published a version) -> dispatch from the live
--     leaf exactly as before.
--   * results.artifact_version_id  Records which version produced each result, so the
--     validation engine can REFUSE to compare results from different versions
--     (interacts with #12 structured-output validation) and so the head has per-result
--     version provenance (interacts with #19 account<->host).
--
-- Both columns are nullable and FK ON DELETE SET NULL: existing rows and unversioned
-- leaves are unaffected, so this migration is non-breaking.

ALTER TABLE public.work_units
    ADD COLUMN pinned_artifact_version_id uuid;

ALTER TABLE ONLY public.work_units
    ADD CONSTRAINT work_units_pinned_artifact_version_id_fkey FOREIGN KEY (pinned_artifact_version_id) REFERENCES public.leaf_artifact_versions(id) ON DELETE SET NULL;

CREATE INDEX idx_work_units_pinned_artifact
    ON public.work_units USING btree (pinned_artifact_version_id)
    WHERE (pinned_artifact_version_id IS NOT NULL);

ALTER TABLE public.results
    ADD COLUMN artifact_version_id uuid;

ALTER TABLE ONLY public.results
    ADD CONSTRAINT results_artifact_version_id_fkey FOREIGN KEY (artifact_version_id) REFERENCES public.leaf_artifact_versions(id) ON DELETE SET NULL;

CREATE INDEX idx_results_artifact_version
    ON public.results USING btree (artifact_version_id)
    WHERE (artifact_version_id IS NOT NULL);
