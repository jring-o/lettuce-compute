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
	LeafID             *types.ID
	VolunteerPublicKey []byte
	From               *string
	To                 *string
}

// Creator is the write-side surface the validation engine (and the revocation emitter)
// needs: append one signed attestation. Narrow on purpose — the producer never reads.
type Creator interface {
	Create(ctx context.Context, att *Attestation) error
}

// Reader is the read-side surface the HTTP handler needs: the public list, verify-by-id,
// and the revocation chain of a grant.
type Reader interface {
	List(ctx context.Context, filters ListFilters, page types.PaginationRequest) ([]*Attestation, types.PaginationResponse, error)
	GetByID(ctx context.Context, id types.ID) (*Attestation, error)
	// ListRevocationsOf returns every revocation attestation referencing the given original,
	// oldest first (partial clawbacks produce one revocation per adjustment).
	ListRevocationsOf(ctx context.Context, attestationID types.ID) ([]*Attestation, error)
}

// Repository is the full data-access interface for credit attestations (the concrete pgx
// repository implements it; wiring code passes it wherever a narrower view suffices).
type Repository interface {
	Creator
	Reader
	// GetGrantByResultID returns THE AGREED v2 grant attestation for a result — unique by
	// the uq_attestations_result_agreed partial index (at most one AGREED grant per result).
	GetGrantByResultID(ctx context.Context, resultID types.ID) (*Attestation, error)
}

// attestationColumns selects every column plus credit_amount rendered as text: the fixed
// numeric(18,6) scale renders exactly six fractional digits, which IS the v2 canonical
// credit representation — verification uses the stored text, never a re-rounded float.
const attestationColumns = `id, schema_version, leaf_id, volunteer_public_key, work_unit_id,
	result_id, output_checksum, quorum_descriptor, policy_version,
	revokes_attestation_id, adjustment_id, reason,
	raw_metrics, validation_outcome, credit_amount, credit_amount::text,
	attestation_timestamp, signature, created_at`

func scanAttestation(row pgx.Row) (*Attestation, error) {
	var a Attestation
	var rawMetrics, descriptor []byte
	err := row.Scan(
		&a.ID,
		&a.SchemaVersion,
		&a.LeafID,
		&a.VolunteerPublicKey,
		&a.WorkUnitID,
		&a.ResultID,
		&a.OutputChecksum,
		&descriptor,
		&a.PolicyVersion,
		&a.RevokesAttestationID,
		&a.AdjustmentID,
		&a.Reason,
		&rawMetrics,
		&a.ValidationOutcome,
		&a.CreditAmount,
		&a.CreditAmountCanonical,
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
	if descriptor != nil {
		var d QuorumDescriptor
		if err := json.Unmarshal(descriptor, &d); err != nil {
			return nil, fmt.Errorf("unmarshal quorum_descriptor: %w", err)
		}
		a.QuorumDescriptor = &d
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

// Create inserts a new credit attestation. The credit_amount parameter is the CANONICAL
// STRING representation (six fractional digits) — Postgres parses it exactly at the
// column's scale, so the stored value equals the signed bytes by construction; passing the
// raw float64 would let the driver and the database re-round it under a different rule.
func (r *PgxRepository) Create(ctx context.Context, att *Attestation) error {
	metricsJSON, err := json.Marshal(att.RawMetrics)
	if err != nil {
		return apierror.Internal("failed to marshal raw_metrics", err)
	}

	var descriptorJSON any
	if att.QuorumDescriptor != nil {
		b, err := json.Marshal(att.QuorumDescriptor)
		if err != nil {
			return apierror.Internal("failed to marshal quorum_descriptor", err)
		}
		descriptorJSON = b
	}

	if att.SchemaVersion == 0 {
		att.SchemaVersion = SchemaVersionV2
	}
	if att.CreditAmountCanonical == "" {
		att.CreditAmountCanonical = CanonicalCreditString(att.CreditAmount)
	}

	row := r.db.QueryRow(ctx, `
		INSERT INTO credit_attestations (
			schema_version, leaf_id, volunteer_public_key, work_unit_id,
			result_id, output_checksum, quorum_descriptor, policy_version,
			revokes_attestation_id, adjustment_id, reason,
			raw_metrics, validation_outcome, credit_amount,
			attestation_timestamp, signature
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING `+attestationColumns,
		att.SchemaVersion, att.LeafID, att.VolunteerPublicKey, att.WorkUnitID,
		att.ResultID, att.OutputChecksum, descriptorJSON, att.PolicyVersion,
		att.RevokesAttestationID, att.AdjustmentID, att.Reason,
		metricsJSON, att.ValidationOutcome, att.CreditAmountCanonical,
		att.AttestationTimestamp, att.Signature,
	)

	created, scanErr := scanAttestation(row)
	if scanErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(scanErr, &pgErr) {
			switch pgErr.Code {
			case "23503":
				return apierror.Conflict(
					"referenced entity does not exist",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			case "23505":
				return apierror.Conflict(
					"attestation already exists",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
		}
		return apierror.Internal("failed to create credit attestation", scanErr)
	}
	*att = *created
	return nil
}

// GetByID retrieves a single attestation by primary key.
func (r *PgxRepository) GetByID(ctx context.Context, id types.ID) (*Attestation, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+attestationColumns+" FROM credit_attestations WHERE id = $1", id)
	a, err := scanAttestation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("attestation", id.String())
		}
		return nil, apierror.Internal("failed to get attestation", err)
	}
	return a, nil
}

// GetGrantByResultID retrieves THE AGREED v2 grant attestation for a result (unique by the
// uq_attestations_result_agreed partial index).
func (r *PgxRepository) GetGrantByResultID(ctx context.Context, resultID types.ID) (*Attestation, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+attestationColumns+` FROM credit_attestations
		 WHERE result_id = $1 AND validation_outcome = 'AGREED' AND schema_version = 2`,
		resultID)
	a, err := scanAttestation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("grant_attestation_for_result", resultID.String())
		}
		return nil, apierror.Internal("failed to get grant attestation", err)
	}
	return a, nil
}

// ListRevocationsOf returns every revocation referencing the given attestation, oldest
// first.
func (r *PgxRepository) ListRevocationsOf(ctx context.Context, attestationID types.ID) ([]*Attestation, error) {
	rows, err := r.db.Query(ctx,
		"SELECT "+attestationColumns+` FROM credit_attestations
		 WHERE revokes_attestation_id = $1
		 ORDER BY attestation_timestamp ASC, id ASC`,
		attestationID)
	if err != nil {
		return nil, apierror.Internal("failed to list revocations", err)
	}
	defer rows.Close()

	var revocations []*Attestation
	for rows.Next() {
		a, scanErr := scanAttestation(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan revocation", scanErr)
		}
		revocations = append(revocations, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate revocations", err)
	}
	return revocations, nil
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
