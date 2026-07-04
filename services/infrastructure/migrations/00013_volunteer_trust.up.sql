-- 00013_volunteer_trust.up.sql
-- Account-level trust signal for quorum gating.
--
-- Lettuce validates a volunteer's result by redundant corroboration: several
-- volunteers run the same work unit and their agreeing results validate it. Because
-- a volunteer account (an Ed25519 keypair) is free to mint, raw copy counting is
-- Sybil-vulnerable — one operator can spin up many accounts and manufacture an
-- "agreement". This table holds a per-SUBJECT trust score so the validator can
-- require that a quorum contain enough DISTINCT, TRUSTED subjects, not merely enough
-- copies. Trust is earned by corroborated-clean work and is seedable by the head
-- operator; the guiding principle is "identity must be cheap to HAVE but expensive to
-- have TRUSTED".
--
-- SUBJECT is the account-level trust key. It is either:
--   * a bound volunteer's ATProto decentralized identifier (DID) — see migration
--     00012 — so every device a person runs under one identity shares one score, or
--   * the per-keypair sentinel "vol:<volunteer-uuid>" for an unbound (or revoked)
--     volunteer that has only its keypair identity.
-- There is DELIBERATELY NO foreign key to volunteers (mirroring host_reliability's
-- design note): the subject is not always a volunteers row — a DID subject spans many
-- volunteer rows, and a "vol:<uuid>" subject may outlive the volunteer row it names —
-- so a foreign key would be wrong, not merely inconvenient.
--
-- Columns:
--   subject      the account-level trust key described above (primary key)
--   score        the QUORUM-POWER number: the value the trust gate compares against a
--                floor to decide whether the subject counts as trusted. Operator-
--                SEEDABLE (the admin trust API sets it directly to bootstrap the first
--                trusted subjects, since accrual is otherwise circular).
--   clean_units  the earned-by-corroborated-work counter (an append-only audit trail).
--                Accrual increments BOTH clean_units and score by 1; operator seeding
--                touches ONLY score. Keeping them separate lets an auditor tell earned
--                trust from granted trust.
--   slashed_at   when the subject was last slashed (score zeroed on a detected bad
--                result); NULL if never slashed. clean_units is retained across a slash
--                for the audit trail.
--
-- This migration also snapshots the submission-time trust onto each result:
--   results.trust_subject          the subject resolved for the submitting volunteer
--   results.trust_score_at_submit  that subject's score at submit time
-- Acceptance decisions must use the score AS OF SUBMISSION (not a later, possibly
-- slashed or re-accrued value), so the snapshot is stamped per result. NULL in either
-- column marks a legacy row created before this feature, following the later-ALTER
-- precedent set by results.artifact_version_id and results.host_id.
--
-- ADDITIVE / head-only: a new table plus two nullable result columns, no backfill and
-- no foreign keys. Nothing reads the table until the trust gate is enabled
-- (LETTUCE_HEAD_TRUST_GATE_ENABLED), and existing volunteers keep working against an
-- upgraded head with no proto / volunteer-CLI change. A code rollback does not require
-- a schema rollback.

CREATE TABLE public.volunteer_trust (
    subject text NOT NULL,
    score integer DEFAULT 0 NOT NULL CHECK (score >= 0),
    clean_units integer DEFAULT 0 NOT NULL CHECK (clean_units >= 0),
    slashed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT volunteer_trust_pkey PRIMARY KEY (subject)
);

ALTER TABLE public.results
    ADD COLUMN trust_subject text,
    ADD COLUMN trust_score_at_submit integer;
