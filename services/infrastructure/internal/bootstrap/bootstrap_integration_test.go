//go:build integration

package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setupTestDB connects to LETTUCE_TEST_DB_URL and DELETE-cleans the tables these
// tests touch. It never creates schema (the migrations own that) — the house
// pattern (see internal/apikey/pgx-repo_test.go).
func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM api_keys")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// seedUser inserts a minimal user with the given role and password hash, returning
// its id. Mirrors the apikey house helper's minimal insert.
func seedUser(t *testing.T, pool *pgxpool.Pool, role, passwordHash string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, role, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, id.String()+"@test.com", "user-"+id.String()[:8], role, passwordHash,
	)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestAdminUser_PlaceholderPasswordProduction: a placeholder LETTUCE_ADMIN_PASSWORD
// with production=true refuses to bootstrap and creates no user row (i).
func TestAdminUser_PlaceholderPasswordProduction(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Setenv("LETTUCE_ADMIN_EMAIL", "admin@test.com")
	t.Setenv("LETTUCE_ADMIN_PASSWORD", "change-me-to-a-strong-password")

	if err := AdminUser(ctx, pool, testLogger(), true); err == nil {
		t.Fatal("expected AdminUser to refuse a placeholder password in production")
	}
	if n := countRows(t, pool, "SELECT COUNT(*) FROM users"); n != 0 {
		t.Errorf("expected no user row after refusal, got %d", n)
	}
}

// TestAdminUser_PlaceholderPasswordDev: the same placeholder with production=false
// preserves today's behavior — it warns and creates the admin user (i).
func TestAdminUser_PlaceholderPasswordDev(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Setenv("LETTUCE_ADMIN_EMAIL", "admin@test.com")
	t.Setenv("LETTUCE_ADMIN_PASSWORD", "change-me-to-a-strong-password")

	if err := AdminUser(ctx, pool, testLogger(), false); err != nil {
		t.Fatalf("dev AdminUser should proceed, got: %v", err)
	}
	if n := countRows(t, pool, "SELECT COUNT(*) FROM users WHERE role = 'ADMIN'"); n != 1 {
		t.Errorf("expected 1 admin user in dev posture, got %d", n)
	}
}

// TestDashboardAPIKey_PlaceholderProduction: a placeholder DASHBOARD_API_KEY with
// production=true refuses and persists no api_keys row (ii).
func TestDashboardAPIKey_PlaceholderProduction(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Setenv("DASHBOARD_API_KEY", "generate-with-openssl-rand-base64-32")

	if err := DashboardAPIKey(ctx, pool, testLogger(), true); err == nil {
		t.Fatal("expected DashboardAPIKey to refuse a placeholder value in production")
	}
	if n := countRows(t, pool, "SELECT COUNT(*) FROM api_keys"); n != 0 {
		t.Errorf("expected no api_keys row after refusal, got %d", n)
	}
}

// TestSweepRevokesPlaceholderAPIKeys (BG-30b, iii): a pre-seeded api_keys row whose
// hash is a known placeholder is revoked by the sweep; a non-placeholder row in the
// same table survives untouched.
func TestSweepRevokesPlaceholderAPIKeys(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := seedUser(t, pool, "ADMIN", "bcrypt-does-not-matter-here")

	placeholderHash := apikey.HashKey("generate-with-openssl-rand-base64-32")
	realHash := apikey.HashKey("a-genuinely-real-non-placeholder-dashboard-key")

	_, err := pool.Exec(ctx, `
		INSERT INTO api_keys (user_id, name, key_prefix, key_hash) VALUES
		  ($1, 'placeholder key', 'ph000000', $2),
		  ($1, 'real key',        're000000', $3)`,
		userID, placeholderHash, realHash,
	)
	if err != nil {
		t.Fatalf("seed api keys: %v", err)
	}

	if err := SweepPlaceholderCredentials(ctx, pool, testLogger(), true); err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}

	if got := revokedAt(t, pool, placeholderHash); got == nil {
		t.Error("placeholder api key should have been revoked (revoked_at set)")
	}
	if got := revokedAt(t, pool, realHash); got != nil {
		t.Errorf("non-placeholder api key should survive, got revoked_at=%v", got)
	}
}

// TestSweepRefusesPlaceholderAdminPassword (BG-30b, iv): an ADMIN whose password_hash
// is the bcrypt of a known placeholder makes the sweep refuse to start.
func TestSweepRefusesPlaceholderAdminPassword(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	hash, err := bcrypt.GenerateFromPassword([]byte("change-me-to-a-strong-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	seedUser(t, pool, "ADMIN", string(hash))

	err = SweepPlaceholderCredentials(ctx, pool, testLogger(), true)
	if err == nil {
		t.Fatal("expected sweep to refuse a placeholder admin password")
	}
	if !strings.Contains(err.Error(), "refusing to start: admin user") {
		t.Errorf("error should carry the pinned refuse-to-start message: %v", err)
	}
}

// TestSweepAllowsStrongAdminPassword (BG-30b, iv): an ADMIN with a strong password
// (no placeholder match) passes the sweep.
func TestSweepAllowsStrongAdminPassword(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	hash, err := bcrypt.GenerateFromPassword([]byte("a-genuinely-strong-unique-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	seedUser(t, pool, "ADMIN", string(hash))

	if err := SweepPlaceholderCredentials(ctx, pool, testLogger(), true); err != nil {
		t.Fatalf("sweep should pass a strong admin password, got: %v", err)
	}
}

// TestSweepIgnoresPasswordlessAdmin (BG-30b): an OAuth-only ADMIN has a NULL
// password_hash (chk_users_auth_method is satisfied by github_id), so there is no
// placeholder password to refuse — the sweep must pass it, not crash scanning the
// NULL.
func TestSweepIgnoresPasswordlessAdmin(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	id := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, role, github_id)
		VALUES ($1, $2, $3, 'ADMIN', $4)`,
		id, id.String()+"@test.com", "user-"+id.String()[:8], "gh-"+id.String()[:8],
	)
	if err != nil {
		t.Fatalf("seed passwordless admin: %v", err)
	}

	if err := SweepPlaceholderCredentials(ctx, pool, testLogger(), true); err != nil {
		t.Fatalf("sweep must ignore a passwordless (OAuth-only) admin, got: %v", err)
	}
}

// revokedAt returns the revoked_at timestamp for the api key with the given hash
// (nil when the key is active or absent).
func revokedAt(t *testing.T, pool *pgxpool.Pool, keyHash []byte) *time.Time {
	t.Helper()
	var ts *time.Time
	err := pool.QueryRow(context.Background(),
		"SELECT revoked_at FROM api_keys WHERE key_hash = $1", keyHash).Scan(&ts)
	if err != nil {
		t.Fatalf("revoked_at query: %v", err)
	}
	return ts
}
