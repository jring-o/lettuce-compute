//go:build integration

package admission

// DB-backed integration tests for the registration proof-of-work store — IssueChallenge,
// RedeemChallenge, and the ChallengeSweeper. They pin the load-bearing single-use and
// consume-then-verify semantics of RedeemChallenge (a DELETE ... RETURNING that takes the
// row lock before the in-Go difficulty check), and — critically — the fact that the
// un-consume-on-failed-verify property belongs to the CALLER's transaction, not to
// RedeemChallenge run bare on the pool.
//
// These reuse the shared admission harness (setupTestDB in admission_integration_test.go,
// same package) which cleans registration_challenges at both ends. Like the rest of the
// integration suite they skip unless LETTUCE_TEST_DB_URL is set and must run with -p 1
// (they share one database and DELETE-clean between runs).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// powKey returns a deterministic 32-byte public key filled with b, distinct per test so
// challenge rows never alias across cases (challenges key on id, but distinct keys keep the
// wrong-pubkey case honest).
func powKey(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// wrongNonceFor returns the first nonce that does NOT meet difficulty for (challenge,
// pubkey). At difficulty 8 a random nonce fails with probability 255/256, so nonce 0 almost
// always qualifies and this returns immediately.
func wrongNonceFor(challenge, pubkey []byte, difficultyBits int) uint64 {
	for n := uint64(0); ; n++ {
		if !VerifySolution(challenge, pubkey, n, difficultyBits) {
			return n
		}
	}
}

// challengeExists reports whether a registration_challenges row with the given id is
// present. Used to assert consumption (single-use), survival (un-consume), and sweeping.
func challengeExists(t *testing.T, pool *pgxpool.Pool, id types.ID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM registration_challenges WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count challenge row %v: %v", id, err)
	}
	return n > 0
}

// TestIssueChallenge_StoresRow: issuing writes a row with a 32-byte random challenge, the
// caller's public key and difficulty, and an expires_at ≈ now + ttl.
func TestIssueChallenge_StoresRow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	pubkey := powKey(0x11)
	const difficulty = 8
	const ttl = 10 * time.Minute

	before := time.Now().UTC()
	c, err := IssueChallenge(ctx, pool, pubkey, difficulty, ttl)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	after := time.Now().UTC()

	var gotPubkey, gotChallenge []byte
	var gotDifficulty int
	var gotExpires time.Time
	if err := pool.QueryRow(ctx,
		`SELECT public_key, challenge, difficulty, expires_at
		 FROM registration_challenges WHERE id = $1`, c.ID).
		Scan(&gotPubkey, &gotChallenge, &gotDifficulty, &gotExpires); err != nil {
		t.Fatalf("issued row should exist: %v", err)
	}

	if len(gotChallenge) != challengeBytes {
		t.Errorf("stored challenge = %d bytes, want %d", len(gotChallenge), challengeBytes)
	}
	if !bytes.Equal(gotChallenge, c.Challenge) {
		t.Error("stored challenge bytes differ from the returned Challenge.Challenge")
	}
	if !bytes.Equal(gotPubkey, pubkey) {
		t.Error("stored public_key differs from the issued key")
	}
	if gotDifficulty != difficulty {
		t.Errorf("stored difficulty = %d, want %d", gotDifficulty, difficulty)
	}
	// expires_at must fall within [before+ttl, after+ttl] with a small tolerance for the
	// round-trip; compared as instants so timezone of the returned value is irrelevant.
	lo := before.Add(ttl).Add(-2 * time.Second)
	hi := after.Add(ttl).Add(2 * time.Second)
	if gotExpires.Before(lo) || gotExpires.After(hi) {
		t.Errorf("expires_at = %v, want within [%v, %v]", gotExpires, lo, hi)
	}
}

// TestRedeemChallenge_BarePoolSemantics documents the consume-then-verify behaviour when
// RedeemChallenge runs directly on the pool (no surrounding transaction): a correct nonce
// succeeds and the row is gone (single-use); a WRONG nonce returns ErrPowSolutionInvalid
// AND the row is ALSO gone — the DELETE auto-commits before the in-Go verify, so on a bare
// pool a failed verification still consumes the challenge. The un-consume-on-failure
// property belongs to the caller's transaction (tests below and in the volunteer package).
func TestRedeemChallenge_BarePoolSemantics(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const difficulty = 8

	// Correct nonce on the pool: nil error and the row is consumed.
	pubkey := powKey(0x21)
	c, err := IssueChallenge(ctx, pool, pubkey, difficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce := Solve(c.Challenge, pubkey, difficulty)
	if err := RedeemChallenge(ctx, pool, &PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce}); err != nil {
		t.Fatalf("correct redeem on pool: %v", err)
	}
	if challengeExists(t, pool, c.ID) {
		t.Error("row should be consumed (single-use) after a correct redeem")
	}

	// Wrong nonce on the pool: ErrPowSolutionInvalid, and (deliberately) the row is ALSO
	// gone because the DELETE auto-committed before the verify failed.
	pubkey2 := powKey(0x22)
	c2, err := IssueChallenge(ctx, pool, pubkey2, difficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge 2: %v", err)
	}
	wrong := wrongNonceFor(c2.Challenge, pubkey2, difficulty)
	err = RedeemChallenge(ctx, pool, &PowRedemption{ChallengeID: c2.ID, PublicKey: pubkey2, Nonce: wrong})
	if !errors.Is(err, ErrPowSolutionInvalid) {
		t.Fatalf("wrong redeem on pool: err=%v, want ErrPowSolutionInvalid", err)
	}
	if challengeExists(t, pool, c2.ID) {
		t.Error("bare-pool wrong-nonce redeem: row should be gone (DELETE auto-commits before the in-Go verify)")
	}
}

