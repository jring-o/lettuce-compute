//go:build integration

package volunteer

// DB-backed integration tests for PgxRepository.CreateAdmitted — the transactional
// registration-admission create path. They pin the load-bearing exactness property of the
// per-(IP bucket, UTC day) creation cap: the counter increment and the volunteer INSERT
// commit or roll back together, so the counter counts exactly the creations that committed
// and a refused or racing registration never burns a cap slot.
//
// These reuse the shared volunteer integration harness (setupTestDB / newTestVolunteer /
// newTestPublicKey in pgx-repo_test.go). setupTestDB's cleanup deletes the volunteers table
// but NOT the admission counter table, so setupCreationCapTestDB below layers
// registration_creation_counts cleanup over it. Like the rest of the integration suite these
// skip unless LETTUCE_TEST_DB_URL is set and must run with -p 1 (they share one database and
// DELETE-clean between runs).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setupCreationCapTestDB wraps the shared setupTestDB helper, additionally cleaning the
// registration_creation_counts table at both ends. setupTestDB's own cleanup does not touch
// that table, so without this a leaked (bucket, day) counter row would bleed a spent cap
// into the next serialized (-p 1) test.
func setupCreationCapTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	pool, base := setupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM registration_creation_counts"); err != nil {
		base()
		t.Fatalf("clean registration_creation_counts: %v", err)
	}
	return pool, func() {
		_, _ = pool.Exec(ctx, "DELETE FROM registration_creation_counts")
		base()
	}
}

