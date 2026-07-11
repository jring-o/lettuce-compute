package result

import (
	"context"
	"encoding/json"
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

const resultColumns = `id, work_unit_id, volunteer_id, output_data, output_data_ref,
	output_checksum, execution_metadata, validation_status,
	submitted_at, validated_at, created_at, updated_at, artifact_version_id, host_id,
	trust_subject, trust_score_at_submit, standing_at_submit,
	verified_output_checksum, content_fetch_attempts, content_fetch_next_attempt_at,
	content_fetch_last_error`

// prefixedResultColumns is resultColumns with an r. table alias prefix, for
// queries that JOIN results against another table (e.g. ListByLeaf). Keep it in
// lockstep with resultColumns — TestResultColumnsParity enforces that.
const prefixedResultColumns = `r.id, r.work_unit_id, r.volunteer_id, r.output_data, r.output_data_ref,
	r.output_checksum, r.execution_metadata, r.validation_status,
	r.submitted_at, r.validated_at, r.created_at, r.updated_at, r.artifact_version_id, r.host_id,
	r.trust_subject, r.trust_score_at_submit, r.standing_at_submit,
	r.verified_output_checksum, r.content_fetch_attempts, r.content_fetch_next_attempt_at,
	r.content_fetch_last_error`

func scanResult(row pgx.Row) (*Result, error) {
	var r Result
	var metadataJSON []byte
	err := row.Scan(
		&r.ID,
		&r.WorkUnitID,
		&r.VolunteerID,
		&r.OutputData,
		&r.OutputDataRef,
		&r.OutputChecksum,
		&metadataJSON,
		&r.ValidationStatus,
		&r.SubmittedAt,
		&r.ValidatedAt,
		&r.CreatedAt,
		&r.UpdatedAt,
		&r.ArtifactVersionID,
		&r.HostID,
		&r.TrustSubject,
		&r.TrustScoreAtSubmit,
		&r.StandingAtSubmit,
		&r.VerifiedOutputChecksum,
		&r.ContentFetchAttempts,
		&r.ContentFetchNextAttemptAt,
		&r.ContentFetchLastError,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(metadataJSON, &r.ExecutionMetadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal execution_metadata: %w", err)
	}
	return &r, nil
}

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// Create inserts a new result. On return, r is populated with DB-generated id and timestamps.
func (repo *PgxRepository) Create(ctx context.Context, r *Result) error {
	metadataJSON, err := json.Marshal(r.ExecutionMetadata)
	if err != nil {
		return apierror.Internal("failed to marshal execution_metadata", err)
	}

	// verified_output_checksum, content_fetch_attempts, and content_fetch_last_error
	// are never set at creation (a ref result is inserted HELD with defaults; the
	// fetch worker owns every later write to them), so only the worker-scan
	// timestamp rides the INSERT.
	row := repo.db.QueryRow(ctx, `
		INSERT INTO results (
			work_unit_id, volunteer_id, output_data, output_data_ref,
			output_checksum, execution_metadata, validation_status, artifact_version_id, host_id,
			trust_subject, trust_score_at_submit, standing_at_submit, content_fetch_next_attempt_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+resultColumns,
		r.WorkUnitID, r.VolunteerID, r.OutputData, r.OutputDataRef,
		r.OutputChecksum, metadataJSON, r.ValidationStatus, r.ArtifactVersionID, r.HostID,
		r.TrustSubject, r.TrustScoreAtSubmit, r.StandingAtSubmit, r.ContentFetchNextAttemptAt,
	)

	created, err := scanResult(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" {
				return apierror.Conflict(
					"result already exists for this work unit and volunteer",
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
		return apierror.Internal("failed to create result", err)
	}
	*r = *created
	return nil
}

// GetByID retrieves a result by its UUID.
func (repo *PgxRepository) GetByID(ctx context.Context, id types.ID) (*Result, error) {
	row := repo.db.QueryRow(ctx,
		"SELECT "+resultColumns+" FROM results WHERE id = $1", id)

	r, err := scanResult(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("result", id.String())
		}
		return nil, apierror.Internal("failed to get result", err)
	}
	return r, nil
}

// ListByWorkUnit retrieves all results for a work unit, ordered by submitted_at ASC.
func (repo *PgxRepository) ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*Result, error) {
	rows, err := repo.db.Query(ctx,
		"SELECT "+resultColumns+" FROM results WHERE work_unit_id = $1 ORDER BY submitted_at ASC",
		workUnitID,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list results by work unit", err)
	}
	defer rows.Close()

	var results []*Result
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan result", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate results", err)
	}
	return results, nil
}

// ListByVolunteer retrieves results for a volunteer with cursor-based pagination,
// ordered by submitted_at DESC.
func (repo *PgxRepository) ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*Result, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	args = append(args, volunteerID)

	query := "SELECT " + resultColumns + " FROM results WHERE volunteer_id = $1"

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		query += " AND (submitted_at, id) < ($2, $3)"
		args = append(args, cursorTime, cursorID)
	}

	query += fmt.Sprintf(" ORDER BY submitted_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, pageSize+1)

	rows, err := repo.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list results by volunteer", err)
	}
	defer rows.Close()

	var results []*Result
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan result", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate results", err)
	}

	pagination := types.PaginationResponse{}
	if len(results) > pageSize {
		results = results[:pageSize]
		last := results[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.SubmittedAt, last.ID)
	}

	return results, pagination, nil
}

// ListByLeaf retrieves results for a project (via work_units JOIN) with optional
// filtering and cursor-based pagination, ordered by submitted_at DESC.
func (repo *PgxRepository) ListByLeaf(ctx context.Context, projectID types.ID, filters ResultFilters, page types.PaginationRequest) ([]*Result, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	args = append(args, projectID)

	query := "SELECT " + prefixedResultColumns +
		" FROM results r JOIN work_units wu ON r.work_unit_id = wu.id WHERE wu.leaf_id = $1"

	argIdx := 2
	if filters.ValidationStatus != nil {
		query += fmt.Sprintf(" AND r.validation_status = $%d", argIdx)
		args = append(args, *filters.ValidationStatus)
		argIdx++
	}
	if filters.WorkUnitID != nil {
		query += fmt.Sprintf(" AND r.work_unit_id = $%d", argIdx)
		args = append(args, *filters.WorkUnitID)
		argIdx++
	}
	if filters.VolunteerID != nil {
		query += fmt.Sprintf(" AND r.volunteer_id = $%d", argIdx)
		args = append(args, *filters.VolunteerID)
		argIdx++
	}

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		query += fmt.Sprintf(" AND (r.submitted_at, r.id) < ($%d, $%d)", argIdx, argIdx+1)
		args = append(args, cursorTime, cursorID)
		argIdx += 2
	}

	query += fmt.Sprintf(" ORDER BY r.submitted_at DESC, r.id DESC LIMIT $%d", argIdx)
	args = append(args, pageSize+1)

	rows, err := repo.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list results by project", err)
	}
	defer rows.Close()

	var results []*Result
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan result", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate results", err)
	}

	pagination := types.PaginationResponse{}
	if len(results) > pageSize {
		results = results[:pageSize]
		last := results[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.SubmittedAt, last.ID)
	}

	return results, pagination, nil
}

// CountByWorkUnit returns the total number of results for a work unit.
func (repo *PgxRepository) CountByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error) {
	var count int
	err := repo.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM results WHERE work_unit_id = $1",
		workUnitID,
	).Scan(&count)
	if err != nil {
		return 0, apierror.Internal("failed to count results by work unit", err)
	}
	return count, nil
}

// CountPendingByWorkUnit returns the number of PENDING results for a work unit.
func (repo *PgxRepository) CountPendingByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error) {
	var count int
	err := repo.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'PENDING'",
		workUnitID,
	).Scan(&count)
	if err != nil {
		return 0, apierror.Internal("failed to count pending results by work unit", err)
	}
	return count, nil
}

// UpdateValidationStatus updates the validation_status and validated_at for a single result.
func (repo *PgxRepository) UpdateValidationStatus(ctx context.Context, id types.ID, status ValidationStatus) error {
	tag, err := repo.db.Exec(ctx,
		"UPDATE results SET validation_status = $2, validated_at = NOW(), updated_at = NOW() WHERE id = $1",
		id, status,
	)
	if err != nil {
		return apierror.Internal("failed to update result validation status", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("result", id.String())
	}
	return nil
}

// BatchUpdateValidationStatus updates validation_status and validated_at for multiple results.
func (repo *PgxRepository) BatchUpdateValidationStatus(ctx context.Context, ids []types.ID, status ValidationStatus) error {
	if len(ids) == 0 {
		return nil
	}
	tag, err := repo.db.Exec(ctx,
		"UPDATE results SET validation_status = $2, validated_at = NOW(), updated_at = NOW() WHERE id = ANY($1)",
		ids, status,
	)
	if err != nil {
		return apierror.Internal("failed to batch update result validation status", err)
	}
	if int(tag.RowsAffected()) != len(ids) {
		return apierror.Internal(
			fmt.Sprintf("batch update: expected %d rows affected, got %d", len(ids), tag.RowsAffected()),
			nil,
		)
	}
	return nil
}
