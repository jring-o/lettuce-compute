-- Down pair for 00021_audit_enforcement (reverse order of the up).
DROP INDEX IF EXISTS idx_audit_repairs_audit;
DROP TABLE IF EXISTS audit_repairs;

DROP INDEX IF EXISTS idx_credit_adjustments_audit;
ALTER TABLE credit_adjustments
    DROP COLUMN IF EXISTS rac_applied_at,
    DROP COLUMN IF EXISTS audit_id;

DROP INDEX IF EXISTS idx_result_audits_enforcement;
DROP INDEX IF EXISTS idx_result_audits_confirms;
ALTER TABLE result_audits
    DROP COLUMN IF EXISTS claimed_hr_class,
    DROP COLUMN IF EXISTS confirms_audit_id,
    DROP COLUMN IF EXISTS enforced_at,
    DROP COLUMN IF EXISTS enforcement_state,
    DROP COLUMN IF EXISTS enforcement_eligible;
