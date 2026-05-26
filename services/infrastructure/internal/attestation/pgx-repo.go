package attestation

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

// ListFilters controls optional filtering for attestation list queries.
type ListFilters struct {
	LeafID          *types.ID
	VolunteerPublicKey []byte
	From               *string
	To                 *string
}

// Repository defines the data-access interface for credit attestations.
type Repository interface {
	Create(ctx context.Context, att *Attestation) error
	List(ctx context.Context, filters ListFilters, page types.PaginationRequest) ([]*Attestation, types.PaginationResponse, error)
}

const attestationColumns = `id, leaf_id, volunteer_public_key, work_unit_id,
	raw_metrics, validation_outcome, credit_amount, attestation_timestamp,
	signature, created_at`

func scanAttestation(row pgx.Row) (*Attestation, error) {
	var a Attestation
	var rawMetrics []byte
	err := row.Scan(
		&a.ID,
		&a.LeafID,
		&a.VolunteerPublicKey,
		&a.WorkUnitID,
		&rawMetrics,
		&a.ValidationOutcome,
		&a.CreditAmount,
		&a.AttestationTimestamp,
		&a.Signature,
		&a.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if rawMetrics != nil {
		if err := json.Unmarshal(rawMetrics, &a.RawMetrics); err != nil {
			return nil, fmt.Errorf("unmarshal raw_metrics: %w", err)
		}
	}
	return &a, nil
}

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// Create inserts a new credit attestation.
func (r *PgxRepository) Create(ctx context.Context, att *Attestation) error {
	metricsJSON, err := json.Marshal(att.RawMetrics)
	if err != nil {
		return apierror.Internal("failed to marshal raw_metrics", err)
	}

	row := r.db.QueryRow(ctx, `
		INSERT INTO credit_attestations (
			leaf_id, volunteer_public_key, work_unit_id,
			raw_metrics, validation_outcome, credit_amount,
			attestation_timestamp, signature
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+attestationColumns,
		att.LeafID, att.VolunteerPublicKey, att.WorkUnitID,
		metricsJSON, att.ValidationOutcome, att.CreditAmount,
		att.AttestationTimestamp, att.Signature,
	)

	created, scanErr := scanAttestation(row)
	if scanErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(scanErr, &pgErr) && pgErr.Code == "23503" {
			return apierror.Conflict(
				"referenced entity does not exist",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to create credit attestation", scanErr)
	}
	*att = *created
	return nil
}

// List retrieves attestations with optional filters and cursor-based pagination,
// ordered by attestation_timestamp DESC.
func (r *PgxRepository) List(ctx context.Context, filters ListFilters, page types.PaginationRequest) ([]*Attestation, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var args []any
	query := "SELECT " + attestationColumns + " FROM credit_attestations WHERE 1=1"

	if filters.LeafID != nil {
		args = append(args, *filters.LeafID)
		query += fmt.Sprintf(" AND leaf_id = $%d", len(args))
	}

	if len(filters.VolunteerPublicKey) > 0 {
		args = append(args, filters.VolunteerPublicKey)
		query += fmt.Sprintf(" AND volunteer_public_key = $%d", len(args))
	}

	if filters.From != nil {
		fromTime, err := types.ParseTimestamp(*filters.From)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid 'from' timestamp", nil)
		}
		args = append(args, fromTime)
		query += fmt.Sprintf(" AND attestation_timestamp >= $%d", len(args))
	}

	if filters.To != nil {
		toTime, err := types.ParseTimestamp(*filters.To)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid 'to' timestamp", nil)
		}
		args = append(args, toTime)
		query += fmt.Sprintf(" AND attestation_timestamp <= $%d", len(args))
	}

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		args = append(args, cursorTime, cursorID)
		query += fmt.Sprintf(" AND (attestation_timestamp, id) < ($%d, $%d)", len(args)-1, len(args))
	}

	query += fmt.Sprintf(" ORDER BY attestation_timestamp DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, pageSize+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list attestations", err)
	}
	defer rows.Close()

	var attestations []*Attestation
	for rows.Next() {
		a, scanErr := scanAttestation(rows)
		if scanErr != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan attestation", scanErr)
		}
		attestations = append(attestations, a)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate attestations", err)
	}

	pagination := types.PaginationResponse{}
	if len(attestations) > pageSize {
		attestations = attestations[:pageSize]
		last := attestations[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.AttestationTimestamp, last.ID)
	}

	return attestations, pagination, nil
}
