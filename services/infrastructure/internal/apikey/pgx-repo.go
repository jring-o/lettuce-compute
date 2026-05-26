package apikey

import (
	"context"
	"errors"

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

// apiKeyColumns is the standard column list for SELECT queries.
const apiKeyColumns = `id, user_id, name, key_prefix, key_hash, created_at, last_used_at, revoked_at`

// scanApiKey scans an api_key row into an ApiKey struct.
func scanApiKey(row pgx.Row) (*ApiKey, error) {
	var k ApiKey
	err := row.Scan(
		&k.ID,
		&k.UserID,
		&k.Name,
		&k.KeyPrefix,
		&k.KeyHash,
		&k.CreatedAt,
		&k.LastUsedAt,
		&k.RevokedAt,
	)
	return &k, err
}

// Create inserts a new API key record.
func (r *PgxRepository) Create(ctx context.Context, key *ApiKey) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, key_prefix, key_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING `+apiKeyColumns,
		key.UserID, key.Name, key.KeyPrefix, key.KeyHash,
	)

	result, err := scanApiKey(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return apierror.Conflict(
				"API key with this hash already exists",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to create API key", err)
	}
	*key = *result
	return nil
}

// GetByHash looks up an active (non-revoked) API key by its SHA-256 hash.
// Returns nil, nil if no matching active key is found.
func (r *PgxRepository) GetByHash(ctx context.Context, keyHash []byte) (*ApiKey, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+apiKeyColumns+" FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL", keyHash)

	k, err := scanApiKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to get API key by hash", err)
	}
	return k, nil
}

// ListByUser returns all API keys for a user (including revoked ones),
// ordered by created_at DESC.
func (r *PgxRepository) ListByUser(ctx context.Context, userID types.ID) ([]*ApiKey, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+apiKeyColumns+" FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, apierror.Internal("failed to list API keys", err)
	}
	defer rows.Close()

	var keys []*ApiKey
	for rows.Next() {
		k, scanErr := scanApiKey(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan API key", scanErr)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate API keys", err)
	}
	return keys, nil
}

// Revoke sets revoked_at to NOW() for the given key ID.
// Returns an error if the key doesn't exist or is already revoked.
func (r *PgxRepository) Revoke(ctx context.Context, id types.ID) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL", id)
	if err != nil {
		return apierror.Internal("failed to revoke API key", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("api_key", id.String())
	}
	return nil
}

// UpdateLastUsed sets last_used_at to NOW() for the given key ID.
func (r *PgxRepository) UpdateLastUsed(ctx context.Context, id types.ID) error {
	_, err := r.pool.Exec(ctx,
		"UPDATE api_keys SET last_used_at = NOW() WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to update last used", err)
	}
	return nil
}
