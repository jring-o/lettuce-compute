-- 00008_volunteer_hosts.up.sql
-- Account <-> host split (TODO #19): the Ed25519 keypair is the ACCOUNT; a user runs
-- the SAME key on every machine. Credit/RAC/attestations and per-WU distinctness stay
-- per ACCOUNT (the existing `volunteers` row + uq_results_work_unit_volunteer +
-- uq_wuah_live_copy_per_volunteer, all keyed on volunteer_id). This migration adds a
-- per-MACHINE host so the per-machine facts that used to collide on the single
-- `volunteers` row (advertised runtimes/hardware -> the flapping-row bug, in-flight cap,
-- the work-send-interval floor, last-seen) are tracked per host, and a unit's copies +
-- results can be attributed to the machine that produced them.
--
-- ADDITIVE / non-breaking: a volunteer that reports no host_id keeps today's per-account
-- behavior (the head treats host == account). The new columns are nullable; no backfill.
--
-- IMPORTANT: distinctness is NOT re-keyed here. Redundant copies of a unit must still
-- come from distinct ACCOUNTS (a user's own machines must never corroborate each other),
-- so the uniqueness/distinctness constraints stay on volunteer_id. host_id is purely
-- additive attribution + per-machine metering.

-- 1. hosts: one row per (account, machine). The primary key is the head's deterministic
--    "effective host id" derived from (volunteer_id, host_key) so it is stable across
--    restarts/replicas and computable in memory with no lookup; host_key is the raw
--    string the volunteer self-generates and persists next to its keypair. The
--    per-machine capability columns mirror what the `volunteers` row carries for the
--    no-host fallback path, but stored per host so N machines under one key no longer
--    overwrite each other.
CREATE TABLE public.hosts (
    id uuid NOT NULL,
    volunteer_id uuid NOT NULL,
    host_key text NOT NULL,
    display_name character varying(100),
    hardware_capabilities jsonb DEFAULT '{}'::jsonb NOT NULL,
    available_runtimes text[] DEFAULT '{NATIVE}'::text[] NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    last_seen_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hosts_pkey PRIMARY KEY (id),
    CONSTRAINT hosts_volunteer_id_fkey FOREIGN KEY (volunteer_id)
        REFERENCES public.volunteers(id) ON DELETE CASCADE,
    CONSTRAINT hosts_volunteer_id_host_key_key UNIQUE (volunteer_id, host_key)
);

-- List a user's machines (and the FK lookups) by account.
CREATE INDEX idx_hosts_volunteer_id ON public.hosts (volunteer_id);

-- 2. Per-machine attribution on the copy + result rows. NULL = produced by a volunteer
--    that did not report a host (the per-account fallback); a non-NULL value is the
--    hosts.id of the machine. The dispatch path keys the per-host in-flight cap and the
--    work-send floor on COALESCE(host_id, volunteer_id), which exactly equals the head's
--    effective host id (= volunteer_id in the fallback), so the in-memory metering and
--    the DB agree with no special-casing.
ALTER TABLE public.work_unit_assignment_history
    ADD COLUMN host_id uuid;

ALTER TABLE public.results
    ADD COLUMN host_id uuid;

-- Per-host live-copy count (the in-flight cap reconcile, re-keyed per machine). Kept
-- tiny by the partial predicate, mirroring idx_wuah_active_by_volunteer.
CREATE INDEX idx_wuah_active_by_host
    ON public.work_unit_assignment_history (host_id)
    WHERE (outcome IS NULL AND host_id IS NOT NULL);
