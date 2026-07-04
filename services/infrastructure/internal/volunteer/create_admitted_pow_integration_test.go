//go:build integration

package volunteer

// DB-backed integration tests for PgxRepository.CreateAdmitted's proof-of-work gate — the
// gate.Pow branch that redeems a single-use registration challenge inside the same
// transaction as the volunteer INSERT (and, when set, the creation-cap increment). They pin
// the load-bearing retry property: a valid solution is consumed only if the whole
// registration commits, so an unrelated refusal (the cap) or a bad nonce rolls the
// consumption back and the challenge survives for a retry.
//
// These reuse the shared volunteer integration harness (setupTestDB / newTestVolunteer /
// newTestPublicKey in pgx-repo_test.go) and the counter/row helpers in
// create_admitted_integration_test.go. setupTestDB cleans the volunteers table but neither
// admission table, so setupPowTestDB below layers registration_challenges +
// registration_creation_counts cleanup over it. Like the rest of the integration suite they
// skip unless LETTUCE_TEST_DB_URL is set and must run with -p 1 (they share one database and
// DELETE-clean between runs).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// setupPowTestDB wraps the shared setupTestDB helper, additionally cleaning BOTH admission
// tables (registration_challenges and registration_creation_counts) at both ends. setupTestDB
// touches neither, so without this a leaked challenge row or (bucket, day) counter would
// bleed into the next serialized (-p 1) test. This is the create_admitted counterpart of
// setupCreationCapTestDB, extended to also purge challenges.
func setupPowTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	pool, base := setupTestDB(t)
	ctx := context.Background()
	cleanAdmission := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM registration_challenges")
		_, _ = pool.Exec(ctx, "DELETE FROM registration_creation_counts")
	}
	cleanAdmission()
	return pool, func() {
		cleanAdmission()
		base()
	}
}

// challengeRowExists reports whether a registration_challenges row with the given id is
// present — used to assert a challenge was consumed (create committed) or survived (the tx
// rolled its consumption back).
func challengeRowExists(t *testing.T, pool *pgxpool.Pool, id types.ID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM registration_challenges WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count challenge row %v: %v", id, err)
	}
	return n > 0
}

// wrongPowNonce returns the first nonce that does NOT satisfy difficulty for (challenge,
// pubkey). At difficulty 8 a random nonce fails with probability 255/256, so this returns
// almost immediately. Uses the exported admission.VerifySolution (the shared solution rule).
func wrongPowNonce(challenge, pubkey []byte, difficultyBits int) uint64 {
	for n := uint64(0); ; n++ {
		if !admission.VerifySolution(challenge, pubkey, n, difficultyBits) {
			return n
		}
	}
}

const powTestDifficulty = 8

// TestCreateAdmitted_PowValidSolutionCreates: a gate carrying a correct solution and NO cap
// (CapPerDay 0) creates the volunteer, consumes the challenge, and — pinning gate
// orthogonality — writes no counter row.
func TestCreateAdmitted_PowValidSolutionCreates(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	pubkey := v.PublicKey
	c, err := admission.IssueChallenge(ctx, pool, pubkey, powTestDifficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce := admission.Solve(c.Challenge, pubkey, powTestDifficulty)

	gate := &admission.CreateGate{
		Pow: &admission.PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce},
		// CapPerDay 0 → creation cap off.
	}
	if err := repo.CreateAdmitted(ctx, v, gate); err != nil {
		t.Fatalf("CreateAdmitted (valid pow, no cap): %v", err)
	}
	if types.IsNilID(v.ID) {
		t.Error("volunteer ID should be set after a valid-pow create")
	}
	if _, err := repo.GetByID(ctx, v.ID); err != nil {
		t.Fatalf("created volunteer row should exist: %v", err)
	}
	if challengeRowExists(t, pool, c.ID) {
		t.Error("challenge row should be consumed after a committed create")
	}
	if n := totalCounterRows(t, pool); n != 0 {
		t.Errorf("registration_creation_counts rows = %d, want 0 (cap off, gates orthogonal)", n)
	}
}

// TestCreateAdmitted_PowWrongNonce: a gate with a wrong nonce is refused with
// ErrPowSolutionInvalid, inserts no volunteer, and — the load-bearing retry property — the
// tx rollback un-consumes the challenge so the row SURVIVES.
func TestCreateAdmitted_PowWrongNonce(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	pubkey := v.PublicKey
	c, err := admission.IssueChallenge(ctx, pool, pubkey, powTestDifficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	wrong := wrongPowNonce(c.Challenge, pubkey, powTestDifficulty)

	gate := &admission.CreateGate{
		Pow: &admission.PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: wrong},
	}
	err = repo.CreateAdmitted(ctx, v, gate)
	if !errors.Is(err, admission.ErrPowSolutionInvalid) {
		t.Fatalf("wrong-nonce create: err=%v, want admission.ErrPowSolutionInvalid", err)
	}
	if n := volunteerRowCount(t, pool); n != 0 {
		t.Errorf("volunteer rows = %d, want 0 (refused create inserted nothing)", n)
	}
	if !challengeRowExists(t, pool, c.ID) {
		t.Error("challenge row should survive a failed create (tx rollback un-consumes)")
	}
}

