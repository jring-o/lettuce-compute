package volunteer

import (
	"context"
	"errors"

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

const hostColumns = `id, volunteer_id, host_key, display_name,
	hardware_capabilities, available_runtimes, is_active, last_seen_at,
	created_at, updated_at`

func scanHost(row pgx.Row) (*Host, error) {
	var h Host
	err := row.Scan(
		&h.ID,
		&h.VolunteerID,
		&h.HostKey,
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

// Upsert inserts the host or refreshes its per-machine facts on id conflict. The id is
// the head's deterministic effective host id, so a machine re-registering (new hardware,
// changed runtimes) lands on the SAME row — fixing the flapping-row bug where N machines
// under one key overwrote each other on the single volunteers row.
func (r *PgxHostRepository) Upsert(ctx context.Context, h *Host) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO hosts (
			id, volunteer_id, host_key, display_name,
			hardware_capabilities, available_runtimes, is_active, last_seen_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8
		)
		ON CONFLICT (id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			hardware_capabilities = EXCLUDED.hardware_capabilities,
			available_runtimes = EXCLUDED.available_runtimes,
			is_active = EXCLUDED.is_active,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = now()
		RETURNING `+hostColumns,
		h.ID, h.VolunteerID, h.HostKey, h.DisplayName,
		h.HardwareCapabilities, h.AvailableRuntimes, h.IsActive, h.LastSeenAt,
	)
	result, err := scanHost(row)
	if err != nil {
		return apierror.Internal("failed to upsert host", err)
	}
	*h = *result
	return nil
}

// GetByID retrieves a host by its (effective) id.
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
