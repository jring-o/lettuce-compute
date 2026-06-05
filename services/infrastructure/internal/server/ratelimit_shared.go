package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// limiter is the small contract the rate-limit call sites use. Both the in-process
// tokenBucket and the cross-replica sharedBucket satisfy it, so getBucket can hand
// back either without the call sites changing.
type limiter interface {
	// allow reports whether a request is permitted at now, plus the remaining
	// budget and the window/refill reset time (for X-RateLimit-* headers).
	allow(now time.Time) (allowed bool, remaining int, resetAt time.Time)
}

// rateLimitCmd is the narrow subset of redis commands the shared rate limiter uses.
// *redis.Client (redis.Cmdable) satisfies it; a fake satisfies it in unit tests so
// the fixed-window logic and fail-open path are testable without a live Redis.
type rateLimitCmd interface {
	Incr(ctx context.Context, key string) *redis.IntCmd
	PExpire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
}

// sharedBucket is a cross-replica fixed-window limiter (Layer 3, BREAK 3) backed by
// Redis. Each call does an atomic INCR and ALWAYS (re)asserts a PEXPIRE so the
// window key can never be left without a TTL. Because all replicas INCR the SAME
// key, a client gets its intended GLOBAL budget regardless of replica count (the
// per-process tokenBucket gave each client N× its budget across N replicas).
//
// Fairness, not dispatch correctness: a Redis error fails OPEN (admits the request)
// — over-admitting a few requests during an outage beats locking out the fleet; the
// limiter is a DoS backstop, not a security boundary. A fixed window permits up to
// 2× the limit across a window boundary; this is an ACCEPTED property for a DoS
// backstop (a sliding-window upgrade is out of scope).
type sharedBucket struct {
	client    rateLimitCmd
	key       string
	limit     int
	window    time.Duration
	clientKey string // the application-level key ("grpc:"+ip etc.) for the redis key
}

const redisRateLimitKeyPrefix = "lettuce:ratelimit:"

// newSharedBucket builds a shared fixed-window limiter for one client key.
func newSharedBucket(client rateLimitCmd, clientKey string, limit int, window time.Duration) *sharedBucket {
	return &sharedBucket{
		client:    client,
		key:       redisRateLimitKeyPrefix + clientKey,
		limit:     limit,
		window:    window,
		clientKey: clientKey,
	}
}

// allow performs the atomic INCR and then ALWAYS (re)asserts the window TTL.
// resetAt is the end of the current fixed window. On a Redis INCR error it fails
// OPEN (allowed=true) and logs at ERROR so an outage degrades to today's
// per-process over-admission rather than a fleet lockout.
//
// The TTL is (re)asserted on EVERY call, not just the first hit of a window. If
// the very first PEXPIRE failed (e.g. a transient store hiccup) the key would
// otherwise be left with NO expiry: every later call sees count>1, the counter
// climbs past the limit, and the client is blocked PERMANENTLY because the window
// never resets. Re-asserting PEXPIRE each call means a single failed expire is
// self-healing on the next request — the key can never be stranded without a TTL.
// PEXPIRE is cheap and idempotent; re-stamping a sub-window-old key with the full
// window is the accepted fixed-window approximation (already up to 2× at a window
// boundary). A PEXPIRE error is non-fatal: we log and admit per the request's INCR
// result, and the next call retries the expire.
func (b *sharedBucket) allow(now time.Time) (bool, int, time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	count, err := b.client.Incr(ctx, b.key).Result()
	if err != nil {
		slog.Default().Error("rate-limit store error; admitting request (fail-open)",
			"key", b.clientKey, "error", err)
		return true, b.limit, now.Add(b.window)
	}
	// Always (re)assert the TTL so a window key can never be left without one. A
	// failure here is non-fatal — the next call retries it — but we log it.
	if _, eerr := b.client.PExpire(ctx, b.key, b.window).Result(); eerr != nil {
		slog.Default().Error("rate-limit store PEXPIRE error",
			"key", b.clientKey, "error", eerr)
	}

	resetAt := now.Add(b.window)
	remaining := b.limit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	if count > int64(b.limit) {
		return false, 0, resetAt
	}
	return true, remaining, resetAt
}
