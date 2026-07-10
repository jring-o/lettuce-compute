package credit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// RACRepository defines data access for the volunteer_rac table.
type RACRepository interface {
	Upsert(ctx context.Context, volunteerID, leafID types.ID, creditAmount float64) error
	GetByVolunteerProject(ctx context.Context, volunteerID, leafID types.ID) (*RACEntry, error)
	ListByVolunteer(ctx context.Context, volunteerID types.ID) ([]*RACEntry, error)
	ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*RACEntry, types.PaginationResponse, error)
	DecayAll(ctx context.Context) (int64, error)
}

// PgxRACRepository implements RACRepository using pgx.
type PgxRACRepository struct {
	db DBTX
}

// NewPgxRACRepository creates a new PgxRACRepository.
func NewPgxRACRepository(db DBTX) *PgxRACRepository {
	return &PgxRACRepository{db: db}
}

func scanRACEntry(row pgx.Row) (*RACEntry, error) {
	var e RACEntry
	err := row.Scan(
		&e.VolunteerID,
		&e.LeafID,
		&e.RAC,
		&e.TotalCredit,
		&e.LastCreditAt,
		&e.LastUpdatedAt,
		&e.CreatedAt,
	)
	return &e, err
}

const racColumns = `volunteer_id, leaf_id, rac, total_credit, last_credit_at, last_updated_at, created_at`

// Upsert creates or updates a volunteer_rac row atomically. On first credit for a
// volunteer+project pair, a new row is created. On subsequent credits, the
// existing row is updated with the RAC formula applied over the elapsed time.
// Uses INSERT ... ON CONFLICT to avoid race conditions under concurrent access.
func (r *PgxRACRepository) Upsert(ctx context.Context, volunteerID, leafID types.ID, creditAmount float64) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO volunteer_rac (volunteer_id, leaf_id, rac, total_credit, last_credit_at, last_updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (volunteer_id, leaf_id) DO UPDATE SET
			rac = volunteer_rac.rac
				* exp(-EXTRACT(EPOCH FROM (NOW() - volunteer_rac.last_updated_at)) * ln(2) / $5)
				+ $6 * (1 - exp(-EXTRACT(EPOCH FROM (NOW() - volunteer_rac.last_updated_at)) * ln(2) / $5)),
			total_credit = volunteer_rac.total_credit + $7,
			last_credit_at = NOW(),
			last_updated_at = NOW()`,
		volunteerID, leafID, creditAmount, creditAmount,
		float64(HalfLifeSeconds), creditAmount, creditAmount,
	)
	if err != nil {
		return apierror.Internal("failed to upsert volunteer_rac", err)
	}
	return nil
}

// GetByVolunteerProject retrieves a single RAC entry.
func (r *PgxRACRepository) GetByVolunteerProject(ctx context.Context, volunteerID, leafID types.ID) (*RACEntry, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+racColumns+" FROM volunteer_rac WHERE volunteer_id = $1 AND leaf_id = $2",
		volunteerID, leafID,
	)
	e, err := scanRACEntry(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("volunteer_rac", fmt.Sprintf("%s/%s", volunteerID, leafID))
		}
		return nil, apierror.Internal("failed to get volunteer_rac", err)
	}
	return e, nil
}

// ListByVolunteer returns all RAC entries for a volunteer across all projects.
func (r *PgxRACRepository) ListByVolunteer(ctx context.Context, volunteerID types.ID) ([]*RACEntry, error) {
	rows, err := r.db.Query(ctx,
		"SELECT "+racColumns+" FROM volunteer_rac WHERE volunteer_id = $1 ORDER BY rac DESC",
		volunteerID,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list volunteer_rac by volunteer", err)
	}
	defer rows.Close()

	var entries []*RACEntry
	for rows.Next() {
		e, scanErr := scanRACEntry(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan volunteer_rac", scanErr)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate volunteer_rac", err)
	}
	return entries, nil
}

// ListByLeaf returns RAC entries for a project ordered by RAC descending (leaderboard),
// with cursor-based pagination. The cursor encodes (created_at, volunteer_id) — since RAC
// values change over time, we paginate on stable keys and re-sort per page.
func (r *PgxRACRepository) ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*RACEntry, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	args = append(args, leafID)

	query := "SELECT " + racColumns + " FROM volunteer_rac WHERE leaf_id = $1"

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		query += " AND (created_at, volunteer_id) < ($2, $3)"
		args = append(args, cursorTime, cursorID)
	}

	query += fmt.Sprintf(" ORDER BY created_at DESC, volunteer_id DESC LIMIT $%d", len(args)+1)
	args = append(args, pageSize+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list volunteer_rac by project", err)
	}
	defer rows.Close()

	var entries []*RACEntry
	for rows.Next() {
		e, scanErr := scanRACEntry(rows)
		if scanErr != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan volunteer_rac", scanErr)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate volunteer_rac", err)
	}

	pagination := types.PaginationResponse{}
	if len(entries) > pageSize {
		entries = entries[:pageSize]
		last := entries[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.CreatedAt, last.VolunteerID)
	}

	return entries, pagination, nil
}

// DecayAll applies time-based decay to all volunteer_rac rows that haven't been
// updated in the last hour. Returns the number of rows affected.
func (r *PgxRACRepository) DecayAll(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE volunteer_rac
		SET rac = rac * exp(-EXTRACT(EPOCH FROM (NOW() - last_updated_at)) * ln(2) / $1),
			last_updated_at = NOW()
		WHERE last_updated_at < NOW() - INTERVAL '1 hour'
		  AND rac > $2`,
		float64(HalfLifeSeconds),
		1e-9, // Don't bother updating rows with effectively zero RAC.
	)
	if err != nil {
		return 0, apierror.Internal("failed to decay volunteer_rac", err)
	}
	return tag.RowsAffected(), nil
}

// ApplyAdjustment applies the clamped RAC decrement for one committed adjustment
// exactly-once (design doc §9.5) — keel stub; the credit implementer replaces it with
// the single-transaction stamp + decay-subtract-clamp UPDATE.
func (r *PgxRACRepository) ApplyAdjustment(ctx context.Context, adjustmentID types.ID) (bool, error) {
	return false, apierror.Internal("ApplyAdjustment not implemented (slice-3 keel stub)", nil)
}
