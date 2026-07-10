-- 00020_attestation_v2: versioned, domain-separated, result-bound credit attestations.
--
-- Attestations are the head's externally-verifiable record of credit decisions. The v1
-- signed form (migration 00001 columns) covered six fields with no schema version, no
-- result binding, and no domain separation: two attestations sharing the six-field tuple
-- were signature-transplantable, and the signed byte space was unversioned. v2 rows sign a
-- canonical JSON that includes an explicit context string, the result id, the server-side
-- output checksum, a quorum descriptor (what the policy demanded and what the comparison
-- delivered), and a policy version. New writes are v2-only from this migration's release
-- (hard cutover); existing rows are back-stamped schema_version 1 and keep verifying under
-- the frozen v1 rule.
--
-- Revocation attestations (validation_outcome = 'REVOKED') are signed statements that the
-- head clawed back previously attested credit: they reference the original grant attestation
-- (revokes_attestation_id, self-FK) and the credit_adjustments row that caused them
-- (adjustment_id), and carry the adjustment's machine-readable reason code so a row is
-- self-contained for verification.
--
-- Additive: no column is dropped or renamed, so a code rollback needs no schema rollback.

ALTER TABLE credit_attestations
    ADD COLUMN schema_version         int  NOT NULL DEFAULT 1,
    ADD COLUMN result_id              uuid REFERENCES results(id) ON DELETE RESTRICT,
    ADD COLUMN output_checksum        varchar(64),
    ADD COLUMN quorum_descriptor      jsonb,
    ADD COLUMN policy_version         int,
    ADD COLUMN revokes_attestation_id uuid REFERENCES credit_attestations(id) ON DELETE RESTRICT,
    ADD COLUMN adjustment_id          uuid REFERENCES credit_adjustments(id) ON DELETE RESTRICT,
    ADD COLUMN reason                 varchar(64);

-- Existing rows were back-stamped v1 by the ADD DEFAULT above; new inserts default to the
-- CURRENT format so an INSERT that forgets the column can never mint a false v1 row (the
-- repository always writes it explicitly regardless).
ALTER TABLE credit_attestations ALTER COLUMN schema_version SET DEFAULT 2;

ALTER TABLE credit_attestations
    ADD CONSTRAINT credit_attestations_schema_version_check CHECK (schema_version IN (1, 2)),
    -- A revocation always carries its back-reference, its causing adjustment, its result,
    -- and its reason code; nothing else ever does. Revocations are v2-only by construction.
    ADD CONSTRAINT credit_attestations_revocation_check
        CHECK ((validation_outcome = 'REVOKED')
               = (revokes_attestation_id IS NOT NULL AND adjustment_id IS NOT NULL
                  AND reason IS NOT NULL AND result_id IS NOT NULL AND schema_version = 2)),
    -- A v2 grant/reject row missing any signed-set column would be stored-but-unverifiable
    -- (the canonical rebuild would hit NULL); guard the whole signed set at write time.
    ADD CONSTRAINT credit_attestations_v2_shape_check
        CHECK (schema_version = 1
               OR validation_outcome = 'REVOKED'
               OR (result_id IS NOT NULL AND output_checksum IS NOT NULL
                   AND quorum_descriptor IS NOT NULL AND policy_version IS NOT NULL));

-- Replace the outcome CHECK: admit REVOKED; drop EXPIRED, which no code path has ever
-- written (verified against the full tree at design time — if a row somehow carries it,
-- failing the boot migration loudly is the correct outcome).
ALTER TABLE credit_attestations DROP CONSTRAINT credit_attestations_validation_outcome_check;
ALTER TABLE credit_attestations ADD CONSTRAINT credit_attestations_validation_outcome_check
    CHECK (validation_outcome IN ('AGREED', 'DISAGREED', 'REVOKED'));

-- Verify-by-result and revocation lookups.
CREATE INDEX idx_attestations_result ON credit_attestations (result_id)
    WHERE result_id IS NOT NULL;
CREATE INDEX idx_attestations_revokes ON credit_attestations (revokes_attestation_id)
    WHERE revokes_attestation_id IS NOT NULL;

-- The revocation producer binds to THE un-revoked AGREED v2 grant for a result; enforce the
-- "at most one AGREED grant attestation per result" invariant that binding rests on. A
-- future slice that needs a legitimate re-grant must relax this in its own migration.
CREATE UNIQUE INDEX uq_attestations_result_agreed ON credit_attestations (result_id)
    WHERE result_id IS NOT NULL AND validation_outcome = 'AGREED';

-- One revocation per adjustment: makes emission idempotent (the clawback handler and the
-- reconciliation sweep can race; the second insert resolves as a clean unique conflict).
CREATE UNIQUE INDEX uq_attestations_adjustment ON credit_attestations (adjustment_id)
    WHERE adjustment_id IS NOT NULL;
