-- 00012_volunteer_did.down.sql
-- Reverse 00012: drop the optional ATProto DID identity-binding columns and the
-- partial DID index from volunteers. The did_binding_status CHECK constraint is
-- dropped automatically with its column. Purely additive feature, so removing it
-- simply reverts volunteers to keypair-only identity with no binding metadata.

DROP INDEX IF EXISTS public.idx_volunteers_did;

ALTER TABLE public.volunteers
    DROP COLUMN IF EXISTS did_frozen_until,
    DROP COLUMN IF EXISTS did_binding_check_failures,
    DROP COLUMN IF EXISTS did_binding_checked_at,
    DROP COLUMN IF EXISTS did_bound_at,
    DROP COLUMN IF EXISTS did_binding_status,
    DROP COLUMN IF EXISTS did_binding_cid,
    DROP COLUMN IF EXISTS did_binding_uri,
    DROP COLUMN IF EXISTS did;
