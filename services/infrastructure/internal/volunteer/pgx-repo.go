package volunteer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	pool *pgxpool.Pool
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(pool *pgxpool.Pool) *PgxRepository {
	return &PgxRepository{pool: pool}
}

// volunteerColumns is the standard column list for SELECT queries.
const volunteerColumns = `id, numeric_id, public_key, user_id, display_name,
	hardware_capabilities, available_runtimes, scheduling_mode, schedule_config,
	is_active, last_seen_at,
	total_work_units_completed, total_work_units_rejected,
	registered_at, created_at, updated_at`

// scanVolunteer scans a volunteer row into a Volunteer struct.
func scanVolunteer(row pgx.Row) (*Volunteer, error) {
	var v Volunteer
	err := row.Scan(
		&v.ID,
		&v.NumericID,
		&v.PublicKey,
		&v.UserID,
		&v.DisplayName,
		&v.HardwareCapabilities,
		&v.AvailableRuntimes,
		&v.SchedulingMode,
		&v.ScheduleConfig,
		&v.IsActive,
		&v.LastSeenAt,
		&v.TotalWorkUnitsCompleted,
		&v.TotalWorkUnitsRejected,
		&v.RegisteredAt,
		&v.CreatedAt,
		&v.UpdatedAt,
	)
	return &v, err
}

// Create inserts a new volunteer.
// On return, v is populated with the DB-generated id and timestamps.
func (r *PgxRepository) Create(ctx context.Context, v *Volunteer) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO volunteers (
			public_key, user_id, display_name,
			hardware_capabilities, available_runtimes, scheduling_mode, schedule_config,
			is_active, last_seen_at
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7,
			$8, $9
		) RETURNING `+volunteerColumns,
		v.PublicKey, v.UserID, v.DisplayName,
		v.HardwareCapabilities, v.AvailableRuntimes, v.SchedulingMode, v.ScheduleConfig,
		v.IsActive, v.LastSeenAt,
	)

	result, err := scanVolunteer(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return apierror.Conflict(
				"volunteer with this public key already exists",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to create volunteer", err)
	}
	*v = *result
	return nil
}

// GetByID retrieves a volunteer by its UUID.
func (r *PgxRepository) GetByID(ctx context.Context, id types.ID) (*Volunteer, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+volunteerColumns+" FROM volunteers WHERE id = $1", id)

	v, err := scanVolunteer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("volunteer", id.String())
		}
		return nil, apierror.Internal("failed to get volunteer", err)
	}
	return v, nil
}

// GetByPublicKey retrieves a volunteer by Ed25519 public key.
func (r *PgxRepository) GetByPublicKey(ctx context.Context, publicKey []byte) (*Volunteer, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+volunteerColumns+" FROM volunteers WHERE public_key = $1", publicKey)

	v, err := scanVolunteer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("volunteer", "public_key")
		}
		return nil, apierror.Internal("failed to get volunteer by public key", err)
	}
	return v, nil
}

// GetByUserID retrieves a volunteer by their linked platform user ID.
func (r *PgxRepository) GetByUserID(ctx context.Context, userID types.ID) (*Volunteer, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+volunteerColumns+" FROM volunteers WHERE user_id = $1", userID)

	v, err := scanVolunteer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("volunteer", "user_id="+userID.String())
		}
		return nil, apierror.Internal("failed to get volunteer by user_id", err)
	}
	return v, nil
}

// Update modifies an existing volunteer's mutable fields.
func (r *PgxRepository) Update(ctx context.Context, v *Volunteer) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volunteers SET
			display_name = $2,
			hardware_capabilities = $3,
			available_runtimes = $4,
			scheduling_mode = $5,
			schedule_config = $6,
			is_active = $7,
			last_seen_at = $8
		WHERE id = $1`,
		v.ID,
		v.DisplayName,
		v.HardwareCapabilities,
		v.AvailableRuntimes,
		v.SchedulingMode,
		v.ScheduleConfig,
		v.IsActive,
		v.LastSeenAt,
	)
	if err != nil {
		return apierror.Internal("failed to update volunteer", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", v.ID.String())
	}

	// Re-read to get updated_at from the trigger.
	updated, err := r.GetByID(ctx, v.ID)
	if err != nil {
		return err
	}
	*v = *updated
	return nil
}

// UpdateLastSeen updates the last_seen_at timestamp to NOW().
func (r *PgxRepository) UpdateLastSeen(ctx context.Context, id types.ID) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET last_seen_at = NOW() WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to update last seen", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", id.String())
	}
	return nil
}

// SetActive sets the is_active flag for a volunteer.
func (r *PgxRepository) SetActive(ctx context.Context, id types.ID, active bool) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET is_active = $2 WHERE id = $1", id, active)
	if err != nil {
		return apierror.Internal("failed to set active", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", id.String())
	}
	return nil
}

// IncrementWorkUnitsCompleted atomically increments total_work_units_completed by 1.
func (r *PgxRepository) IncrementWorkUnitsCompleted(ctx context.Context, id types.ID) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET total_work_units_completed = total_work_units_completed + 1 WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to increment work units completed", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", id.String())
	}
	return nil
}

// IncrementWorkUnitsRejected atomically increments total_work_units_rejected by 1.
func (r *PgxRepository) IncrementWorkUnitsRejected(ctx context.Context, id types.ID) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET total_work_units_rejected = total_work_units_rejected + 1 WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to increment work units rejected", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", id.String())
	}
	return nil
}

// MarkInactiveOlderThan sets is_active = false for all volunteers
// whose last_seen_at < NOW() - threshold. Returns count of updated rows.
func (r *PgxRepository) MarkInactiveOlderThan(ctx context.Context, threshold time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-threshold)
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET is_active = false WHERE is_active = true AND last_seen_at < $1",
		cutoff,
	)
	if err != nil {
		return 0, apierror.Internal("failed to mark inactive volunteers", err)
	}
	return int(tag.RowsAffected()), nil
}

// List retrieves volunteers with optional filters and cursor-based pagination.
func (r *PgxRepository) List(ctx context.Context, filters VolunteerListFilters, page types.PaginationRequest) ([]*Volunteer, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var conditions []string
	var args []any
	argIdx := 1

	if filters.IsActive != nil {
		conditions = append(conditions, fmt.Sprintf("is_active = $%d", argIdx))
		args = append(args, *filters.IsActive)
		argIdx++
	}
	if filters.SchedulingMode != nil {
		conditions = append(conditions, fmt.Sprintf("scheduling_mode = $%d", argIdx))
		args = append(args, *filters.SchedulingMode)
		argIdx++
	}

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, cursorTime, cursorID)
		argIdx += 2
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf("SELECT %s FROM volunteers %s ORDER BY created_at DESC, id DESC LIMIT $%d",
		volunteerColumns, where, argIdx)
	args = append(args, pageSize+1)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list volunteers", err)
	}
	defer rows.Close()

	var volunteers []*Volunteer
	for rows.Next() {
		v, scanErr := scanVolunteer(rows)
		if scanErr != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan volunteer", scanErr)
		}
		volunteers = append(volunteers, v)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate volunteers", err)
	}

	pagination := types.PaginationResponse{}
	if len(volunteers) > pageSize {
		volunteers = volunteers[:pageSize]
		last := volunteers[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.CreatedAt, last.ID)
	}

	return volunteers, pagination, nil
}
