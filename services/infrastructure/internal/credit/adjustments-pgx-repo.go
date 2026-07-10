package credit

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// adjustmentColumns is the projection every read and RETURNING clause shares, in
// scanAdjustment order. audit_id and rac_applied_at (migration 00021) are nullable and
// present on every row: an OPERATOR clawback and an as-yet-unRAC'd adjustment leave them NULL.
const adjustmentColumns = `id, ledger_entry_id, volunteer_id, leaf_id,
	amount, reason, note, created_by, created_at, audit_id, rac_applied_at`

// scanAdjustment scans one Adjustment from a pgx.Row (a QueryRow result or a Rows cursor).
// note is nullable in the schema, so it is scanned through a pointer and flattened to the
// empty string when NULL (which the struct's omitempty tag then drops from JSON). audit_id
// and rac_applied_at are scanned straight into the struct's pointer fields (nil on NULL).
func scanAdjustment(row pgx.Row) (*Adjustment, error) {
	var a Adjustment
	var note *string
	err := row.Scan(
		&a.ID,
		&a.LedgerEntryID,
		&a.VolunteerID,
		&a.LeafID,
		&a.Amount,
		&a.Reason,
		&note,
		&a.CreatedBy,
		&a.CreatedAt,
		&a.AuditID,
		&a.RACAppliedAt,
	)
	if err != nil {
		return nil, err
	}
	if note != nil {
		a.Note = *note
	}
	return &a, nil
}

// PgxAdjustmentsRepository implements AdjustmentsRepository using pgx. It holds the pool
// directly (not the shared DBTX interface) because Clawback needs its own transaction to
// take the FOR UPDATE lock that serializes concurrent adjustments against one entry.
type PgxAdjustmentsRepository struct {
	pool *pgxpool.Pool
}

// NewPgxAdjustmentsRepository creates a new PgxAdjustmentsRepository.
func NewPgxAdjustmentsRepository(pool *pgxpool.Pool) *PgxAdjustmentsRepository {
	return &PgxAdjustmentsRepository{pool: pool}
}

// Clawback appends a guarded negative adjustment against entryID inside ONE transaction, on
// behalf of a manual operator action (audit_id NULL). See clawback for the transactional
// contract.
//
// magnitude is the POSITIVE amount to claw back; nil means "the full remaining net", computed
// inside the same transaction so the read and the write cannot straddle a concurrent
// adjustment (TOCTOU-free). Returns ErrAdjustmentExhausted when nothing remains and
// ErrAdjustmentOvershoot when magnitude exceeds the remaining net.
func (r *PgxAdjustmentsRepository) Clawback(ctx context.Context, entryID types.ID, magnitude *float64, reason, note, createdBy string) (*Adjustment, error) {
	return r.clawback(ctx, entryID, magnitude, reason, note, createdBy, nil)
}

// ClawbackForAudit is the automated-enforcement clawback (design doc §9.4): the same
// transactional FOR-UPDATE net guard as Clawback, ALWAYS full-remaining (magnitude nil,
// resolved under the lock), stamped created_by='AUDIT' and the causing audit_id. reason is a
// package machine code (ReasonAuditMismatch / ReasonAuditMismatchUnmatured); note is empty.
// ErrAdjustmentExhausted is the caller's idempotent no-op (F17): a retrying enforcement pass
// treats an already-cancelled entry as success.
func (r *PgxAdjustmentsRepository) ClawbackForAudit(ctx context.Context, entryID, auditID types.ID, reason string) (*Adjustment, error) {
	return r.clawback(ctx, entryID, nil, reason, "", AdjustmentByAudit, &auditID)
}

