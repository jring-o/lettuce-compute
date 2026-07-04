package admission

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// counterRetentionDays is how long spent (bucket, day) creation-counter rows are kept
// after their day has passed, for operator forensics (which networks were creating
// accounts). Past rows are never read by the gate itself — the cap keys strictly on the
// current UTC day — so retention is purely observational.
const counterRetentionDays = 7

// counterSweepInterval is how often the sweeper prunes aged counter rows. The table only
// grows by one row per (bucket, day) that actually created a volunteer, so a relaxed
// cadence is plenty.
const counterSweepInterval = 6 * time.Hour

// CounterSweeper prunes aged registration_creation_counts rows. It is a singleton
// background job: main.go starts it inside the leader-jobs closure (the
// challengeStore.StartCleanup pattern), and only when the creation cap is enabled.
type CounterSweeper struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewCounterSweeper builds a sweeper over the given pool.
func NewCounterSweeper(pool *pgxpool.Pool, logger *slog.Logger) *CounterSweeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &CounterSweeper{pool: pool, logger: logger}
}

// Start runs the sweep loop until ctx is cancelled: once immediately on start (the DID
// re-check worker pattern — a head that was down past the retention window catches up on
// election), then every counterSweepInterval.
func (s *CounterSweeper) Start(ctx context.Context) {
	s.sweep(ctx)

	ticker := time.NewTicker(counterSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *CounterSweeper) sweep(ctx context.Context) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM registration_creation_counts
		 WHERE day < (NOW() AT TIME ZONE 'utc')::date - $1::int`,
		counterRetentionDays,
	)
	if err != nil {
		s.logger.Error("failed to sweep aged registration creation counters", "error", err)
		return
	}
	if deleted := tag.RowsAffected(); deleted > 0 {
		s.logger.Info("swept aged registration creation counters", "count", deleted)
	}
}

// challengeSweepInterval matches the identity-challenge cleanup cadence: expired
// proof-of-work challenges are dead weight after their (default 10-minute) TTL.
const challengeSweepInterval = 5 * time.Minute

// ChallengeSweeper deletes expired registration_challenges rows. Unlike the counter
// sweeper it is wired UNCONDITIONALLY into the leader jobs: challenge ISSUANCE works
// whether or not enforcement is on (so clients can be written probe-free), so the table
// can gain rows even while the enforcement knob is off.
type ChallengeSweeper struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewChallengeSweeper builds a sweeper over the given pool.
func NewChallengeSweeper(pool *pgxpool.Pool, logger *slog.Logger) *ChallengeSweeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChallengeSweeper{pool: pool, logger: logger}
}

// Start runs the sweep loop until ctx is cancelled: once immediately, then every
// challengeSweepInterval.
func (s *ChallengeSweeper) Start(ctx context.Context) {
	s.sweep(ctx)

	ticker := time.NewTicker(challengeSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *ChallengeSweeper) sweep(ctx context.Context) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM registration_challenges WHERE expires_at < NOW()`)
	if err != nil {
		s.logger.Error("failed to sweep expired registration challenges", "error", err)
		return
	}
	if deleted := tag.RowsAffected(); deleted > 0 {
		s.logger.Info("swept expired registration challenges", "count", deleted)
	}
}
