//go:build integration

package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

func createTestUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, username+"@test.example.com", username, "Test User "+username,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return id
}

func createTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	slug := "test-att-" + uuid.New().String()[:8]
	_, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, 'A test leaf for attestation tests', 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":60,"heartbeat_timeout_seconds":300,"max_reassignments":3,"assignment_timeout_seconds":3600}',
			'{"input_format":"JSON","output_format":"JSON","max_input_size_mb":100,"max_output_size_mb":100}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $4
		)`, id, "Test Attestation Project "+slug, slug, creatorID,
	)
	if err != nil {
		t.Fatalf("create test leaf: %v", err)
	}
	return id
}

func createTestWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO work_units (id, leaf_id, state, code_artifact_ref, deadline_seconds)
		VALUES ($1, $2, 'VALIDATED', 'ref://test', 3600)`,
		id, leafID,
	)
	if err != nil {
		t.Fatalf("create test work unit: %v", err)
	}
	return id
}

func TestCreateAndList(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "att-creator1")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	att := &Attestation{
		LeafID:          leafID,
		VolunteerPublicKey: signer.PublicKey(),
		WorkUnitID:         wuID,
		RawMetrics: map[string]any{
			"wall_clock_seconds": float64(100),
			"cpu_seconds_user":   float64(90),
		},
		ValidationOutcome:   OutcomeAgreed,
		CreditAmount:        1.5,
		AttestationTimestamp: types.Now(),
	}

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	if err := repo.Create(ctx, att); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(att.ID) {
		t.Error("ID should be populated after Create")
	}

	// List by leaf.
	results, pagination, err := repo.List(ctx, ListFilters{LeafID: &leafID}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 attestation, got %d", len(results))
	}
	if results[0].ID != att.ID {
		t.Errorf("ID = %v, want %v", results[0].ID, att.ID)
	}
	if results[0].CreditAmount != 1.5 {
		t.Errorf("CreditAmount = %v, want 1.5", results[0].CreditAmount)
	}
	if pagination.HasMore {
		t.Error("HasMore should be false")
	}

	// Verify the signature is valid.
	if !VerifyAttestation(signer.PublicKey(), results[0]) {
		t.Error("retrieved attestation signature verification failed")
	}
}

func TestListByProjectWithTimeRange(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "att-creator2")
	leafID := createTestLeaf(t, pool, &userID)
	wu1 := createTestWorkUnit(t, pool, leafID)
	wu2 := createTestWorkUnit(t, pool, leafID)
	wu3 := createTestWorkUnit(t, pool, leafID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	now := types.Now()
	timestamps := []time.Time{
		now,
		now.Add(-24 * time.Hour),
		now.Add(-48 * time.Hour),
	}
	wuIDs := []types.ID{wu1, wu2, wu3}

	for i, ts := range timestamps {
		att := &Attestation{
			LeafID:           leafID,
			VolunteerPublicKey:  signer.PublicKey(),
			WorkUnitID:          wuIDs[i],
			RawMetrics:          map[string]any{"cpu": float64(100)},
			ValidationOutcome:   OutcomeAgreed,
			CreditAmount:        1.0,
			AttestationTimestamp: ts,
		}
		sig, _ := signer.Sign(att)
		att.Signature = sig
		if err := repo.Create(ctx, att); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// Filter: last 30 hours.
	from := types.FormatTimestamp(now.Add(-30 * time.Hour))
	to := types.FormatTimestamp(now.Add(time.Hour))
	results, _, err := repo.List(ctx, ListFilters{
		LeafID: &leafID,
		From:      &from,
		To:        &to,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 attestations within range, got %d", len(results))
	}
}

func TestListByVolunteerWithPagination(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "att-creator3")
	leafID := createTestLeaf(t, pool, &userID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Create 5 attestations.
	for i := 0; i < 5; i++ {
		wuID := createTestWorkUnit(t, pool, leafID)
		att := &Attestation{
			LeafID:           leafID,
			VolunteerPublicKey:  signer.PublicKey(),
			WorkUnitID:          wuID,
			RawMetrics:          map[string]any{"cpu": float64(i)},
			ValidationOutcome:   OutcomeAgreed,
			CreditAmount:        1.0,
			AttestationTimestamp: types.Now().Add(time.Duration(-i) * time.Minute),
		}
		sig, _ := signer.Sign(att)
		att.Signature = sig
		if err := repo.Create(ctx, att); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// Page 1 of 3.
	results, pagination, err := repo.List(ctx, ListFilters{
		VolunteerPublicKey: signer.PublicKey(),
	}, types.PaginationRequest{PageSize: 3})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("page 1: expected 3, got %d", len(results))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	// Page 2.
	results2, pagination2, err := repo.List(ctx, ListFilters{
		VolunteerPublicKey: signer.PublicKey(),
	}, types.PaginationRequest{PageSize: 3, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(results2) != 2 {
		t.Fatalf("page 2: expected 2, got %d", len(results2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}
}

