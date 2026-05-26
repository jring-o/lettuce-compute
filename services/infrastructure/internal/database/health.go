package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const healthCheckTimeout = 5 * time.Second

// HealthCheck verifies the database is reachable and responsive.
// Executes `SELECT 1` with a 5-second timeout.
// Returns nil on success, error on failure.
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	var n int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		return fmt.Errorf("database health check failed: %w", err)
	}
	return nil
}