// creationCount returns the created_count for (bucket, current UTC day) and whether the
// counter row exists at all. It keys exactly the way ReserveCreationSlot does, so assertions
// read precisely what the gate wrote.
func creationCount(t *testing.T, pool *pgxpool.Pool, bucket string) (count int, exists bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT created_count FROM registration_creation_counts
		 WHERE bucket = $1 AND day = (NOW() AT TIME ZONE 'utc')::date`, bucket).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read creation count for bucket %q: %v", bucket, err)
	}
	return count, true
}

// totalCounterRows counts all registration_creation_counts rows — used to prove a nil-gate
// create writes nothing to the counter table.
func totalCounterRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM registration_creation_counts").Scan(&n); err != nil {
		t.Fatalf("count registration_creation_counts rows: %v", err)
	}
	return n
}

// volunteerRowCount counts all volunteer rows — used to prove a refused create inserted
// nothing.
func volunteerRowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM volunteers").Scan(&n); err != nil {
		t.Fatalf("count volunteers rows: %v", err)
	}
	return n
}

// TestCreateAdmitted_NilGate_PlainCreate: a nil gate is exactly Create — it populates the
// row's id/registered_at and writes NO counter row.
func TestCreateAdmitted_NilGate_PlainCreate(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v, nil); err != nil {
		t.Fatalf("CreateAdmitted(nil gate): %v", err)
	}
	if types.IsNilID(v.ID) {
		t.Error("ID should be set after a nil-gate CreateAdmitted")
	}
	if v.RegisteredAt.IsZero() {
		t.Error("RegisteredAt should be populated after a nil-gate CreateAdmitted")
	}
	if n := totalCounterRows(t, pool); n != 0 {
		t.Errorf("registration_creation_counts rows = %d, want 0 for a nil-gate create", n)
	}
}

// TestCreateAdmitted_GateIncrementsAndCreates: a gate creates the volunteer row AND its
// (bucket, today) counter row, and a second distinct-key create in the same bucket advances
// the counter to 2.
func TestCreateAdmitted_GateIncrementsAndCreates(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucket := "203.0.113.10"
	gate := &admission.CreateGate{Bucket: bucket, CapPerDay: 2}

	v1 := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v1, gate); err != nil {
		t.Fatalf("CreateAdmitted first: %v", err)
	}
	if types.IsNilID(v1.ID) {
		t.Error("first volunteer ID should be set")
	}
	if _, err := repo.GetByID(ctx, v1.ID); err != nil {
		t.Fatalf("first volunteer row should exist: %v", err)
	}
	if count, exists := creationCount(t, pool, bucket); !exists || count != 1 {
		t.Errorf("after first create: count=%d exists=%v, want 1/true", count, exists)
	}

	v2 := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v2, gate); err != nil {
		t.Fatalf("CreateAdmitted second: %v", err)
	}
	if _, err := repo.GetByID(ctx, v2.ID); err != nil {
		t.Fatalf("second volunteer row should exist: %v", err)
	}
	if count, exists := creationCount(t, pool, bucket); !exists || count != 2 {
		t.Errorf("after second create: count=%d exists=%v, want 2/true", count, exists)
	}
}

// TestCreateAdmitted_CapRefusalLeavesNothing: at cap 1 the second create returns
// ErrCreationCapExceeded, inserts no second volunteer row, and the rollback leaves the
// counter at exactly 1 (the load-bearing exactness property).
func TestCreateAdmitted_CapRefusalLeavesNothing(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucket := "203.0.113.20"
	gate := &admission.CreateGate{Bucket: bucket, CapPerDay: 1}

	v1 := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v1, gate); err != nil {
		t.Fatalf("first create (within cap): %v", err)
	}
	if count, _ := creationCount(t, pool, bucket); count != 1 {
		t.Fatalf("after first create: count=%d, want 1", count)
	}

	v2 := newTestVolunteer()
	err := repo.CreateAdmitted(ctx, v2, gate)
	if !errors.Is(err, admission.ErrCreationCapExceeded) {
		t.Fatalf("second create over cap: err=%v, want admission.ErrCreationCapExceeded", err)
	}
	// The refused volunteer must not have been inserted.
	if _, getErr := repo.GetByPublicKey(ctx, v2.PublicKey); getErr == nil {
		t.Error("refused volunteer row should not exist")
	}
	if n := volunteerRowCount(t, pool); n != 1 {
		t.Errorf("volunteer rows = %d, want 1 (refused create inserted nothing)", n)
	}
	// The rollback must have un-incremented the counter back to exactly 1.
	if count, exists := creationCount(t, pool, bucket); !exists || count != 1 {
		t.Errorf("after refusal: count=%d exists=%v, want 1/true (rollback un-increments)", count, exists)
	}
}

// TestCreateAdmitted_DuplicateKeyRollsBackIncrement: re-registering the SAME key in the same
// bucket increments the counter inside the tx, then fails createIn on the unique-key
// violation (apierror.Conflict / HTTP 409); the whole tx rolls back so the counter stays at
// 1. This pins that a re-registration race never burns a cap slot.
func TestCreateAdmitted_DuplicateKeyRollsBackIncrement(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucket := "203.0.113.30"
	gate := &admission.CreateGate{Bucket: bucket, CapPerDay: 10}

	v1 := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v1, gate); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if count, _ := creationCount(t, pool, bucket); count != 1 {
		t.Fatalf("after first create: count=%d, want 1", count)
	}

	dup := newTestVolunteer()
	dup.PublicKey = v1.PublicKey // same key K
	err := repo.CreateAdmitted(ctx, dup, gate)
	if err == nil {
		t.Fatal("duplicate-key create should have failed")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("duplicate-key create: err=%v (%T), want *apierror.APIError", err, err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409 (Conflict)", apiErr.HTTPStatus)
	}
	// The failed tx must have rolled its increment back: counter still 1, not 2.
	if count, exists := creationCount(t, pool, bucket); !exists || count != 1 {
		t.Errorf("after duplicate-key rollback: count=%d exists=%v, want 1/true", count, exists)
	}
}

// TestCreateAdmitted_ConcurrentSameBucketExactness: at cap 5, 10 goroutines each create a
// fresh keypair in one bucket → exactly 5 volunteer rows created and exactly 5 counted; the
// other 5 all return ErrCreationCapExceeded. The counter-row lock serializes the racers, so
// the cap is exact.
func TestCreateAdmitted_ConcurrentSameBucketExactness(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	const capPerDay = 5
	const goroutines = 10
	bucket := "203.0.113.40"
	gate := &admission.CreateGate{Bucket: bucket, CapPerDay: capPerDay}

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			v := newTestVolunteer()
			errs[idx] = repo.CreateAdmitted(ctx, v, gate)
		}(i)
	}
	wg.Wait()

	var okCount, capCount int
	for i, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, admission.ErrCreationCapExceeded):
			capCount++
		default:
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if okCount != capPerDay {
		t.Errorf("successful creates = %d, want %d", okCount, capPerDay)
	}
	if capCount != goroutines-capPerDay {
		t.Errorf("cap-exceeded refusals = %d, want %d", capCount, goroutines-capPerDay)
	}
	// Exactly the committed creations are counted, and exactly that many rows exist.
	if count, exists := creationCount(t, pool, bucket); !exists || count != capPerDay {
		t.Errorf("counter = %d exists=%v, want %d/true", count, exists, capPerDay)
	}
	if n := volunteerRowCount(t, pool); n != capPerDay {
		t.Errorf("volunteer rows = %d, want %d", n, capPerDay)
	}
}

// TestCreateAdmitted_DistinctBucketsIndependent: at cap 1, one create in each of two
// different buckets both succeed — a bucket's spent cap does not refuse another bucket.
func TestCreateAdmitted_DistinctBucketsIndependent(t *testing.T) {
	pool, cleanup := setupCreationCapTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucketA := "203.0.113.50"
	bucketB := "198.51.100.50"
	gateA := &admission.CreateGate{Bucket: bucketA, CapPerDay: 1}
	gateB := &admission.CreateGate{Bucket: bucketB, CapPerDay: 1}

	vA := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, vA, gateA); err != nil {
		t.Fatalf("create in bucket A: %v", err)
	}
	vB := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, vB, gateB); err != nil {
		t.Fatalf("create in bucket B (cap 1 spent only on A): %v", err)
	}
	if count, exists := creationCount(t, pool, bucketA); !exists || count != 1 {
		t.Errorf("bucket A counter = %d exists=%v, want 1/true", count, exists)
	}
	if count, exists := creationCount(t, pool, bucketB); !exists || count != 1 {
		t.Errorf("bucket B counter = %d exists=%v, want 1/true", count, exists)
	}
}
