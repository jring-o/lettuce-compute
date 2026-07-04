-- 00015_rejection_signal: decayed rejection-rate accumulators (BG-24 / BG-24b PR-B) —
-- additive, head-only.
--
-- The automatic standing-backpressure machine folds every ADJUDICATED result
-- (AGREED or DISAGREED — never EXPIRED/ABANDONED, which remain a host-reliability
-- concern) into a pair of exponentially decayed accumulators on the volunteer row,
-- on the same 7-day half-life the host-reliability and RAC signals use. The decayed
-- rejection rate rejection_bad / (rejection_good + rejection_bad) drives standing
-- transitions with hysteresis (OK -> PROBATION -> BENCHED and back) for rows whose
-- standing_source is 'AUTO'; OPERATOR-owned rows accumulate nothing and are never
-- auto-changed (this is the "later migration adds its signal columns" promised by
-- 00014's header comment).
--
-- NULL rejection_updated_at = no adjudicated outcome has been folded yet; the first
-- fold starts from zero elapsed time (no decay). The columns are pure signal state:
-- nothing reads them while LETTUCE_HEAD_STANDING_BACKPRESSURE_ENABLED is false
-- (the default), so this migration is deploy-neutral.
ALTER TABLE public.volunteers
    ADD COLUMN rejection_good double precision NOT NULL DEFAULT 0,
    ADD COLUMN rejection_bad double precision NOT NULL DEFAULT 0,
    ADD COLUMN rejection_updated_at timestamp with time zone;
