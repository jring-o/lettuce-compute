package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
)

// AdminUser runs on startup to ensure an admin user exists.
// If LETTUCE_ADMIN_EMAIL and LETTUCE_ADMIN_PASSWORD are set and no admin
// user exists in the database, one is created with the given credentials.
// Idempotent — safe to run on every startup.
func AdminUser(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	email := os.Getenv("LETTUCE_ADMIN_EMAIL")
	password := os.Getenv("LETTUCE_ADMIN_PASSWORD")
	if email == "" || password == "" {
		logger.Debug("LETTUCE_ADMIN_EMAIL or LETTUCE_ADMIN_PASSWORD not set, skipping admin bootstrap")
		return nil
	}

	// Check if any admin user already exists.
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE role = 'ADMIN'").Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		logger.Debug("admin user already exists, skipping bootstrap")
		return nil
	}

	// Derive username from email (part before @), enforce constraints.
	username := os.Getenv("LETTUCE_ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}

	displayName := os.Getenv("LETTUCE_ADMIN_DISPLAY_NAME")
	if displayName == "" {
		displayName = "Administrator"
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO users (email, email_verified, username, display_name, password_hash, role)
		VALUES ($1, true, $2, $3, $4, 'ADMIN')`,
		email, username, displayName, string(hash),
	)
	if err != nil {
		return err
	}

	logger.Info("admin user created via bootstrap", "email", email, "username", username)
	return nil
}

// DashboardAPIKey runs on startup to ensure the dashboard's API key exists
// in the database. If DASHBOARD_API_KEY is set and no matching key exists,
// one is created for the admin user.
// Idempotent — safe to run on every startup.
func DashboardAPIKey(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	dashKey := os.Getenv("DASHBOARD_API_KEY")
	if dashKey == "" || dashKey == "placeholder" {
		logger.Debug("DASHBOARD_API_KEY not set or placeholder, skipping dashboard key bootstrap")
		return nil
	}

	// Check if a key with this hash already exists.
	keyHash := apikey.HashKey(dashKey)
	var exists bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL)", keyHash,
	).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		logger.Debug("dashboard API key already exists, skipping bootstrap")
		return nil
	}

	// Find the admin user to own this key.
	var adminID string
	err = pool.QueryRow(ctx, "SELECT id FROM users WHERE role = 'ADMIN' ORDER BY created_at LIMIT 1").Scan(&adminID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("no admin user found, cannot bootstrap dashboard API key — create an admin user first")
			return nil
		}
		return err
	}

	// Prefix for display: first 8 chars or less.
	prefix := dashKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO api_keys (user_id, name, key_prefix, key_hash)
		VALUES ($1, 'Dashboard Service Key', $2, $3)`,
		adminID, prefix, keyHash,
	)
	if err != nil {
		return err
	}

	logger.Info("dashboard API key created via bootstrap")
	return nil
}
