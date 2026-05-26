package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/config"
)

const maxBackoff = 30 * time.Second

// ConnectWithRetry attempts to create a connection pool with exponential backoff.
// maxRetries: maximum number of connection attempts (0 = try once).
// baseDelay: initial delay between retries (doubled each attempt).
// Returns the pool on success, or the last error on failure.
func ConnectWithRetry(ctx context.Context, cfg config.DatabaseConfig, maxRetries int, baseDelay time.Duration) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseDelay * (1 << (attempt - 1))
			if delay > maxBackoff {
				delay = maxBackoff
			}
			slog.Warn("retrying database connection",
				"attempt", attempt,
				"max_retries", maxRetries,
				"delay", delay.String(),
				"error", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context canceled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		pool, err := NewPool(ctx, cfg)
		if err == nil {
			if attempt > 0 {
				slog.Info("database connection established after retry", "attempt", attempt)
			}
			return pool, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("failed to connect after %d attempts: %w", maxRetries+1, lastErr)
}