// clawback is the single transactional code path behind both Clawback (auditID nil) and
// ClawbackForAudit (auditID set). It locks the parent ledger row, recomputes the committed
// net under that lock, rejects overshoot/exhaustion, and appends one negative adjustment
// carrying the given audit_id.
func (r *PgxAdjustmentsRepository) clawback(ctx context.Context, entryID types.ID, magnitude *float64, reason, note, createdBy string, auditID *types.ID) (*Adjustment, error) {
	// Defensive magnitude check for an explicit request. nil ("full remaining") is resolved
	// below from the locked row and is > 0 by construction. A non-positive or non-finite
	// magnitude is a caller bug, not an overshoot — reject it distinctly.
	if magnitude != nil {
		m := *magnitude
		if math.IsNaN(m) || math.IsInf(m, 0) || m <= 0 {
			return nil, apierror.ValidationError("adjustment magnitude must be a positive, finite number", nil)
		}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, apierror.Internal("failed to begin adjustment transaction", err)
	}
	defer tx.Rollback(ctx)

	// Audit-F1 race fix: lock the parent ledger row FOR UPDATE. The ledger row is never
	// UPDATEd, so this lock serializes ONLY concurrent adjustments against the same entry —
	// two simultaneous full-cancels can no longer each snapshot the pre-commit net and both
	// insert (which a plain aggregate-guarded INSERT would allow under READ COMMITTED). The
	// remaining-net computation below therefore reads a value no concurrent adjustment can
	// change until this transaction commits or rolls back.
	var creditAmount float64
	var volunteerID, leafID types.ID
	err = tx.QueryRow(ctx,
		`SELECT credit_amount, volunteer_id, leaf_id FROM credit_ledger WHERE id = $1 FOR UPDATE`,
		entryID,
	).Scan(&creditAmount, &volunteerID, &leafID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("credit_ledger_entry", entryID.String())
		}
		return nil, apierror.Internal("failed to lock credit ledger entry", err)
	}

	// Sum of adjustments already booked against this entry (<= 0), read under the lock.
	var adjSum float64
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)::float8 FROM credit_adjustments WHERE ledger_entry_id = $1`,
		entryID,
	).Scan(&adjSum)
	if err != nil {
		return nil, apierror.Internal("failed to sum existing adjustments", err)
	}

	remaining := creditAmount + adjSum
	if remaining <= 0 {
		return nil, ErrAdjustmentExhausted
	}

	mag := remaining
	if magnitude != nil {
		mag = *magnitude
	}
	// An adjustment can cancel at most the entry's remaining net; anything larger would drive
	// the exported per-entry net negative.
	if mag > remaining {
		return nil, ErrAdjustmentOvershoot
	}

	var noteArg *string
	if note != "" {
		noteArg = &note
	}

	// volunteer_id/leaf_id are taken from the LOCKED ledger row, never from the caller, so a
	// clawback can never be mis-attributed by a forged request field. audit_id is nil for the
	// operator path (stored NULL) and set for the enforcement path.
	row := tx.QueryRow(ctx,
		`INSERT INTO credit_adjustments (
			ledger_entry_id, volunteer_id, leaf_id, amount, reason, note, created_by, audit_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+adjustmentColumns,
		entryID, volunteerID, leafID, -mag, reason, noteArg, createdBy, auditID,
	)
	adj, err := scanAdjustment(row)
	if err != nil {
		return nil, apierror.Internal("failed to insert credit adjustment", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, apierror.Internal("failed to commit adjustment transaction", err)
	}
	return adj, nil
}

// ListByVolunteer returns a volunteer's adjustments, newest first. limit is clamped to
// [1, 1000] (default 100 when non-positive); offset is floored at 0.
func (r *PgxAdjustmentsRepository) ListByVolunteer(ctx context.Context, volunteerID types.ID, limit, offset int) ([]*Adjustment, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.pool.Query(ctx,
		`SELECT `+adjustmentColumns+`
		FROM credit_adjustments
		WHERE volunteer_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`,
		volunteerID, limit, offset,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list credit adjustments", err)
	}
	defer rows.Close()

	var out []*Adjustment
	for rows.Next() {
		a, scanErr := scanAdjustment(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan credit adjustment", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate credit adjustments", err)
	}
	return out, nil
}

// SumForEntry returns the (negative or zero) sum of an entry's adjustments.
func (r *PgxAdjustmentsRepository) SumForEntry(ctx context.Context, entryID types.ID) (float64, error) {
	var sum float64
	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)::float8 FROM credit_adjustments WHERE ledger_entry_id = $1`,
		entryID,
	).Scan(&sum)
	if err != nil {
		return 0, apierror.Internal("failed to sum adjustments for entry", err)
	}
	return sum, nil
}

// ListUnmaturedEntryIDs returns the ids of the volunteer's ledger entries still inside the
// maturation window (granted_at > now() - maturationDays), oldest first, riding
// idx_credit_ledger_volunteer_time (design doc §9.4). It DELIBERATELY does NOT pre-filter on
// remaining net: ClawbackForAudit recomputes net under its FOR-UPDATE lock and no-ops the
// already-cancelled entries, so there is one race-safe code path rather than a TOCTOU-prone
// pre-filter (the F1/F9 lesson).
func (r *PgxAdjustmentsRepository) ListUnmaturedEntryIDs(ctx context.Context, volunteerID types.ID, maturationDays int) ([]types.ID, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id FROM credit_ledger
		WHERE volunteer_id = $1 AND granted_at > now() - ($2::int * interval '1 day')
		ORDER BY granted_at ASC`,
		volunteerID, maturationDays,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list unmatured ledger entries", err)
	}
	defer rows.Close()

	var ids []types.ID
	for rows.Next() {
		var id types.ID
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, apierror.Internal("failed to scan unmatured ledger entry id", scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate unmatured ledger entries", err)
	}
	return ids, nil
}
