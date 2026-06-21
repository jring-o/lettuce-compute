package assignment

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

const assignmentColumns = `id, work_unit_id, volunteer_id, assigned_at,
	outcome, outcome_at, result_id, host_id, created_at`

func scanAssignment(row pgx.Row) (*AssignmentHistoryEntry, error) {
	var e AssignmentHistoryEntry
	err := row.Scan(
		&e.ID,
		&e.WorkUnitID,
		&e.VolunteerID,
		&e.AssignedAt,
		&e.Outcome,
		&e.OutcomeAt,
		&e.ResultID,
		&e.HostID,
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

// Create inserts a new assignment history entry. On return, entry is populated
// with DB-generated id and timestamps.
func (r *PgxRepository) Create(ctx context.Context, entry *AssignmentHistoryEntry) error {
	row := r.db.QueryRow(ctx, `
		INSERT INTO work_unit_assignment_history (
			work_unit_id, volunteer_id, assigned_at, outcome, outcome_at, result_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+assignmentColumns,
		entry.WorkUnitID, entry.VolunteerID, entry.AssignedAt,
		entry.Outcome, entry.OutcomeAt, entry.ResultID,
	)

	result, err := scanAssignment(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return apierror.Conflict(
				"referenced entity does not exist",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to create assignment history entry", err)
	}
	*entry = *result
	return nil
}

// GetByID retrieves an assignment history entry by its UUID.
func (r *PgxRepository) GetByID(ctx context.Context, id types.ID) (*AssignmentHistoryEntry, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+assignmentColumns+" FROM work_unit_assignment_history WHERE id = $1", id)

	entry, err := scanAssignment(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("assignment_history", id.String())
		}
		return nil, apierror.Internal("failed to get assignment history entry", err)
	}
	return entry, nil
}

// ListByWorkUnit retrieves all assignment history entries for a work unit,
// ordered by assigned_at DESC.
func (r *PgxRepository) ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*AssignmentHistoryEntry, error) {
	rows, err := r.db.Query(ctx,
		"SELECT "+assignmentColumns+" FROM work_unit_assignment_history WHERE work_unit_id = $1 ORDER BY assigned_at DESC",
		workUnitID,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list assignment history by work unit", err)
	}
	defer rows.Close()

	var entries []*AssignmentHistoryEntry
	for rows.Next() {
		entry, err := scanAssignment(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan assignment history entry", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate assignment history", err)
	}
	return entries, nil
}

// ListByVolunteer retrieves assignment history entries for a volunteer
// with cursor-based pagination, ordered by assigned_at DESC.
func (r *PgxRepository) ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*AssignmentHistoryEntry, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	args = append(args, volunteerID)

	query := "SELECT " + assignmentColumns + " FROM work_unit_assignment_history WHERE volunteer_id = $1"

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		query += " AND (assigned_at, id) < ($2, $3)"
		args = append(args, cursorTime, cursorID)
	}

	query += fmt.Sprintf(" ORDER BY assigned_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, pageSize+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list assignment history by volunteer", err)
	}
	defer rows.Close()

	var entries []*AssignmentHistoryEntry
	for rows.Next() {
		entry, err := scanAssignment(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan assignment history entry", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate assignment history", err)
	}

	pagination := types.PaginationResponse{}
	if len(entries) > pageSize {
		entries = entries[:pageSize]
		last := entries[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.AssignedAt, last.ID)
	}

	return entries, pagination, nil
}

// CountActiveByWorkUnit returns the number of active (non-terminal) assignments
// for a work unit. An assignment is active if outcome IS NULL.
func (r *PgxRepository) CountActiveByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error) {
	var count int
	err := r.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
		workUnitID,
	).Scan(&count)
	if err != nil {
		return 0, apierror.Internal("failed to count active assignments", err)
	}
	return count, nil
}

// FindActiveByWorkUnitAndVolunteer returns the active assignment (outcome IS NULL)
// for a given work unit and volunteer. Returns apierror.NotFound if none exists.
func (r *PgxRepository) FindActiveByWorkUnitAndVolunteer(ctx context.Context, workUnitID, volunteerID types.ID) (*AssignmentHistoryEntry, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+assignmentColumns+" FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL",
		workUnitID, volunteerID,
	)

	entry, err := scanAssignment(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("active_assignment", fmt.Sprintf("work_unit=%s volunteer=%s", workUnitID, volunteerID))
		}
		return nil, apierror.Internal("failed to find active assignment", err)
	}
	return entry, nil
}

// UpdateOutcome sets the outcome, outcome_at, and optionally result_id for an
// assignment history entry. outcome_at is set to NOW().
func (r *PgxRepository) UpdateOutcome(ctx context.Context, id types.ID, outcome AssignmentOutcome, resultID *types.ID) error {
	tag, err := r.db.Exec(ctx,
		"UPDATE work_unit_assignment_history SET outcome = $2, outcome_at = NOW(), result_id = $3 WHERE id = $1",
		id, outcome, resultID,
	)
	if err != nil {
		return apierror.Internal("failed to update assignment outcome", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("assignment_history", id.String())
	}
	return nil
}