// TestCreateAdmitted_PowUnknownChallenge: a gate referencing a random (nonexistent)
// challenge id is refused with ErrPowChallengeInvalid and creates nothing.
func TestCreateAdmitted_PowUnknownChallenge(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	gate := &admission.CreateGate{
		Pow: &admission.PowRedemption{ChallengeID: types.NewID(), PublicKey: v.PublicKey, Nonce: 0},
	}
	err := repo.CreateAdmitted(ctx, v, gate)
	if !errors.Is(err, admission.ErrPowChallengeInvalid) {
		t.Fatalf("unknown-challenge create: err=%v, want admission.ErrPowChallengeInvalid", err)
	}
	if n := volunteerRowCount(t, pool); n != 0 {
		t.Errorf("volunteer rows = %d, want 0", n)
	}
}

// TestCreateAdmitted_PowSingleUse: a valid create consumes the challenge, so a SECOND
// create (fresh key) reusing the same challenge id is refused with ErrPowChallengeInvalid —
// the row is gone, so the WHERE id clause matches nothing.
func TestCreateAdmitted_PowSingleUse(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v1 := newTestVolunteer()
	pubkey := v1.PublicKey
	c, err := admission.IssueChallenge(ctx, pool, pubkey, powTestDifficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce := admission.Solve(c.Challenge, pubkey, powTestDifficulty)

	gate1 := &admission.CreateGate{
		Pow: &admission.PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce},
	}
	if err := repo.CreateAdmitted(ctx, v1, gate1); err != nil {
		t.Fatalf("first create (valid pow): %v", err)
	}
	if challengeRowExists(t, pool, c.ID) {
		t.Fatal("challenge should be consumed after the first create")
	}

	// Second create, fresh key, same (now-gone) challenge id.
	v2 := newTestVolunteer()
	gate2 := &admission.CreateGate{
		Pow: &admission.PowRedemption{ChallengeID: c.ID, PublicKey: v2.PublicKey, Nonce: 0},
	}
	err = repo.CreateAdmitted(ctx, v2, gate2)
	if !errors.Is(err, admission.ErrPowChallengeInvalid) {
		t.Fatalf("reused-challenge create: err=%v, want admission.ErrPowChallengeInvalid", err)
	}
	if n := volunteerRowCount(t, pool); n != 1 {
		t.Errorf("volunteer rows = %d, want 1 (only the first create committed)", n)
	}
}

// TestCreateAdmitted_PowValidButCapExceeded: with cap 1 already spent in bucket X, a gate
// carrying a VALID pow plus bucket X at cap 1 is refused with ErrCreationCapExceeded, and
// the challenge row SURVIVES — because pow is redeemed before the cap is reserved (design
// order), and the cap refusal rolls the whole tx (including the consumption) back. The
// solver spent no challenge and can retry tomorrow.
func TestCreateAdmitted_PowValidButCapExceeded(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucket := "203.0.113.60"

	// Spend the cap with a pow-free create in the bucket.
	v1 := newTestVolunteer()
	if err := repo.CreateAdmitted(ctx, v1, &admission.CreateGate{Bucket: bucket, CapPerDay: 1}); err != nil {
		t.Fatalf("first (cap-spending) create: %v", err)
	}
	if count, _ := creationCount(t, pool, bucket); count != 1 {
		t.Fatalf("after first create: count=%d, want 1", count)
	}

	// Valid pow, same bucket, cap 1 already spent.
	v2 := newTestVolunteer()
	pubkey := v2.PublicKey
	c, err := admission.IssueChallenge(ctx, pool, pubkey, powTestDifficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce := admission.Solve(c.Challenge, pubkey, powTestDifficulty)
	gate := &admission.CreateGate{
		Bucket:    bucket,
		CapPerDay: 1,
		Pow:       &admission.PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce},
	}
	err = repo.CreateAdmitted(ctx, v2, gate)
	if !errors.Is(err, admission.ErrCreationCapExceeded) {
		t.Fatalf("valid-pow over-cap create: err=%v, want admission.ErrCreationCapExceeded", err)
	}
	if !challengeRowExists(t, pool, c.ID) {
		t.Error("challenge row should survive the cap refusal (consumption rolled back with the tx)")
	}
	if n := volunteerRowCount(t, pool); n != 1 {
		t.Errorf("volunteer rows = %d, want 1 (over-cap create inserted nothing)", n)
	}
	if count, exists := creationCount(t, pool, bucket); !exists || count != 1 {
		t.Errorf("bucket counter = %d exists=%v, want 1/true (over-cap increment rolled back)", count, exists)
	}
}

// TestCreateAdmitted_PowAndCapBothPass: a gate with a valid pow and a cap slot available
// (bucket Y, cap 5) creates the volunteer, consumes the challenge, and counts exactly one
// creation in the bucket.
func TestCreateAdmitted_PowAndCapBothPass(t *testing.T) {
	pool, cleanup := setupPowTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	bucket := "203.0.113.61"

	v := newTestVolunteer()
	pubkey := v.PublicKey
	c, err := admission.IssueChallenge(ctx, pool, pubkey, powTestDifficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce := admission.Solve(c.Challenge, pubkey, powTestDifficulty)

	gate := &admission.CreateGate{
		Bucket:    bucket,
		CapPerDay: 5,
		Pow:       &admission.PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce},
	}
	if err := repo.CreateAdmitted(ctx, v, gate); err != nil {
		t.Fatalf("CreateAdmitted (valid pow + cap slot): %v", err)
	}
	if _, err := repo.GetByID(ctx, v.ID); err != nil {
		t.Fatalf("created volunteer row should exist: %v", err)
	}
	if challengeRowExists(t, pool, c.ID) {
		t.Error("challenge row should be consumed after a committed create")
	}
	if count, exists := creationCount(t, pool, bucket); !exists || count != 1 {
		t.Errorf("bucket counter = %d exists=%v, want 1/true", count, exists)
	}
}
