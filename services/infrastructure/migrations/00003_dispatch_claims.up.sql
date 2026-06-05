-- 00003_dispatch_claims.up.sql
-- Add per-head dispatch CLAIM columns for horizontal scale-out (Layer 3).
--
-- With N stateless head replicas behind a reverse proxy against one shared
-- Postgres, each replica's in-process dispatch cache bulk-refills QUEUED units
-- into its own in-memory ready pool. The reservation guard (00002) only hides a
-- unit AFTER its reservation is flushed, which lands AFTER the unit is handed
-- out — so two replicas would otherwise stage and hand out the SAME QUEUED unit
-- (a cross-replica double-hand).
--
-- The dispatch claim closes that gap with claim-on-refill: the bulk refill
-- atomically stamps a per-head claim (dispatch_claimed_by = the claiming head's
-- instance id, dispatch_claim_expires_at = a short lease in the future) on each
-- staged unit. A unit carrying a LIVE claim owned by another head is invisible
-- to that head's refill, so only the owner stages and hands it out. The unit
-- stays state='QUEUED' until run-start; the claim is a SECOND, head-owned lease
-- distinct from the volunteer reservation (00002).
--
-- The claim is amortized at bulk-refill (one UPDATE per LIMIT-N refill), keeping
-- the per-request hand-out hot path free of any DB write. A held unit's claim is
-- renewed off the hot path by the async reservation flush. A crashed replica's
-- claims simply EXPIRE (dispatch_claim_expires_at < NOW()) and become
-- re-claimable by any survivor on its next refill — passive expiry is the
-- reclaim guarantee, no active sweep is required for correctness.

ALTER TABLE public.work_units
    ADD COLUMN dispatch_claimed_by       uuid,
    ADD COLUMN dispatch_claim_expires_at timestamp with time zone;

-- Partial index over only the claimed rows. It drives two predicates cheaply:
--   * the refill exclude/re-claim test (another head's LIVE claim is skipped;
--     an expired claim or this head's own claim is re-claimable), and
--   * the leader-gated hygiene sweep that NULLs expired claims to keep the table
--     tidy and observable.
-- Leading on dispatch_claim_expires_at keeps the expiry-range scans index-driven.
CREATE INDEX idx_work_units_dispatch_claim
    ON public.work_units USING btree (dispatch_claim_expires_at)
    WHERE (dispatch_claimed_by IS NOT NULL);
