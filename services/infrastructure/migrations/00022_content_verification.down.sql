-- 00022_content_verification down.
--
-- Remove every row in the slice-5 holding/terminal states before dropping the bookkeeping
-- columns. Referencing the enum values here is legal (they were committed by the up). The
-- DELETE is FK-safe by construction: a held/failed result was never PENDING, so it can never
-- have a credit_ledger row (AGREED-only), a credit_attestations row (AGREED/DISAGREED at
-- accept/reject only), a result_audits.accepted_result_id reference (AGREED winners only), or
-- an audit_repairs row (DISAGREED→AGREED flips only). work_unit_assignment_history.result_id
-- is ON DELETE SET NULL, so the DELETE nulls those pointers rather than being blocked.
DELETE FROM results
    WHERE validation_status IN ('AWAITING_CONTENT_VERIFICATION', 'CONTENT_VERIFICATION_FAILED');

DROP INDEX IF EXISTS idx_results_content_fetch_due;

ALTER TABLE results
    DROP COLUMN IF EXISTS verified_output_checksum,
    DROP COLUMN IF EXISTS content_fetch_attempts,
    DROP COLUMN IF EXISTS content_fetch_next_attempt_at,
    DROP COLUMN IF EXISTS content_fetch_last_error;

-- The two enum values are left in place (00011 down precedent: PostgreSQL cannot drop an
-- enum value without a type rebuild). An orphaned value is inert for pre-slice-5 code
-- because every validation_status filter in the repo is positive-form.
SELECT 1;
