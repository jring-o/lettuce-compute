package volunteer

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PgxHostRepository implements HostRepository using pgx.
type PgxHostRepository struct {
	pool *pgxpool.Pool
}

// NewPgxHostRepository creates a new PgxHostRepository.
func NewPgxHostRepository(pool *pgxpool.Pool) *PgxHostRepository {
	return &PgxHostRepository{pool: pool}
}

const hostColumns = `id, volunteer_id, display_name,
	hardware_capabilities, available_runtimes, is_active, last_seen_at,
	created_at, updated_at`

func scanHost(row pgx.Row) (*Host, error) {
	var h Host
	err := row.Scan(
		&h.ID,
		&h.VolunteerID,
		&h.DisplayName,
		&h.HardwareCapabilities,
		&h.AvailableRuntimes,
		&h.IsActive,
		&h.LastSeenAt,
		&h.CreatedAt,
		&h.UpdatedAt,
	)
	return &h, err
}

// Mint inserts a NEW host row under the per-account host cap (BG-25). See the interface
// doc for the semantics. Serialization: the SELECT ... FOR UPDATE on the account's
// volunteers row pins ALL of this account's mints to one-at-a-time, so the count, the
// eviction, and the insert are atomic per account. Do NOT imitate CreateAdmitted's
// mechanism here — that path serializes on the admission COUNTER row's upsert lock,
// which has no analog for a count-over-rows cap (audit F-D).
func (r *PgxHostRepository) Mint(ctx context.Context, h *Host, capPerAccount int, activeWindow time.Duration) (bool, error) {
	if capPerAccount <= 0 {
		// Cap disabled: a plain insert, no per-account serialization needed.
		row := r.pool.QueryRow(ctx, `
			INSERT INTO hosts (
				id, volunteer_id, display_name,
				hardware_capabilities, available_runtimes, is_active, last_seen_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+hostColumns,
			h.ID, h.VolunteerID, h.DisplayName,
			h.HardwareCapabilities, h.AvailableRuntimes, h.IsActive, h.LastSeenAt,
		)
		result, err := scanHost(row)
		if err != nil {
			return false, apierror.Internal("failed to mint host", err)
		}
		*h = *result
		return true, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, apierror.Internal("failed to begin host mint transaction", err)
	}
	// Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize this account's mints. The volunteers row must exist (registration
	// created or updated it moments ago in the same request).
	var lockedID types.ID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM volunteers WHERE id = $1 FOR UPDATE`, h.VolunteerID,
	).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, apierror.NotFound("volunteer", h.VolunteerID.String())
		}
		return false, apierror.Internal("failed to lock account for host mint", err)
	}

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM hosts WHERE volunteer_id = $1`, h.VolunteerID,
	).Scan(&count); err != nil {
		return false, apierror.Internal("failed to count account hosts", err)
	}

	if count >= capPerAccount {
		// At cap: evict the STALEST slot iff it is genuinely stale (unseen past the
		// activity window); otherwise refuse. The cutoff uses the head's clock, the
		// same source that stamps last_seen_at. NULLS FIRST is defensive — rows are
		// always stamped at registration, but an unstamped row is by definition the
		// stalest.
		cutoff := time.Now().UTC().Add(-activeWindow)
		var evictedID types.ID
		err := tx.QueryRow(ctx, `
			DELETE FROM hosts WHERE id = (
				SELECT id FROM hosts
				WHERE volunteer_id = $1
				  AND (last_seen_at IS NULL OR last_seen_at < $2)
				ORDER BY last_seen_at ASC NULLS FIRST
				LIMIT 1
			)
			RETURNING id`, h.VolunteerID, cutoff,
		).Scan(&evictedID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Every slot is recently active: the refusal. Not an error — the caller
			// returns an empty host id and the machine works in the account bucket.
			return false, nil
		}
		if err != nil {
			return false, apierror.Internal("failed to evict stale host", err)
		}
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO hosts (
			id, volunteer_id, display_name,
			hardware_capabilities, available_runtimes, is_active, last_seen_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+hostColumns,
		h.ID, h.VolunteerID, h.DisplayName,
		h.HardwareCapabilities, h.AvailableRuntimes, h.IsActive, h.LastSeenAt,
	)
	result, err := scanHost(row)
	if err != nil {
		return false, apierror.Internal("failed to mint host", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, apierror.Internal("failed to commit host mint", err)
	}
	*h = *result
	return true, nil
}

// Upsert inserts the host or refreshes its per-machine facts on id conflict. This is
// the ECHO-REFRESH path: a machine re-registering with its issued id lands on the SAME
// row (fixing the flapping-row bug where N machines under one key overwrote each other
// on the single volunteers row). New rows are created here only when the caller has
// already validated ownership; cap-enforced creation goes through Mint.
func (r *PgxHostRepository) Upsert(ctx context.Context, h *Host) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO hosts (
			id, volunteer_id, display_name,
			hardware_capabilities, available_runtimes, is_active, last_seen_at
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7
		)
		ON CONFLICT (id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			hardware_capabilities = EXCLUDED.hardware_capabilities,
			available_runtimes = EXCLUDED.available_runtimes,
			is_active = EXCLUDED.is_active,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = now()
		RETURNING `+hostColumns,
		h.ID, h.VolunteerID, h.DisplayName,
		h.HardwareCapabilities, h.AvailableRuntimes, h.IsActive, h.LastSeenAt,
	)
	result, err := scanHost(row)
	if err != nil {
		return apierror.Internal("failed to upsert host", err)
	}
	*h = *result
	return nil
}

// GetByID retrieves a host by its issued id.
func (r *PgxHostRepository) GetByID(ctx context.Context, id types.ID) (*Host, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE id = $1", id)
	h, err := scanHost(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("host", id.String())
		}
		return nil, apierror.Internal("failed to get host", err)
	}
	return h, nil
}

// UpdateLastSeen bumps last_seen_at/is_active for a host without rewriting capabilities.
func (r *PgxHostRepository) UpdateLastSeen(ctx context.Context, id types.ID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE hosts SET last_seen_at = now(), is_active = true, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return apierror.Internal("failed to update host last_seen", err)
	}
	return nil
}
