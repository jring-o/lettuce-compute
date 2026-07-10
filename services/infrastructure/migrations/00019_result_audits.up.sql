-- 00019_result_audits: trusted-runner registry + post-hoc result-audit jobs (observe-only).
--
-- trusted_runners is the admin-managed registry of volunteer accounts the operator vouches
-- for as audit re-execution runners (head-operated or operator-vetted machines). Registry
-- membership authorizes the runner job surface (AuditService claim/submit) and upgrades the
-- trust-accrual witness rule; it is deliberately minimal — externally-identified/bonded
-- runner fields arrive additively when such runners become real.
--
-- result_audits is the job + verdict table for post-hoc re-execution of VALIDATED work
-- units: after a unit validates, it is sampled (crypto/rand, per-leaf rate overlaid on the
-- head default) and queued here; a registered runner claims the job, re-executes the unit
-- from the snapshots pinned at sampling time, and returns raw output bytes; the HEAD
-- computes the verdict. Rows here are invisible to all validation/quorum/coverage
-- arithmetic (those queries name the physical results/work_unit_assignment_history tables
-- only). MISMATCH verdicts are recorded + logged in this slice; consequences are a later,
-- separately-gated slice.
--
-- Additive and deploy-neutral: nothing writes these tables unless the audit knobs are
-- enabled, and the claim surface refuses volunteers not present in the (initially empty)
-- registry.

CREATE TABLE trusted_runners (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    volunteer_id  uuid NOT NULL UNIQUE REFERENCES volunteers(id) ON DELETE CASCADE,
    label         varchar(128) NOT NULL,
    note          text,
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE result_audits (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_unit_id            uuid NOT NULL REFERENCES work_units(id) ON DELETE CASCADE,
    leaf_id                 uuid NOT NULL REFERENCES leafs(id) ON DELETE CASCADE,
    -- The accepted winner this audit adjudicates against: the AGREED result with the
    -- lexicographically smallest id (deterministic representative; NUMERIC verdicts read
    -- every AGREED member at verdict time, so the pick carries no comparison weight).
    accepted_result_id      uuid NOT NULL REFERENCES results(id) ON DELETE CASCADE,
    -- EXACT-mode grouping key of the winner at sampling time (raw submitted checksum, or
    -- the canonical stripped-key form). NULL for NUMERIC_TOLERANCE (value-level verdict).
    accepted_comparison_key text,
    -- Validation-time comparison semantics, pinned at sampling so a later leaf-config edit
    -- can never change how this audit adjudicates:
    -- {comparison_mode, numeric_tolerance, ignore_fields, compare_fields}.
    comparison_snapshot     jsonb NOT NULL,
    -- Hardware-class requirement for claiming: NULL = any runner; non-NULL = the unit's
    -- pinned class (set for every pinned unit regardless of comparison mode — a
    -- homogeneous-redundancy leaf is not portably deterministic across classes).
    required_hr_class       text,
    -- Provenance + GC-pin join target. SET NULL on version deletion: deleting a version is
    -- never blocked by an audit; the audit degrades to INCONCLUSIVE at claim time.
    artifact_version_id     uuid REFERENCES leaf_artifact_versions(id) ON DELETE SET NULL,
    -- The effective ExecutionConfig the accepted winner ran, pinned at sampling time: the
    -- runner executes THIS, never a claim-time resolution of owner-mutable leaf config.
    execution_snapshot      jsonb NOT NULL,
    status                  varchar(16) NOT NULL DEFAULT 'QUEUED'
                            CHECK (status IN ('QUEUED', 'CLAIMED', 'COMPLETED', 'EXPIRED')),
    verdict                 varchar(16)
                            CHECK (verdict IN ('MATCH', 'MISMATCH', 'INCONCLUSIVE')),
    verdict_detail          text,
    attempts                int NOT NULL DEFAULT 0,
    claimed_by              uuid REFERENCES trusted_runners(id) ON DELETE SET NULL,
    lease_expires_at        timestamptz,
    -- Verbatim bytes the runner returned (bytea: jsonb would normalize tokens and break
    -- re-hashing against runner_output_checksum). The checksum is HEAD-computed sha256 of
    -- exactly these bytes — never a runner-claimed value.
    runner_output           bytea,
    runner_output_checksum  varchar(64),
    created_at              timestamptz NOT NULL DEFAULT now(),
    claimed_at              timestamptz,
    completed_at            timestamptz,
    -- COMPLETED always carries a verdict; QUEUED/CLAIMED/EXPIRED never do.
    CHECK ((status = 'COMPLETED') = (verdict IS NOT NULL)),
    -- A CLAIMED row the reclaim sweep cannot see (NULL lease) must be unrepresentable.
    CHECK (status <> 'CLAIMED' OR (claimed_by IS NOT NULL AND lease_expires_at IS NOT NULL))
);

-- Claim scan: oldest QUEUED first, filtered by hr class in the query.
CREATE INDEX idx_result_audits_claim ON result_audits (created_at) WHERE status = 'QUEUED';
-- Reclaim sweep: lapsed leases.
CREATE INDEX idx_result_audits_lease ON result_audits (lease_expires_at) WHERE status = 'CLAIMED';
-- Per-unit lookups (verdict reads, admin list joins).
CREATE INDEX idx_result_audits_unit ON result_audits (work_unit_id);
-- At most one open audit per unit (the sampling hook runs once per validated unit under the
-- per-unit advisory lock; this is the schema-level guarantee behind that).
CREATE UNIQUE INDEX uq_result_audits_open_unit ON result_audits (work_unit_id)
    WHERE status IN ('QUEUED', 'CLAIMED');
