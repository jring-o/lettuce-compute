//go:build integration

package database

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/config"
)

func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LETTUCE_TEST_DB_URL")
	if url == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set, skipping integration test")
	}
	return url
}

func testDBConfig(t *testing.T) config.DatabaseConfig {
	t.Helper()
	_ = testDBURL(t) // ensure env var is set
	return config.DatabaseConfig{
		Host:            envOrDefault("LETTUCE_TEST_DB_HOST", "localhost"),
		Port:            envOrDefaultInt("LETTUCE_TEST_DB_PORT", 5433),
		Database:        envOrDefault("LETTUCE_TEST_DB_NAME", "lettuce_test"),
		User:            envOrDefault("LETTUCE_TEST_DB_USER", "lettuce"),
		Password:        os.Getenv("LETTUCE_TEST_DB_PASSWORD"),
		SSLMode:         "disable",
		MaxConns:        5,
		MinConns:        1,
		MaxConnLifetime: "5m",
		MaxConnIdleTime: "1m",
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// setupMigrations runs migrations against the test database.
func setupMigrations(t *testing.T) {
	t.Helper()
	url := testDBURL(t)
	if err := RunMigrations(url); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
}

func TestNewPool_Success(t *testing.T) {
	cfg := testDBConfig(t)
	setupMigrations(t)

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		t.Fatalf("pool query failed: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1, got %d", n)
	}
}

func TestNewPool_InvalidConfig(t *testing.T) {
	_ = testDBURL(t) // skip if no DB env set

	cfg := config.DatabaseConfig{
		Host:     "invalid-host-that-does-not-exist",
		Port:     5432,
		Database: "nonexistent",
		User:     "nobody",
		SSLMode:  "disable",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := NewPool(ctx, cfg)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestConnectWithRetry_Success(t *testing.T) {
	cfg := testDBConfig(t)
	setupMigrations(t)

	ctx := context.Background()
	pool, err := ConnectWithRetry(ctx, cfg, 0, time.Second)
	if err != nil {
		t.Fatalf("ConnectWithRetry returned error: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		t.Fatalf("pool query failed: %v", err)
	}
}

func TestConnectWithRetry_AllFail(t *testing.T) {
	_ = testDBURL(t)

	cfg := config.DatabaseConfig{
		Host:     "invalid-host-that-does-not-exist",
		Port:     5432,
		Database: "nonexistent",
		User:     "nobody",
		SSLMode:  "disable",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := ConnectWithRetry(ctx, cfg, 2, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
}

func TestConnectWithRetry_ContextCanceled(t *testing.T) {
	_ = testDBURL(t)

	cfg := config.DatabaseConfig{
		Host:     "invalid-host-that-does-not-exist",
		Port:     5432,
		Database: "nonexistent",
		User:     "nobody",
		SSLMode:  "disable",
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the retry loop sees a canceled context.
	cancel()

	_, err := ConnectWithRetry(ctx, cfg, 5, time.Second)
	if err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
}

func TestHealthCheck_Success(t *testing.T) {
	cfg := testDBConfig(t)
	setupMigrations(t)

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}
	defer pool.Close()

	if err := HealthCheck(ctx, pool); err != nil {
		t.Fatalf("HealthCheck returned error: %v", err)
	}
}

func TestHealthCheck_Failure(t *testing.T) {
	cfg := testDBConfig(t)
	setupMigrations(t)

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}
	// Close pool to simulate unreachable DB.
	pool.Close()

	if err := HealthCheck(ctx, pool); err == nil {
		t.Fatal("expected error for closed pool, got nil")
	}
}

func TestRunMigrations_Success(t *testing.T) {
	url := testDBURL(t)

	if err := RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations returned error: %v", err)
	}

	// Verify all 17 tables exist.
	cfg := testDBConfig(t)
	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}
	defer pool.Close()

	expectedTables := []string{
		"users", "volunteers", "leafs", "batches", "work_units",
		"results", "credit_ledger", "volunteer_rac", "credit_attestations",
		"leaf_stats_snapshots", "work_unit_assignment_history",
		"volunteer_leaf_preferences", "research_areas", "file_uploads",
		"accounts", "sessions", "verification_tokens",
	}

	for _, table := range expectedTables {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)",
			table,
		).Scan(&exists)
		if err != nil {
			t.Errorf("error checking table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("expected table %s to exist", table)
		}
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	url := testDBURL(t)

	if err := RunMigrations(url); err != nil {
		t.Fatalf("first RunMigrations returned error: %v", err)
	}
	if err := RunMigrations(url); err != nil {
		t.Fatalf("second RunMigrations returned error (should be idempotent): %v", err)
	}
}

func TestRunMigrations_VerifySchema(t *testing.T) {
	url := testDBURL(t)
	if err := RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations returned error: %v", err)
	}

	cfg := testDBConfig(t)
	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}
	defer pool.Close()

	// Verify 12 research_areas seed rows.
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM research_areas").Scan(&count); err != nil {
		t.Fatalf("counting research_areas: %v", err)
	}
	if count != 12 {
		t.Errorf("expected 12 research_areas, got %d", count)
	}

	// Verify enum types exist with correct values.
	enumTests := []struct {
		typeName string
		expected []string
	}{
		{"leaf_state", []string{"DRAFT", "CONFIGURING", "ACTIVE", "PAUSED", "COMPLETED", "ARCHIVED"}},
		{"work_unit_state", []string{"CREATED", "QUEUED", "ASSIGNED", "RUNNING", "COMPLETED", "VALIDATED", "REJECTED", "EXPIRED", "FAILED"}},
		{"work_unit_priority", []string{"NORMAL", "HIGH", "CRITICAL"}},
		{"task_pattern", []string{"PARAMETER_SWEEP", "MAP_REDUCE", "MONTE_CARLO", "CUSTOM"}},
		{"comparison_mode", []string{"EXACT", "NUMERIC_TOLERANCE", "CUSTOM"}},
		{"runtime_type", []string{"NATIVE", "CONTAINER", "WASM", "SCRIPT"}},
		{"leaf_visibility", []string{"PUBLIC", "UNLISTED", "PRIVATE"}},
		{"validation_status", []string{"PENDING", "AGREED", "DISAGREED", "AWAITING_CONTENT_VERIFICATION", "CONTENT_VERIFICATION_FAILED", "SUPERSEDED"}},
		{"assignment_outcome", []string{"COMPLETED", "EXPIRED", "ABANDONED", "REJECTED", "SUPERSEDED"}},
	}

	for _, et := range enumTests {
		rows, err := pool.Query(ctx,
			"SELECT unnest(enum_range(NULL::"+et.typeName+"))::text",
		)
		if err != nil {
			t.Errorf("querying enum %s: %v", et.typeName, err)
			continue
		}

		var values []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Errorf("scanning enum value for %s: %v", et.typeName, err)
			}
			values = append(values, v)
		}
		rows.Close()

		if len(values) != len(et.expected) {
			t.Errorf("enum %s: expected %d values, got %d (%v)", et.typeName, len(et.expected), len(values), values)
			continue
		}
		for i, v := range values {
			if v != et.expected[i] {
				t.Errorf("enum %s[%d]: expected %q, got %q", et.typeName, i, et.expected[i], v)
			}
		}
	}

	// Verify leafs table has CHECK constraints.
	var constraintCount int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.table_constraints
		 WHERE table_name = 'leafs' AND constraint_type = 'CHECK'`,
	).Scan(&constraintCount)
	if err != nil {
		t.Fatalf("querying leaf constraints: %v", err)
	}
	if constraintCount < 2 {
		t.Errorf("expected at least 2 CHECK constraints on leafs, got %d", constraintCount)
	}
}