// TestRedeemChallenge_TxRollbackUnconsumes is the load-bearing retry property: a failed
// redeem inside a transaction that rolls back leaves the challenge row intact, so the
// client can retry with a correct nonce — which then succeeds in a fresh committed tx.
func TestRedeemChallenge_TxRollbackUnconsumes(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const difficulty = 8

	pubkey := powKey(0x33)
	c, err := IssueChallenge(ctx, pool, pubkey, difficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}

	// Wrong nonce inside a transaction, then roll back.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	wrong := wrongNonceFor(c.Challenge, pubkey, difficulty)
	err = RedeemChallenge(ctx, tx, &PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: wrong})
	if !errors.Is(err, ErrPowSolutionInvalid) {
		_ = tx.Rollback(ctx)
		t.Fatalf("wrong redeem in tx: err=%v, want ErrPowSolutionInvalid", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The rollback un-consumed the row: it still exists.
	if !challengeExists(t, pool, c.ID) {
		t.Fatal("row should survive a rolled-back failed redeem (un-consume property)")
	}

	// A subsequent correct-nonce redeem in a fresh committed tx succeeds and consumes it.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	nonce := Solve(c.Challenge, pubkey, difficulty)
	if err := RedeemChallenge(ctx, tx2, &PowRedemption{ChallengeID: c.ID, PublicKey: pubkey, Nonce: nonce}); err != nil {
		_ = tx2.Rollback(ctx)
		t.Fatalf("correct redeem in fresh tx: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if challengeExists(t, pool, c.ID) {
		t.Error("row should be consumed after the committed correct redeem")
	}
}

// TestRedeemChallenge_InvalidCases covers the three ErrPowChallengeInvalid paths: an
// unknown id, a challenge issued to A redeemed with key B (WHERE clause misses — nothing
// deleted, so the row survives), and an expired row.
func TestRedeemChallenge_InvalidCases(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const difficulty = 8

	// (a) Unknown challenge id → ErrPowChallengeInvalid.
	if err := RedeemChallenge(ctx, pool,
		&PowRedemption{ChallengeID: types.NewID(), PublicKey: powKey(0x41), Nonce: 0}); !errors.Is(err, ErrPowChallengeInvalid) {
		t.Fatalf("unknown id: err=%v, want ErrPowChallengeInvalid", err)
	}

	// (b) Issued to A, redeemed with pubkey B → ErrPowChallengeInvalid; the WHERE clause
	// (public_key = B) never matches the A row, so nothing is deleted and the row survives.
	keyA := powKey(0x42)
	keyB := powKey(0x43)
	c, err := IssueChallenge(ctx, pool, keyA, difficulty, time.Minute)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	if err := RedeemChallenge(ctx, pool,
		&PowRedemption{ChallengeID: c.ID, PublicKey: keyB, Nonce: 0}); !errors.Is(err, ErrPowChallengeInvalid) {
		t.Fatalf("wrong-pubkey redeem: err=%v, want ErrPowChallengeInvalid", err)
	}
	if !challengeExists(t, pool, c.ID) {
		t.Error("wrong-pubkey redeem: row should survive (WHERE clause did not match, nothing deleted)")
	}

	// (c) Expired row (seeded directly with expires_at in the past) → ErrPowChallengeInvalid.
	keyC := powKey(0x44)
	expiredID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO registration_challenges (id, public_key, challenge, difficulty, expires_at)
		VALUES ($1, $2, $3, $4, NOW() - interval '1 minute')`,
		expiredID, keyC, make([]byte, challengeBytes), difficulty); err != nil {
		t.Fatalf("seed expired row: %v", err)
	}
	if err := RedeemChallenge(ctx, pool,
		&PowRedemption{ChallengeID: expiredID, PublicKey: keyC, Nonce: 0}); !errors.Is(err, ErrPowChallengeInvalid) {
		t.Fatalf("expired redeem: err=%v, want ErrPowChallengeInvalid", err)
	}
}

// TestChallengeSweeper_PrunesExpired seeds an expired row and a live row directly, runs the
// sweeper (which sweeps once immediately, then blocks on a 5-minute ticker) in a goroutine,
// polls until the expired row is gone, then cancels the context. The live row survives.
// Mirrors TestCounterSweeper_PrunesOldRows.
func TestChallengeSweeper_PrunesExpired(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	expiredID := types.NewID()
	liveID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO registration_challenges (id, public_key, challenge, difficulty, expires_at)
		VALUES ($1, $2, $3, 8, NOW() - interval '1 minute')`,
		expiredID, powKey(0x51), make([]byte, challengeBytes)); err != nil {
		t.Fatalf("seed expired row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO registration_challenges (id, public_key, challenge, difficulty, expires_at)
		VALUES ($1, $2, $3, 8, NOW() + interval '10 minutes')`,
		liveID, powKey(0x52), make([]byte, challengeBytes)); err != nil {
		t.Fatalf("seed live row: %v", err)
	}

	sweeperCtx, cancel := context.WithCancel(ctx)
	sweeper := NewChallengeSweeper(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		sweeper.Start(sweeperCtx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for challengeExists(t, pool, expiredID) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("expired challenge not pruned within timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if !challengeExists(t, pool, liveID) {
		t.Error("live challenge was pruned, want it intact (not yet expired)")
	}
}
