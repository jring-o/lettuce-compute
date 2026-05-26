package apikey

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Repository defines the data-access interface for API keys.
type Repository interface {
	// Create inserts a new API key record. The caller is responsible for
	// generating the key and computing the hash via GenerateKey().
	Create(ctx context.Context, key *ApiKey) error

	// GetByHash looks up an active (non-revoked) API key by its SHA-256 hash.
	// Returns nil, nil if no matching active key is found.
	GetByHash(ctx context.Context, keyHash []byte) (*ApiKey, error)

	// ListByUser returns all API keys for a user (including revoked ones),
	// ordered by created_at DESC.
	ListByUser(ctx context.Context, userID types.ID) ([]*ApiKey, error)

	// Revoke sets revoked_at to NOW() for the given key ID.
	// Returns an error if the key doesn't exist or is already revoked.
	Revoke(ctx context.Context, id types.ID) error

	// UpdateLastUsed sets last_used_at to NOW() for the given key ID.
	// Called on each successful authentication.
	UpdateLastUsed(ctx context.Context, id types.ID) error
}
