package server

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

// replayStore is the cross-replica anti-replay dedup seam (Layer 3). It records a
// signed-request signature and reports whether that EXACT signature was already
// seen within ttl. The dedup key is the SIGNATURE ALONE, GLOBAL across every head
// replica — never (instance_id, signature): a per-replica key would let each
// replica accept the same signature once, defeating cross-replica replay
// rejection. TTL is the clock-skew window (ed25519TimestampSkew); after it the
// signature can never re-verify (the timestamp is outside skew), so an expired
// entry is harmless to forget.
//
// Two implementations:
//
//   - inMemReplayStore wraps the existing per-process replayCache. It is the
//     default when no Redis URL is configured (single-replica deploys + every
//     existing unit/integration test stay green). Sharing ONE *inMemReplayStore
//     pointer between two in-process server instances also proves cross-replica
//     rejection WITHOUT Redis (the unit-level scale-out test).
//   - redisReplayStore uses Redis SET NX PX, the operator-sanctioned shared store.
//     It keeps the per-request replay check OFF Postgres (the dispatch hot path
//     Layer 2 cleared) — a sub-ms SETNX, not a DB write.
//
// SeenWithin returns (alreadySeen, err). A non-nil err is a STORE failure
// (network); callers apply the configured fail-open / fail-closed policy. The
// in-mem store never errors.
type replayStore interface {
	SeenWithin(ctx context.Context, sig []byte, ttl time.Duration) (alreadySeen bool, err error)
}

// inMemReplayStore adapts the existing replayCache to the replayStore interface.
// It is the default store and preserves the exact single-process behavior the
// existing tests assert.
type inMemReplayStore struct {
	cache *replayCache
}

// newInMemReplayStore builds an in-memory replay store with the given TTL.
func newInMemReplayStore(ttl time.Duration) *inMemReplayStore {
	return &inMemReplayStore{cache: newReplayCache(ttl)}
}

// SeenWithin records sig and reports whether it was already present within ttl.
// The ttl argument is ignored in favor of the cache's construction TTL (both are
// ed25519TimestampSkew); it is part of the interface so the Redis impl can set a
// per-call expiry. The in-mem store never returns an error.
func (s *inMemReplayStore) SeenWithin(_ context.Context, sig []byte, _ time.Duration) (bool, error) {
	// checkAndAdd returns true when the signature is NEW, so alreadySeen is its
	// negation.
	isNew := s.cache.checkAndAdd(sig, timeNow())
	return !isNew, nil
}

// replaySetNXCmd is the narrow redis subset the replay store uses. *redis.Client
// (redis.Cmdable) satisfies it; a fake satisfies it in unit tests so the SETNX
// dedup and error paths are testable without a live Redis.
type replaySetNXCmd interface {
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
}

// redisReplayStore records signatures in Redis so the dedup is GLOBAL across all
// head replicas. The key is "lettuce:replay:<hex(sig)>" and the value is set with
// NX (only if absent) and PX = the skew window. alreadySeen is true when the NX
// set did NOT happen (the key already existed = a replay).
type redisReplayStore struct {
	client replaySetNXCmd
}

// newRedisReplayStore builds a Redis-backed replay store from an already-built
// client. The caller owns the client lifecycle.
func newRedisReplayStore(client replaySetNXCmd) *redisReplayStore {
	return &redisReplayStore{client: client}
}

const redisReplayKeyPrefix = "lettuce:replay:"

// redisReplayTimeout bounds ONE replay-store lookup (PB-21). A healthy SETNX is
// sub-millisecond; during a Redis outage the go-redis client otherwise burns its
// own dial/DNS retry budget (~30s observed live: "failed to dial after 5 attempts")
// before erroring — the FULL client RPC deadline — so the configured fail-mode
// policy never actually answered: fail-open "admitted" only after the client had
// already given up (context canceled), and fail-closed's Unavailable never reached
// the client either. Every signed RPC hung for the whole outage in BOTH modes.
// Cutting the store call at 2s makes the policy REAL: fail-open admits (no lost
// compute on SubmitResult, the grpc_auth.go promise) and fail-closed refuses
// promptly, both well inside the client deadline.
const redisReplayTimeout = 2 * time.Second

// SeenWithin performs an atomic SET NX PX, bounded by redisReplayTimeout (PB-21).
// On a store error it returns (false, err) so the caller's fail-open / fail-closed
// policy decides; it never fabricates an alreadySeen=true on error (that would
// silently reject legitimate traffic during an outage).
func (s *redisReplayStore) SeenWithin(ctx context.Context, sig []byte, ttl time.Duration) (bool, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, redisReplayTimeout)
	defer cancel()
	key := redisReplayKeyPrefix + hex.EncodeToString(sig)
	// SET key 1 NX PX <ttl_ms>: ok=true when the key was newly set (first sight),
	// ok=false when it already existed (a replay). A nil/err result is a store
	// failure surfaced to the caller.
	ok, err := s.client.SetNX(boundedCtx, key, 1, ttl).Result()
	if err != nil {
		return false, err
	}
	return !ok, nil
}
