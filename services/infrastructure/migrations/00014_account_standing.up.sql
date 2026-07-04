-- 00014_account_standing: account standing (BG-24b) — additive, head-only.
--
-- standing is the account's dispatch/validation standing. OK = normal service.
-- PROBATION = still dispatched and its results are accepted, adjudicated, and
-- credited, but they never count toward quorum and never cover redundancy (full
-- replication is forced around them). BENCHED = no dispatch while benched_until
-- is NULL (indefinite) or in the future; an EXPIRED bench behaves as PROBATION.
--
-- standing_source records who owns the value: AUTO rows are managed by the
-- rejection-rate backpressure machine (a later migration adds its signal
-- columns); OPERATOR rows were set through the admin API and are never
-- auto-changed.
--
-- results.standing_at_submit is the submitter's EFFECTIVE standing at
-- submission time, stamped alongside the trust snapshot. Validation counts only
-- OK-stamped results; NULL = a legacy row created before this feature (OK).
ALTER TABLE public.volunteers
    ADD COLUMN standing character varying(16) NOT NULL DEFAULT 'OK'
        CHECK (standing IN ('OK', 'PROBATION', 'BENCHED')),
    ADD COLUMN benched_until timestamp with time zone,
    ADD COLUMN standing_source character varying(16) NOT NULL DEFAULT 'AUTO'
        CHECK (standing_source IN ('AUTO', 'OPERATOR')),
    ADD COLUMN standing_reason text,
    ADD COLUMN standing_changed_at timestamp with time zone;

-- Dispatch snapshots and the admin list scan only non-OK rows; the partial index
-- keeps the overwhelmingly-OK population free.
CREATE INDEX idx_volunteers_standing ON public.volunteers (standing) WHERE standing <> 'OK';

ALTER TABLE public.results
    ADD COLUMN standing_at_submit character varying(16)
        CHECK (standing_at_submit IN ('OK', 'PROBATION', 'BENCHED'));
