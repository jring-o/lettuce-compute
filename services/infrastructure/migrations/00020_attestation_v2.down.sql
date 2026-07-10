-- Revert 00020_attestation_v2.
--
-- Lossy by necessity: REVOKED rows cannot exist under the restored v1 outcome CHECK, so
-- they are deleted first. Their causing credit_adjustments rows remain the durable clawback
-- record, but the attestation list will over-report clawed credit again after this rollback
-- and rolling forward does NOT re-emit the deleted revocations. Runbook rule: do not
-- down-migrate past 00020 on a head that has issued revocation attestations.

DROP INDEX IF EXISTS uq_attestations_adjustment;
DROP INDEX IF EXISTS uq_attestations_result_agreed;
DROP INDEX IF EXISTS idx_attestations_revokes;
DROP INDEX IF EXISTS idx_attestations_result;

DELETE FROM credit_attestations WHERE validation_outcome = 'REVOKED';

ALTER TABLE credit_attestations DROP CONSTRAINT credit_attestations_validation_outcome_check;
ALTER TABLE credit_attestations ADD CONSTRAINT credit_attestations_validation_outcome_check
    CHECK (validation_outcome IN ('AGREED', 'DISAGREED', 'EXPIRED'));

ALTER TABLE credit_attestations
    DROP CONSTRAINT credit_attestations_v2_shape_check,
    DROP CONSTRAINT credit_attestations_revocation_check,
    DROP CONSTRAINT credit_attestations_schema_version_check;

ALTER TABLE credit_attestations
    DROP COLUMN reason,
    DROP COLUMN adjustment_id,
    DROP COLUMN revokes_attestation_id,
    DROP COLUMN policy_version,
    DROP COLUMN quorum_descriptor,
    DROP COLUMN output_checksum,
    DROP COLUMN result_id,
    DROP COLUMN schema_version;
