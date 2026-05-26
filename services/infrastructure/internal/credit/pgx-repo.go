package credit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// DBTX is the common interface satisfied by *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const ledgerColumns = `id, volunteer_id, leaf_id, work_unit_id, result_id,
	credit_amount, granted_at, created_at`

func scanLedgerEntry(row pgx.Row) (*LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(
		&e.ID,
		&e.VolunteerID,
		&e.LeafID,
		&e.WorkUnitID,
		&e.ResultID,
		&e.CreditAmount,
		&e.GrantedAt,
		&e.CreatedAt,
	)
	return &e, err
}

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// Create inserts a new credit ledger entry.
// On return, entry is populated with the DB-generated id and timestamps.
func (r *PgxRepository) Create(ctx context.Context, entry *LedgerEntry) error {
	row := r.db.QueryRow(ctx, `
		INSERT INTO credit_ledger (
			volunteer_id, leaf_id, work_unit_id, result_id, credit_amount
		) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+ledgerColumns,
		entry.VolunteerID, entry.LeafID, entry.WorkUnitID,
		entry.ResultID, entry.CreditAmount,
	)

	created, err := scanLedgerEntry(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" {
				return apierror.Conflict(
					"credit already granted for this result",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
			if pgErr.Code == "23503" {
				return apierror.Conflict(
					"referenced entity does not exist",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
		}
		return apierror.Internal("failed to create credit ledger entry", err)
	}
	*entry = *created
	return nil
}

// GetByResultID retrieves a credit ledger entry by its result ID.
func (r *PgxRepository) GetByResultID(ctx context.Context, resultID types.ID) (*LedgerEntry, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+ledgerColumns+" FROM credit_ledger WHERE result_id = $1", resultID)

	e, err := scanLedgerEntry(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("credit_ledger_entry", resultID.String())
		}
		return nil, apierror.Internal("failed to get credit ledger entry by result", err)
	}
	return e, nil
}

// SumByVolunteerProject returns the total credit granted to a volunteer for a project.
func (r *PgxRepository) SumByVolunteerProject(ctx context.Context, volunteerID, leafID types.ID) (float64, error) {
	var total float64
	err := r.db.QueryRow(ctx,
		"SELECT COALESCE(SUM(credit_amount), 0)::float8 FROM credit_ledger WHERE volunteer_id = $1 AND leaf_id = $2",
		volunteerID, leafID,
	).Scan(&total)
	if err != nil {
		return 0, apierror.Internal("failed to sum credit by volunteer and project", err)
	}
	return total, nil
}

// CountByVolunteerPerProject returns the count of credit entries per project for a volunteer.
func (r *PgxRepository) CountByVolunteerPerProject(ctx context.Context, volunteerID types.ID) (map[types.ID]int, error) {
	rows, err := r.db.Query(ctx,
		"SELECT leaf_id, COUNT(*)::int FROM credit_ledger WHERE volunteer_id = $1 GROUP BY leaf_id",
		volunteerID,
	)
	if err != nil {
		return nil, apierror.Internal("failed to count credit entries by volunteer per project", err)
	}
	defer rows.Close()

	counts := make(map[types.ID]int)
	for rows.Next() {
		var leafID types.ID
		var count int
		if err := rows.Scan(&leafID, &count); err != nil {
			return nil, apierror.Internal("failed to scan credit count", err)
		}
		counts[leafID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate credit counts", err)
	}
	return counts, nil
}

// ListByVolunteer retrieves credit ledger entries for a volunteer with cursor-based pagination,
// ordered by granted_at DESC.
func (r *PgxRepository) ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error) {
	return r.listByColumn(ctx, "volunteer_id", volunteerID, page)
}

// ListByLeaf retrieves credit ledger entries for a project with cursor-based pagination,
// ordered by granted_at DESC.
func (r *PgxRepository) ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error) {
	return r.listByColumn(ctx, "leaf_id", leafID, page)
}

func (r *PgxRepository) listByColumn(ctx context.Context, column string, id types.ID, page types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	args = append(args, id)

	query := "SELECT " + ledgerColumns + " FROM credit_ledger WHERE " + column + " = $1"

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		query += " AND (granted_at, id) < ($2, $3)"
		args = append(args, cursorTime, cursorID)
	}

	query += fmt.Sprintf(" ORDER BY granted_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, pageSize+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list credit entries by "+column, err)
	}
	defer rows.Close()

	var entries []*LedgerEntry
	for rows.Next() {
		e, scanErr := scanLedgerEntry(rows)
		if scanErr != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan credit entry", scanErr)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate credit entries", err)
	}

	pagination := types.PaginationResponse{}
	if len(entries) > pageSize {
		entries = entries[:pageSize]
		last := entries[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.GrantedAt, last.ID)
	}

	return entries, pagination, nil
}
