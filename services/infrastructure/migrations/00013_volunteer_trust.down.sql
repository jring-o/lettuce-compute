-- 00013_volunteer_trust.down.sql
-- Reverse 00013: drop the per-result submission-time trust snapshot columns and the
-- volunteer_trust table. Purely additive feature with no foreign keys, so removing it
-- simply reverts results to no trust snapshot and drops the account-level trust store;
-- volunteers and their keypair/DID identity are untouched.

ALTER TABLE public.results
    DROP COLUMN IF EXISTS trust_score_at_submit,
    DROP COLUMN IF EXISTS trust_subject;

DROP TABLE IF EXISTS public.volunteer_trust;
