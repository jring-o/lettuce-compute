//go:build integration

package apikey

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

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
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

// createTestUser inserts a minimal user and returns the user ID.
func createTestUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, role, password_hash)
		VALUES ($1, $2, $3, 'USER', 'fakehash')`,
		id, id.String()+"@test.com", "user-"+id.String()[:8],
	)
	if err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return id
}

func TestCreateAndGetByHash(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key := &ApiKey{
		UserID:   userID,
		Name:     "Test Key",
		KeyPrefix: prefix,
		KeyHash:  hash,
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(key.ID) {
		t.Error("ID should be set after Create")
	}
	if key.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}

	got, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got == nil {
		t.Fatal("GetByHash returned nil for active key")
	}
	if got.ID != key.ID {
		t.Errorf("ID = %v, want %v", got.ID, key.ID)
	}
	if got.Name != "Test Key" {
		t.Errorf("Name = %q, want %q", got.Name, "Test Key")
	}
	if got.KeyPrefix != prefix {
		t.Errorf("KeyPrefix = %q, want %q", got.KeyPrefix, prefix)
	}
	if got.UserID != userID {
		t.Errorf("UserID = %v, want %v", got.UserID, userID)
	}
	if got.LastUsedAt != nil {
		t.Error("LastUsedAt should be nil for new key")
	}
	if got.RevokedAt != nil {
		t.Error("RevokedAt should be nil for active key")
	}
}

func TestGetByHash_Revoked(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key := &ApiKey{
		UserID:   userID,
		Name:     "Revoke Test",
		KeyPrefix: prefix,
		KeyHash:  hash,
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got != nil {
		t.Error("GetByHash should return nil for revoked key")
	}
}

func TestGetByHash_NotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	randomHash := sha256.Sum256([]byte("nonexistent-key"))
	got, err := repo.GetByHash(ctx, randomHash[:])
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got != nil {
		t.Error("GetByHash should return nil for non-existent hash")
	}
}

func TestListByUser(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	for i := 0; i < 3; i++ {
		_, prefix, hash, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey %d: %v", i, err)
		}
		key := &ApiKey{
			UserID:   userID,
			Name:     "Key " + prefix,
			KeyPrefix: prefix,
			KeyHash:  hash,
		}
		if err := repo.Create(ctx, key); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	keys, err := repo.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}

	// Verify descending order by created_at.
	for i := 1; i < len(keys); i++ {
		if keys[i].CreatedAt.After(keys[i-1].CreatedAt) {
			t.Errorf("keys not in descending order: key[%d].CreatedAt (%v) > key[%d].CreatedAt (%v)",
				i, keys[i].CreatedAt, i-1, keys[i-1].CreatedAt)
		}
	}
}

func TestListByUser_IncludesRevoked(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	var keyIDs []types.ID
	for i := 0; i < 3; i++ {
		_, prefix, hash, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey %d: %v", i, err)
		}
		key := &ApiKey{
			UserID:   userID,
			Name:     "Key " + prefix,
			KeyPrefix: prefix,
			KeyHash:  hash,
		}
		if err := repo.Create(ctx, key); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		keyIDs = append(keyIDs, key.ID)
	}

	// Revoke one key.
	if err := repo.Revoke(ctx, keyIDs[0]); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	keys, err := repo.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys (including revoked), got %d", len(keys))
	}
}

func TestRevoke(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key := &ApiKey{
		UserID:   userID,
		Name:     "Revoke Test",
		KeyPrefix: prefix,
		KeyHash:  hash,
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Verify revoked_at is set by listing keys.
	keys, err := repo.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].RevokedAt == nil {
		t.Error("RevokedAt should be non-nil after revoke")
	}
}

func TestRevoke_AlreadyRevoked(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key := &ApiKey{
		UserID:   userID,
		Name:     "Double Revoke",
		KeyPrefix: prefix,
		KeyHash:  hash,
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke first: %v", err)
	}

	err = repo.Revoke(ctx, key.ID)
	if err == nil {
		t.Fatal("expected error for already-revoked key")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestRevoke_NotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.Revoke(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent key")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestListByUser_Empty(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	keys, err := repo.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys for user with no keys, got %d", len(keys))
	}
}

func TestCreate_DuplicateHash(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key1 := &ApiKey{
		UserID:    userID,
		Name:      "Original Key",
		KeyPrefix: prefix,
		KeyHash:   hash,
	}
	if err := repo.Create(ctx, key1); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	// Insert a second key with the same hash — should trigger unique constraint.
	key2 := &ApiKey{
		UserID:    userID,
		Name:      "Duplicate Key",
		KeyPrefix: prefix,
		KeyHash:   hash,
	}
	err = repo.Create(ctx, key2)
	if err == nil {
		t.Fatal("expected error for duplicate key_hash")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409 (Conflict)", apiErr.HTTPStatus)
	}
}

func TestUpdateLastUsed_NonExistentID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// UpdateLastUsed on a non-existent ID should not error
	// (the current implementation does not check rows affected).
	err := repo.UpdateLastUsed(ctx, types.NewID())
	if err != nil {
		t.Fatalf("UpdateLastUsed on non-existent ID: %v", err)
	}
}

func TestUpdateLastUsed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()
	userID := createTestUser(t, pool)

	_, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	key := &ApiKey{
		UserID:   userID,
		Name:     "Last Used Test",
		KeyPrefix: prefix,
		KeyHash:  hash,
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.UpdateLastUsed(ctx, key.ID); err != nil {
		t.Fatalf("UpdateLastUsed: %v", err)
	}

	got, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got == nil {
		t.Fatal("GetByHash returned nil")
	}
	if got.LastUsedAt == nil {
		t.Error("LastUsedAt should be non-nil after UpdateLastUsed")
	}
}
