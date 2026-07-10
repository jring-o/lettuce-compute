package credit

import (
	"context"
	"errors"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Adjustment is a compensating NEGATIVE entry against exactly one credit_ledger row.
// The ledger stays append-only; a clawback appends an adjustment referencing the grant it
// unwinds, and external exports serve the per-entry net (see migration 00018). Rows are
// never updated or deleted.
type Adjustment struct {
	ID            types.ID  `json:"id"`
	LedgerEntryID types.ID  `json:"ledger_entry_id"`
	VolunteerID   types.ID  `json:"volunteer_id"`
	LeafID        types.ID  `json:"leaf_id"`
	Amount        float64   `json:"amount"` // always < 0
	Reason        string    `json:"reason"`
	Note          string    `json:"note,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// created_by values: who initiated an adjustment. OPERATOR = the manual admin clawback
// endpoint; AUDIT is reserved for the automated re-execution-audit clawback (a later PR —
// its idempotency contract against ErrAdjustmentExhausted is pinned below NOW so the two
// callers cannot diverge).
const (
	AdjustmentByOperator = "OPERATOR"
	AdjustmentByAudit    = "AUDIT"
)

// ErrAdjustmentExhausted: the ledger entry is already fully adjusted (remaining net is 0).
// The admin endpoint maps this to 409; the future automated clawback treats it as an
// idempotent no-op ("already clawed back" is success for a retrying auditor).
var ErrAdjustmentExhausted = errors.New("ledger entry already fully adjusted")

// ErrAdjustmentOvershoot: the requested magnitude exceeds the entry's remaining net. An
// adjustment can cancel at most its entry — the invariant that keeps every exported
// per-entry (and therefore per-account) net non-negative. Mapped to 409 by the endpoint.
var ErrAdjustmentOvershoot = errors.New("adjustment exceeds the entry's remaining credit")

// AdjustmentsRepository is the data-access interface for credit adjustments.
type AdjustmentsRepository interface {
	// Clawback appends a guarded adjustment against entryID inside ONE transaction: it
	// locks the parent ledger row (SELECT ... FOR UPDATE — the row is never updated, so
	// this serializes only same-entry adjustments), recomputes the committed net,
	// rejects overshoot, and inserts. A plain aggregate-guarded INSERT is NOT race-safe
	// under READ COMMITTED (two concurrent full-cancels each see the pre-commit net and
	// both insert), which is why this is transactional.
	//
	// magnitude is the POSITIVE amount to claw back; nil means "the full remaining net",
	// computed inside the same transaction. Returns ErrAdjustmentExhausted when nothing
	// remains and ErrAdjustmentOvershoot when magnitude exceeds the remaining net.
	Clawback(ctx context.Context, entryID types.ID, magnitude *float64, reason, note, createdBy string) (*Adjustment, error)
	// ListByVolunteer returns a volunteer's adjustments, newest first.
	ListByVolunteer(ctx context.Context, volunteerID types.ID, limit, offset int) ([]*Adjustment, error)
	// SumForEntry returns the (negative or zero) sum of an entry's adjustments.
	SumForEntry(ctx context.Context, entryID types.ID) (float64, error)
}

// CappedCreator is the optional capability the validation engine type-asserts on its
// credit repository when a per-account daily emission cap is configured. It is a distinct
// method from Create because suppression must be a NON-ERROR branch: routing a capped
// grant through Create's 0-row error path would abort the whole validation, turning an
// over-cap honest burst into a validation failure.
type CappedCreator interface {
	// CreateCapped inserts entry unless the volunteer's granted sum over the trailing
	// 24h window (rolling, DB clock) plus this amount would exceed capPerDay. The
	// check + insert ride one SQL statement to narrow (not close) the concurrency
	// window: the cap is an anomaly bound, not an accounting invariant, and the
	// worst-case overshoot is bounded by concurrent same-account validations of
	// distinct units. Returns inserted=false with a nil error on suppression; a
	// non-nil error only on real failures.
	CreateCapped(ctx context.Context, entry *LedgerEntry, capPerDay float64) (inserted bool, err error)
}
