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
	"github.com/lettuce-compute/infrastructure/internal/admission"
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

// volunteerColumns is the standard column list for SELECT queries (and the RETURNING
// clause on Create). The optional DID identity-binding columns are appended last;
// Create/Update do NOT write them (they change only through the dedicated DID
// methods below), but every read path scans them, so they belong here.
const volunteerColumns = `id, numeric_id, public_key, user_id, display_name,
	hardware_capabilities, available_runtimes, scheduling_mode, schedule_config,
	is_active, last_seen_at,
	total_work_units_completed, total_work_units_rejected,
	registered_at, created_at, updated_at,
	did, did_binding_uri, did_binding_cid, did_binding_status,
	did_bound_at, did_binding_checked_at, did_binding_check_failures, did_frozen_until,
	standing, benched_until, standing_source, standing_reason, standing_changed_at`

// scanVolunteer scans a volunteer row into a Volunteer struct. The scan order must
// match volunteerColumns exactly.
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
		&v.DID,
		&v.DIDBindingURI,
		&v.DIDBindingCID,
		&v.DIDBindingStatus,
		&v.DIDBoundAt,
		&v.DIDBindingCheckedAt,
		&v.DIDBindingCheckFailures,
		&v.DIDFrozenUntil,
		&v.Standing,
		&v.BenchedUntil,
		&v.StandingSource,
		&v.StandingReason,
		&v.StandingChangedAt,
	)
	return &v, err
}

// Create inserts a new volunteer.
// On return, v is populated with the DB-generated id and timestamps.
func (r *PgxRepository) Create(ctx context.Context, v *Volunteer) error {
	return createIn(ctx, r.pool, v)
}

// CreateAdmitted creates a volunteer through the registration-admission gates
// (internal/admission). A nil gate is exactly Create — the legacy single-statement
// path, byte-for-byte. With a gate, the proof-of-work redemption (gate.Pow, when set),
// the per-(bucket, UTC day) creation-cap increment (gate.CapPerDay > 0), and the
// volunteer INSERT run in ONE transaction: a refusal (a bad or missing solution, the
// cap, a duplicate-key conflict) or any failure rolls everything back together — so the
// counter counts exactly the creations that committed, and a consumed challenge
// un-consumes when an unrelated refusal follows it.
func (r *PgxRepository) CreateAdmitted(ctx context.Context, v *Volunteer, gate *admission.CreateGate) error {
	if gate == nil {
		return r.Create(ctx, v)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return apierror.Internal("failed to begin registration transaction", err)
	}
	// Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	// Proof-of-work first (design order): an invalid solution must be refused before a
	// cap slot is reserved, and its sentinel must surface un-wrapped for the handlers.
	if gate.Pow != nil {
		if err := admission.RedeemChallenge(ctx, tx, gate.Pow); err != nil {
			if errors.Is(err, admission.ErrPowChallengeInvalid) || errors.Is(err, admission.ErrPowSolutionInvalid) {
				return err
			}
			return apierror.Internal("failed to redeem registration proof-of-work challenge", err)
		}
	}
	if gate.CapPerDay > 0 {
		if err := admission.ReserveCreationSlot(ctx, tx, gate.Bucket, gate.CapPerDay); err != nil {
			if errors.Is(err, admission.ErrCreationCapExceeded) {
				return err
			}
			return apierror.Internal("failed to enforce registration creation cap", err)
		}
	}
	if err := createIn(ctx, tx, v); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return apierror.Internal("failed to commit registration transaction", err)
	}
	return nil
}

