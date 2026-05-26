package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Challenge represents an identity verification challenge.
type Challenge struct {
	ID        types.ID
	PublicKey []byte
	Challenge []byte
	ExpiresAt time.Time
	Verified  bool
	CreatedAt time.Time
}

// ChallengeExpiry is how long a challenge remains valid.
const ChallengeExpiry = 10 * time.Minute

// ChallengeStore manages identity challenge lifecycle.
type ChallengeStore interface {
	Create(ctx context.Context, publicKey []byte) (*Challenge, error)
	Get(ctx context.Context, challengeID types.ID) (*Challenge, error)
	Verify(ctx context.Context, challengeID types.ID) error
}

// PgxChallengeStore implements ChallengeStore using PostgreSQL.
type PgxChallengeStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPgxChallengeStore creates a new PgxChallengeStore.
func NewPgxChallengeStore(pool *pgxpool.Pool, logger *slog.Logger) *PgxChallengeStore {
	return &PgxChallengeStore{pool: pool, logger: logger}
}

// Create generates a new 32-byte random challenge with 10-minute expiry.
func (s *PgxChallengeStore) Create(ctx context.Context, publicKey []byte) (*Challenge, error) {
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return nil, err
	}

	now := types.Now()
	c := &Challenge{
		ID:        types.NewID(),
		PublicKey: publicKey,
		Challenge: challengeBytes,
		ExpiresAt: now.Add(ChallengeExpiry),
		Verified:  false,
		CreatedAt: now,
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO identity_challenges (id, public_key, challenge, expires_at, verified, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, c.PublicKey, c.Challenge, c.ExpiresAt, c.Verified, c.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Get retrieves a challenge by ID. Returns nil if not found.
func (s *PgxChallengeStore) Get(ctx context.Context, challengeID types.ID) (*Challenge, error) {
	c := &Challenge{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, public_key, challenge, expires_at, verified, created_at
		 FROM identity_challenges WHERE id = $1`,
		challengeID,
	).Scan(&c.ID, &c.PublicKey, &c.Challenge, &c.ExpiresAt, &c.Verified, &c.CreatedAt)

	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Verify marks a challenge as verified.
func (s *PgxChallengeStore) Verify(ctx context.Context, challengeID types.ID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE identity_challenges SET verified = true WHERE id = $1`,
		challengeID,
	)
	return err
}

// ChallengeHex returns the challenge bytes as a hex string.
func (c *Challenge) ChallengeHex() string {
	return hex.EncodeToString(c.Challenge)
}

// StartCleanup runs a background goroutine that deletes expired challenges every 5 minutes.
func (s *PgxChallengeStore) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := s.deleteExpired(ctx)
			if err != nil {
				s.logger.Error("failed to clean up expired challenges", "error", err)
			} else if deleted > 0 {
				s.logger.Info("cleaned up expired identity challenges", "count", deleted)
			}
		}
	}
}

func (s *PgxChallengeStore) deleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM identity_challenges WHERE expires_at < NOW()`,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
