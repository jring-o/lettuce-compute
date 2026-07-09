package admission

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/pow"
)

// PowPolicy is the head's registration proof-of-work configuration, a plain struct (no
// config-package dependency) filled from HeadConfig.Effective* values. The zero value is
// the deploy-safety default: enforcement off. Enforcement MUST stay off until
// solver-capable clients ship — no current volunteer CLI or dashboard build can solve a
// challenge, and there is no version field in the register request to grandfather them.
type PowPolicy struct {
	// Enabled turns proof-of-work enforcement on for the CREATE branch of registration.
	Enabled bool
	// DifficultyBits is the required number of leading zero bits in the solution digest
	// (effective, already-defaulted; ~2^bits hash attempts expected).
	DifficultyBits int
	// ChallengeTTL is how long an issued challenge stays redeemable.
	ChallengeTTL time.Duration
}

// PowRedemption carries a client's claimed solution into the transactional create path
// (CreateGate.Pow). The redeem consumes the challenge row inside the registration
// transaction, so an unrelated refusal (e.g. the creation cap) rolls the consumption
// back and the solution survives for a retry.
type PowRedemption struct {
	ChallengeID types.ID
	// PublicKey is the registering key; the challenge must have been issued to it and
	// it is part of the solution-hash preimage, so a solution is single-purpose.
	PublicKey []byte
	Nonce     uint64
}

// Challenge is an issued registration proof-of-work challenge.
type Challenge struct {
	ID             types.ID
	PublicKey      []byte
	Challenge      []byte
	DifficultyBits int
	ExpiresAt      time.Time
}

// ErrPowChallengeInvalid is returned by RedeemChallenge when the claimed challenge does
// not exist, has expired, or was issued to a different public key. Handlers map it to
// InvalidArgument / 400 — a solver-capable client sent something stale or foreign, which
// is distinct from the pow-required signal old clients receive.
var ErrPowChallengeInvalid = errors.New("registration proof-of-work challenge is unknown, expired, or issued to a different key")

// ErrPowSolutionInvalid is returned by RedeemChallenge when the challenge row was valid
// but the nonce does not meet the difficulty target. Same InvalidArgument / 400 mapping.
var ErrPowSolutionInvalid = errors.New("registration proof-of-work solution does not meet the difficulty target")

// PowRequiredMessagePrefix is the machine-readable contract future solver-capable
// clients match on a FailedPrecondition to trigger their fetch-solve-retry flow. Pinned:
// changing it orphans shipped clients.
const PowRequiredMessagePrefix = "registration requires proof-of-work"

// PowRequiredMessage is the full client-facing text of the pow-required refusal on the
// gRPC surface. The word "outdated" is deliberate and load-bearing: every EXISTING
// volunteer CLI classifies this status via its IsVolunteerTooOldError string match and
// prints the actionable "run 'lettuce-volunteer update'" hint instead of a generic
// error. Future clients match PowRequiredMessagePrefix instead and never show this text.
const PowRequiredMessage = PowRequiredMessagePrefix +
	": this volunteer build is outdated — run 'lettuce-volunteer update' to get a build that can solve registration challenges"

// challengeBytes is the size of the random challenge (matches the identity-challenge
// scaffold's 32 bytes).
const challengeBytes = 32

// IssueChallenge creates and stores a proof-of-work challenge bound to publicKey.
// Issuance is deliberately available whether or not enforcement is on, so clients can be
// written probe-free (fetch-solve-retry on the pow-required refusal, or fetch up front).
// The table is bounded by the callers' rate limits, the TTL, and the challenge sweeper.
func IssueChallenge(ctx context.Context, db DBTX, publicKey []byte, difficultyBits int, ttl time.Duration) (*Challenge, error) {
	buf := make([]byte, challengeBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("failed to generate challenge bytes: %w", err)
	}
	c := &Challenge{
		ID:             types.NewID(),
		PublicKey:      publicKey,
		Challenge:      buf,
		DifficultyBits: difficultyBits,
		ExpiresAt:      time.Now().UTC().Add(ttl),
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO registration_challenges (id, public_key, challenge, difficulty, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		c.ID, c.PublicKey, c.Challenge, c.DifficultyBits, c.ExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("failed to store registration challenge: %w", err)
	}
	return c, nil
}

// RedeemChallenge consumes the challenge row (single-use) and verifies the solution, in
// that order and in ONE statement plus one hash: the DELETE ... RETURNING takes the row
// lock, so two racing redemptions of one challenge serialize and exactly one sees the
// row. It MUST run inside the registration transaction — a later refusal (cap) or
// failure rolls the consumption back, so a valid solution is never burned by an
// unrelated refusal; likewise a FAILED verification returns an error, the transaction
// rolls back, and the challenge survives for the client to retry with a correct nonce.
func RedeemChallenge(ctx context.Context, db DBTX, red *PowRedemption) error {
	var challenge []byte
	var difficulty int
	err := db.QueryRow(ctx, `
		DELETE FROM registration_challenges
		WHERE id = $1 AND public_key = $2 AND expires_at > NOW()
		RETURNING challenge, difficulty`,
		red.ChallengeID, red.PublicKey,
	).Scan(&challenge, &difficulty)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPowChallengeInvalid
	}
	if err != nil {
		return fmt.Errorf("failed to redeem registration challenge: %w", err)
	}
	if !VerifySolution(challenge, red.PublicKey, red.Nonce, difficulty) {
		return ErrPowSolutionInvalid
	}
	return nil
}

// VerifySolution reports whether nonce solves the challenge for publicKey at the given
// difficulty. THE solution rule lives in the exported module-root package `pow` (so the
// volunteer CLI — a separate module that cannot import internal/ packages — ships the
// same implementation); this delegate keeps the admission-side call sites and test pins
// on one name:
//
//	digest = SHA-256(challenge || publicKey || nonce as 8 big-endian bytes)
//	valid  = LeadingZeroBits(digest) >= difficultyBits
func VerifySolution(challenge, publicKey []byte, nonce uint64, difficultyBits int) bool {
	return pow.VerifySolution(challenge, publicKey, nonce, difficultyBits)
}

// LeadingZeroBits counts the leading zero bits of digest (delegate — see VerifySolution).
func LeadingZeroBits(digest []byte) int {
	return pow.LeadingZeroBits(digest)
}

// Solve brute-forces a nonce for the challenge (delegate — see VerifySolution; the CLI
// imports the pow package directly, tests here use it with a low difficulty).
func Solve(challenge, publicKey []byte, difficultyBits int) uint64 {
	return pow.Solve(challenge, publicKey, difficultyBits)
}