// createIn runs the volunteer INSERT on the given pgx surface (the shared pool for the
// legacy path, the admission transaction for CreateAdmitted).
func createIn(ctx context.Context, db admission.DBTX, v *Volunteer) error {
	row := db.QueryRow(ctx, `
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

// --- Optional ATProto DID identity binding ---
//
// These methods are the ONLY writers of the did_* columns; Create and Update
// deliberately leave them alone so binding state changes only through this
// verification/recheck surface.

// SetDIDBinding records a freshly verified DID identity binding on a volunteer
// row: it stamps the decentralized identifier, the source key-authorization
// record's AT-URI and content hash (CID), marks the binding OK, sets both
// did_bound_at and did_binding_checked_at to boundAt, and clears the
// consecutive-failure counter. Called after the head has fetched and
// cryptographically verified the record from the volunteer's PDS repo.
func (r *PgxRepository) SetDIDBinding(ctx context.Context, volunteerID types.ID, did, recordURI, recordCID string, boundAt time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volunteers SET
			did = $2,
			did_binding_uri = $3,
			did_binding_cid = $4,
			did_binding_status = $5,
			did_bound_at = $6,
			did_binding_checked_at = $6,
			did_binding_check_failures = 0
		WHERE id = $1`,
		volunteerID, did, recordURI, recordCID, DIDBindingStatusOK, boundAt,
	)
	if err != nil {
		return apierror.Internal("failed to set DID binding", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", volunteerID.String())
	}
	return nil
}

// ListDIDBindingsForRecheck returns volunteers whose DID binding is due for a TTL
// re-verification: a bound row whose status is still active (OK or STALE — a
// REVOKED binding is terminal and never re-checked) and whose last check predates
// checkedBefore, oldest-checked first so the worker drains the most overdue
// bindings first. limit bounds the batch size.
func (r *PgxRepository) ListDIDBindingsForRecheck(ctx context.Context, checkedBefore time.Time, limit int) ([]*Volunteer, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+volunteerColumns+` FROM volunteers
			WHERE did IS NOT NULL
			  AND did_binding_status IN ('OK', 'STALE')
			  AND did_binding_checked_at < $1
			ORDER BY did_binding_checked_at ASC
			LIMIT $2`,
		checkedBefore, limit,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list DID bindings for recheck", err)
	}
	defer rows.Close()

	var volunteers []*Volunteer
	for rows.Next() {
		v, scanErr := scanVolunteer(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan volunteer", scanErr)
		}
		volunteers = append(volunteers, v)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate DID bindings for recheck", err)
	}
	return volunteers, nil
}

// MarkDIDBindingChecked records a successful TTL re-verification: it refreshes the
// stored record CID (the authorization record may have been legitimately updated
// in place), marks the binding OK again — clearing any prior STALE state —
// advances did_binding_checked_at, and resets the consecutive-failure counter.
func (r *PgxRepository) MarkDIDBindingChecked(ctx context.Context, volunteerID types.ID, recordCID string, checkedAt time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volunteers SET
			did_binding_status = $2,
			did_binding_cid = $3,
			did_binding_checked_at = $4,
			did_binding_check_failures = 0
		WHERE id = $1`,
		volunteerID, DIDBindingStatusOK, recordCID, checkedAt,
	)
	if err != nil {
		return apierror.Internal("failed to mark DID binding checked", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", volunteerID.String())
	}
	return nil
}

// MarkDIDBindingCheckFailed records one failed TTL re-verification attempt (a
// transient resolve/fetch error, NOT a definitive revocation): in a single
// statement it increments the consecutive-failure counter, advances
// did_binding_checked_at, and escalates the binding from OK to STALE once the new
// failure count reaches staleAfter. An already-REVOKED binding is left untouched
// (terminal); STALE stays STALE. Only an explicit RevokeDIDBinding hard-revokes.
func (r *PgxRepository) MarkDIDBindingCheckFailed(ctx context.Context, volunteerID types.ID, checkedAt time.Time, staleAfter int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volunteers SET
			did_binding_check_failures = did_binding_check_failures + 1,
			did_binding_checked_at = $2,
			did_binding_status = CASE
				WHEN did_binding_status = $3 THEN $3
				WHEN did_binding_check_failures + 1 >= $4 THEN $5
				ELSE did_binding_status
			END
		WHERE id = $1`,
		volunteerID, checkedAt, DIDBindingStatusRevoked, staleAfter, DIDBindingStatusStale,
	)
	if err != nil {
		return apierror.Internal("failed to mark DID binding check failed", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", volunteerID.String())
	}
	return nil
}

// RevokeDIDBinding hard-revokes a volunteer's DID binding: re-verification found
// the authorization record gone or repudiated. The status becomes REVOKED
// (terminal — ListDIDBindingsForRecheck no longer returns the row) and
// did_binding_checked_at is advanced. The did/uri/cid columns are retained for audit.
func (r *PgxRepository) RevokeDIDBinding(ctx context.Context, volunteerID types.ID, revokedAt time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volunteers SET
			did_binding_status = $2,
			did_binding_checked_at = $3
		WHERE id = $1`,
		volunteerID, DIDBindingStatusRevoked, revokedAt,
	)
	if err != nil {
		return apierror.Internal("failed to revoke DID binding", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", volunteerID.String())
	}
	return nil
}

// SetDIDFrozenUntil sets the timestamp until which a volunteer's DID binding is
// frozen after a signing-key rotation, so a rotated identity cannot immediately
// re-bind (an anti-abuse cool-down; see HeadConfig.DIDRotationFreezeHours).
func (r *PgxRepository) SetDIDFrozenUntil(ctx context.Context, volunteerID types.ID, until time.Time) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE volunteers SET did_frozen_until = $2 WHERE id = $1",
		volunteerID, until,
	)
	if err != nil {
		return apierror.Internal("failed to set DID frozen-until", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("volunteer", volunteerID.String())
	}
	return nil
}
