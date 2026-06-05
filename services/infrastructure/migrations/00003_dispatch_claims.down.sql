-- 00003_dispatch_claims.down.sql
-- Reverse 00003: drop the dispatch-claim index and columns.

DROP INDEX IF EXISTS public.idx_work_units_dispatch_claim;

ALTER TABLE public.work_units
    DROP COLUMN IF EXISTS dispatch_claim_expires_at,
    DROP COLUMN IF EXISTS dispatch_claimed_by;
