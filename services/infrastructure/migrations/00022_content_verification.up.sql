-- 00022_content_verification: slice-5 fetch-and-verify (design doc §10.1; BG-02b).
--
-- Two new results states. ADD VALUE is transactional on PostgreSQL 12+ (the head runs 16;
-- 00011 precedent) — but a value added here CANNOT be referenced by any other statement in
-- this same migration (PG12+ rule), so everything below is deliberately enum-literal-free.
-- AWAITING_CONTENT_VERIFICATION = a ref-only result held pre-vote while the head fetches and
-- hashes the external bytes; CONTENT_VERIFICATION_FAILED = terminal did-not-become-votable
-- (reason-coded in content_fetch_last_error; includes fetch failure and expiry — the STATE is
-- the pipeline outcome). Both values are fail-closed at every existing site because every
-- validation_status filter in the repo is positive-form.
ALTER TYPE public.validation_status ADD VALUE IF NOT EXISTS 'AWAITING_CONTENT_VERIFICATION';
ALTER TYPE public.validation_status ADD VALUE IF NOT EXISTS 'CONTENT_VERIFICATION_FAILED';

-- Verification + fetch bookkeeping. verified_output_checksum is the HEAD-computed sha256 of
-- the bytes actually fetched — the ONLY checksum a ref result may ever vote on (§10.8); it
-- being non-NULL is also the "the head hashed these bytes" flag that comparisonKey keys on.
-- content_fetch_attempts counts TRANSIENT fetch failures only (a successful fetch always
-- promotes on the served hash, §10.6). content_fetch_next_attempt_at IS NOT NULL <=> the row
-- is awaiting a fetch attempt (set at submit, advanced on transient retry, CLEARED on
-- promotion and on every terminal disposition) — this column, not the enum value, is the
-- worker-scan predicate.
ALTER TABLE results
    ADD COLUMN verified_output_checksum      varchar(64),
    ADD COLUMN content_fetch_attempts        integer NOT NULL DEFAULT 0,
    ADD COLUMN content_fetch_next_attempt_at timestamptz,
    ADD COLUMN content_fetch_last_error      text;

-- Worker scan: due rows. Enum-literal-free by necessity (same-transaction rule above) and by
-- economy (the IS NOT NULL invariant keeps this index at exactly the open-fetch set). The
-- worker SELECTs against it with FOR UPDATE SKIP LOCKED (§10.6) so a two-leader failover
-- window cannot double-process a row.
CREATE INDEX idx_results_content_fetch_due ON public.results (content_fetch_next_attempt_at)
    WHERE content_fetch_next_attempt_at IS NOT NULL;
