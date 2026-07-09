package client

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"time"

	"github.com/lettuce-compute/infrastructure/pow"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
)

// BuildRegistrationRequest assembles a RegisterVolunteerRequest from identity,
// hardware capabilities, and config.
//
// availableRuntimes, when non-empty, overrides cfg.AvailableRuntimes for what we
// advertise to the head. Callers pass the runtimes the volunteer can ACTUALLY
// run (e.g. derived from the live runtime registry) so a box that lists
// CONTAINER in config but has no working Docker/Podman doesn't get assigned
// container work it can only abandon. When empty, falls back to config.
//
// hostID is the SERVER-ISSUED id this machine previously received from THIS head
// (echoed so the head refreshes the same hosts row), or empty to ask the head to mint
// one. Host identity is head-minted only (BG-25); clients never generate it.
func BuildRegistrationRequest(pub ed25519.PublicKey, hostID string, hw *lettucev1.HardwareCapabilities, cfg *config.Config, availableRuntimes ...string) *lettucev1.RegisterVolunteerRequest {
	runtimes := availableRuntimes
	if len(runtimes) == 0 {
		runtimes = cfg.AvailableRuntimes
	}
	hostname, _ := os.Hostname()
	return &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pub,
		DisplayName:       hostname,
		Hardware:          hw,
		AvailableRuntimes: runtimes,
		SchedulingMode:    cfg.Scheduling.Mode,
		// Server-issued per-machine host id (BG-25): the previously issued id for this
		// head echoed back, or empty to request a mint. The head keys per-machine
		// metering on the id it returns; empty => per-account fallback.
		HostId: hostID,
	}
}

// Register performs the full registration flow against one head: detect hardware,
// echo the stored per-head host id (empty to mint), call RegisterVolunteer (riding out
// rate limits and solving a registration proof-of-work challenge if the head demands
// one), then persist the head-issued host id and volunteer id.
//
// store/headKey identify where this head's issued host id is persisted (headKey is the
// head's gRPC address). On return the stored id is EXACTLY what the head sent: a fresh
// or confirmed id, or empty (the echoed id was unknown/revoked, or the account is at
// its host cap) — in which case the stored id is discarded and the machine runs
// host-less until a later register mints one. store may be nil (the flow then runs
// host-less and persists nothing).
//
// availableRuntimes (optional) advertises the runtimes actually available rather
// than cfg.AvailableRuntimes — see BuildRegistrationRequest.
//
// Returns the account's volunteer id, whether this was a new registration, and the
// head-issued host id (possibly empty).
func Register(ctx context.Context, client *Client, pub ed25519.PublicKey, store *identity.HostIDStore, headKey string, cfg *config.Config, configPath string, availableRuntimes ...string) (string, bool, string, error) {
	hw := DetectHardware(cfg)

	// Echo the stored per-head id (empty on first contact => the head mints one under
	// the per-account cap). A read error is non-fatal: fall back to empty and let the
	// head mint a fresh id.
	storedHostID := ""
	if store != nil {
		if id, err := store.Get(headKey); err != nil {
			client.logger.Warn("register: could not read stored host id; requesting a fresh one", "head", headKey, "error", err)
		} else {
			storedHostID = id
		}
	}

	req := BuildRegistrationRequest(pub, storedHostID, hw, cfg, availableRuntimes...)
	resp, err := registerWithPow(ctx, client, pub, req)
	if err != nil {
		return "", false, "", fmt.Errorf("registering volunteer: %w", err)
	}

	// Adopt EXACTLY the id the head returned. Empty => discard any stored id for this
	// head and run host-less; non-empty => persist it for the next contact.
	if store != nil {
		var perr error
		if resp.HostId == "" {
			perr = store.Delete(headKey)
		} else {
			perr = store.Set(headKey, resp.HostId)
		}
		if perr != nil {
			client.logger.Warn("register: could not persist head-issued host id", "head", headKey, "error", perr)
		}
	}

	cfg.VolunteerID = resp.VolunteerId
	if err := cfg.Save(configPath); err != nil {
		return resp.VolunteerId, resp.Registered, resp.HostId, fmt.Errorf("saving config after registration: %w", err)
	}

	return resp.VolunteerId, resp.Registered, resp.HostId, nil
}

