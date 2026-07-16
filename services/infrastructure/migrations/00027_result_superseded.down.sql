-- 00027_result_superseded down.
--
-- Rows are converted rather than deleted (less destructive than the 00022 down's DELETE):
-- DISAGREED is the nearest pre-00027 status for "did not corroborate the unit's outcome",
-- and every such row sits under a FAILED unit whose copy budget is already spent, so the
-- retroactive error-count inflation (errorCopiesSQL counts DISAGREED) is inert — nothing
-- re-dispatches or re-decides a FAILED unit.
UPDATE results SET validation_status = 'DISAGREED', updated_at = now()
    WHERE validation_status = 'SUPERSEDED';

-- The enum value itself is left in place (00011/00022 down precedent: PostgreSQL cannot drop
-- an enum value without a full type rebuild). An orphaned value is inert for pre-00027 code
-- because every validation_status filter in the repo is positive-form.
SELECT 1;
