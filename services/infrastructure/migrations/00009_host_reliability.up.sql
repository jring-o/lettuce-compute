-- 00009_host_reliability.up.sql
-- Reliability-weighted ADAPTIVE work quota (TODO #54). Grounds a host's work BUFFER
-- in OBSERVED THROUGHPUT instead of claimed specs: a machine earns a bigger in-flight
-- buffer as its units validate (grow) and loses it on waste (shrink). This is the
-- "smarter" generalization of the flat #53 send-interval floor, built on the #19
-- per-machine host id.
--
-- ADDITIVE / non-breaking: head-only dispatch shaping. No proto / volunteer change. The
-- feature is gated by LETTUCE_HEAD_RELIABILITY_QUOTA_ENABLED (on by default; the per-host
-- budget reads this table off the hot path). With the feature disabled every host keeps
-- today's flat in-flight cap and this table is simply never read.
--
-- KEY = the effective host id (= COALESCE(work_unit_assignment_history.host_id,
-- volunteer_id) = the #19 keystone). A volunteer that reports no host folds onto its
-- account id, so per-account fallback is automatic and the key matches the in-memory
-- metering. NO foreign key to hosts: in the per-account fallback the key is an account
-- id with no hosts row, and a host that vanishes simply stops being read (and decays).
--
-- The signal is a single DECAYING running score (reuses RAC's decay math: a decaying
-- net-good count, NOT a single-unit verdict — one slow unit must not brand a host a liar).
-- good_total / bad_total are decayed lifetime tallies kept for observability only.

CREATE TABLE public.host_reliability (
    host_id uuid NOT NULL,
    -- score: decaying NET-GOOD count as of last_updated. Each validated copy adds a good
    -- step; each timeout / abandon / disagreement subtracts a (larger) bad step, floored
    -- at 0. Decayed at read/write with the RAC half-life. The dispatch budget is derived
    -- from this: floor + (cap-floor)*clamp(0,1, score/ramp_units).
    score double precision DEFAULT 0 NOT NULL,
    -- Decayed lifetime tallies (observability / debugging only; not used for the budget).
    good_total double precision DEFAULT 0 NOT NULL,
    bad_total double precision DEFAULT 0 NOT NULL,
    last_updated timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT host_reliability_pkey PRIMARY KEY (host_id)
);

-- The budget refresher reads only recently-active hosts (a host inactive long enough has
-- decayed back toward the floor anyway), so an index on last_updated keeps that scan tight.
CREATE INDEX idx_host_reliability_last_updated ON public.host_reliability (last_updated);