// registerWithPow calls RegisterVolunteer with reactive registration proof-of-work.
// It registers as-is first; only if the head refuses with the pow-required
// precondition (a brand-new key on a head that enforces PoW) does it fetch a
// challenge, solve it, and retry — once, then once more with a FRESH challenge if the
// first solution is rejected, then surfaces the error. Every RegisterVolunteer call
// rides out rate limiting. Re-registration of an EXISTING key (the common case, and
// every work-path self-heal) never trips PoW, so this collapses to a single call there.
func registerWithPow(ctx context.Context, c *Client, pub ed25519.PublicKey, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	resp, err := callRegister(ctx, c, req)
	if err == nil || !IsPowRequiredError(err) {
		return resp, err
	}

	// pow-required: solve a fresh challenge and retry once.
	resp, err = solveAndRegister(ctx, c, pub, req)
	if err == nil || !IsPowRejectedError(err) {
		return resp, err
	}

	// The first solution was rejected (stale or foreign challenge, or a nonce that
	// missed the target): fetch a FRESH challenge, re-solve, retry once more, then
	// surface whatever the head returns.
	return solveAndRegister(ctx, c, pub, req)
}

// callRegister issues one RegisterVolunteer, riding out the head's rate limiter.
func callRegister(ctx context.Context, c *Client, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	var resp *lettucev1.RegisterVolunteerResponse
	err := retryRPCOnRateLimit(ctx, c.logger, "register volunteer", func(ctx context.Context) error {
		var rpcErr error
		resp, rpcErr = c.RegisterVolunteer(ctx, req)
		return rpcErr
	})
	return resp, err
}

// solveAndRegister fetches a registration proof-of-work challenge, solves it, stamps
// the solution onto req, and re-registers. Called on the pow-required refusal and,
// with a fresh challenge, on a pow-rejected retry.
func solveAndRegister(ctx context.Context, c *Client, pub ed25519.PublicKey, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	challengeID, nonce, err := solveRegistrationChallenge(ctx, c, pub)
	if err != nil {
		return nil, err
	}
	req.PowChallengeId = challengeID
	req.PowNonce = nonce
	return callRegister(ctx, c, req)
}

// solveRegistrationChallenge fetches a challenge and returns its id and a solving
// nonce. The solution rule is the shared, cross-module pow package (imported from the
// head's module so head and CLI provably carry the identical rule — pinned by the
// golden-vector import test): SHA-256(challenge || publicKey || nonce) must have at
// least difficulty_bits leading zero bits. The public key bound into the preimage is
// this client's key, which the head derives from the verified request signature.
func solveRegistrationChallenge(ctx context.Context, c *Client, pub ed25519.PublicKey) (string, uint64, error) {
	ch, err := c.GetRegistrationChallenge(ctx, &lettucev1.GetRegistrationChallengeRequest{})
	if err != nil {
		return "", 0, fmt.Errorf("fetching registration challenge: %w", err)
	}
	nonce := pow.Solve(ch.Challenge, pub, int(ch.DifficultyBits))
	// 2^difficulty SHA-256 attempts is sub-second natively, so a solve outliving the
	// challenge TTL is effectively unreachable — but if it somehow did, submitting the
	// stale solution is guaranteed to be rejected, so fetch a fresh challenge and solve
	// that instead.
	if ch.ExpiresAtUnix > 0 && time.Now().Unix() >= ch.ExpiresAtUnix {
		ch, err = c.GetRegistrationChallenge(ctx, &lettucev1.GetRegistrationChallengeRequest{})
		if err != nil {
			return "", 0, fmt.Errorf("fetching fresh registration challenge after TTL lapse: %w", err)
		}
		nonce = pow.Solve(ch.Challenge, pub, int(ch.DifficultyBits))
	}
	return ch.ChallengeId, nonce, nil
}
