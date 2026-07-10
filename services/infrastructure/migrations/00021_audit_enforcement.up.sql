-- 00021_audit_enforcement: slice-3 enforcement bookkeeping (design doc §9.1).
--
-- Era + lifecycle bookkeeping on the verdict rows. enforcement_eligible records the KNOB
-- STATE AT VERDICT-WRITE TIME (the §7.11/F-M10 pin made structural: every pre-existing row
-- back-fills false and is never directly actionable). enforcement_state is the resumable
-- sweep's state machine; an eligible MISMATCH original is moved to AWAITING_CONFIRMATION
-- inside the SAME verdict UPDATE, so it is never observable in NONE (audit H1).
ALTER TABLE result_audits
    ADD COLUMN enforcement_eligible boolean NOT NULL DEFAULT false,
    ADD COLUMN enforcement_state    varchar(24) NOT NULL DEFAULT 'NONE'
        CHECK (enforcement_state IN
               ('NONE','AWAITING_CONFIRMATION','ENFORCED','CONTRADICTED','STALLED')),
    ADD COLUMN enforced_at          timestamptz,
    ADD COLUMN confirms_audit_id    uuid REFERENCES result_audits(id) ON DELETE RESTRICT,
    -- Audit M1: the claimant's SERVER-computed hr_class, stamped by the claim UPDATE.
    -- Class-diverse confirmation for unpinned units keys on the root row's value.
    ADD COLUMN claimed_hr_class     text;

-- A confirmation row is a plain audit row pointing at its original; originals never point
-- anywhere. Enforcement roots are originals only (confirms_audit_id IS NULL).
CREATE INDEX idx_result_audits_confirms ON result_audits (confirms_audit_id)
    WHERE confirms_audit_id IS NOT NULL;

-- The enforcement sweep scan: actionable roots.
CREATE INDEX idx_result_audits_enforcement ON result_audits (completed_at)
    WHERE verdict = 'MISMATCH' AND enforcement_eligible
      AND enforcement_state IN ('NONE','AWAITING_CONFIRMATION')
      AND confirms_audit_id IS NULL;

-- Adjustment provenance + RAC exactly-once bookkeeping. audit_id is RESTRICT: an
-- enforcement adjustment is settlement evidence — the audit row that caused money movement
-- must never be deletable out from under it. rac_applied_at stamps the exactly-once RAC
-- decrement (same transaction as the RAC update; design doc §9.5).
ALTER TABLE credit_adjustments
    ADD COLUMN audit_id       uuid REFERENCES result_audits(id) ON DELETE RESTRICT,
    ADD COLUMN rac_applied_at timestamptz;
CREATE INDEX idx_credit_adjustments_audit ON credit_adjustments (audit_id)
    WHERE audit_id IS NOT NULL;

-- Repair bookkeeping: the idempotency claim for the NON-idempotent repair effects (trust
-- accrual, reliability/standing compensation, counter bump — design doc §9.6). One repair
-- per result, ever. The flip/grant/attestation are idempotent by their own constraints and
-- do not depend on this row.
CREATE TABLE audit_repairs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    audit_id    uuid NOT NULL REFERENCES result_audits(id) ON DELETE RESTRICT,
    result_id   uuid NOT NULL UNIQUE REFERENCES results(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_repairs_audit ON audit_repairs (audit_id);
