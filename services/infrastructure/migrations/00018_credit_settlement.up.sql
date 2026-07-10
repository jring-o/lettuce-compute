-- 00018_credit_settlement: foundation of the credit settlement layer.
--
-- credit_adjustments holds compensating NEGATIVE entries against individual credit_ledger
-- rows. The ledger itself stays append-only (CHECK credit_amount > 0, one row per AGREED
-- result): a clawback never deletes or mutates a grant — it appends an adjustment
-- referencing it, and external exports serve the per-entry net
-- (entry.credit_amount + SUM(its adjustments)).
--
-- Invariant (enforced in the repository's transactional create, which locks the parent
-- ledger row FOR UPDATE; SQL cannot CHECK across rows): for every ledger entry,
-- credit_amount + SUM(its adjustments) >= 0 — an adjustment can cancel at most the entry.
--
-- Additive and deploy-neutral: nothing reads these objects until the settlement code
-- paths ship, and every settlement knob defaults to current behavior.

CREATE TABLE credit_adjustments (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_entry_id  uuid NOT NULL REFERENCES credit_ledger(id) ON DELETE RESTRICT,
    -- Denormalized from the (immutable) parent entry for the admin list filter and the
    -- future per-account RAC correction. NEVER use these for export netting: the export
    -- nets strictly per-entry via ledger_entry_id, so a clawback of an immature or
    -- inactive-leaf entry can never leak into matured totals.
    volunteer_id     uuid NOT NULL,
    leaf_id          uuid NOT NULL,
    amount           numeric(18,6) NOT NULL CHECK (amount < 0),
    reason           varchar(64) NOT NULL,
    note             text,
    created_by       varchar(16) NOT NULL CHECK (created_by IN ('OPERATOR', 'AUDIT')),
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_credit_adjustments_entry ON credit_adjustments (ledger_entry_id);
CREATE INDEX idx_credit_adjustments_volunteer ON credit_adjustments (volunteer_id, created_at);

-- The emission-cap and anomaly-halt checks are time-range sums over the whole ledger;
-- every existing credit_ledger index leads with volunteer_id or leaf_id, so a global
-- 24h/30d range sum would seq-scan without this.
CREATE INDEX idx_credit_ledger_granted_at ON credit_ledger (granted_at) INCLUDE (credit_amount);
