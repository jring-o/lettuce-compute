-- 00023_user_token_version down.
--
-- Drop the session-revocation column. Additive-only up, so the down is a clean
-- column drop; a code rollback does not require running this (the column is
-- inert to pre-cluster code).
ALTER TABLE public.users
    DROP COLUMN IF EXISTS token_version;
