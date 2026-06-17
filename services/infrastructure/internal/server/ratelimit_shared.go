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
//
// The limiter runs its INCR + conditional-EXPIRE as a single atomic Lua script
// (fixedWindowScript) via EVAL, so the counter and its TTL can never diverge under
// concurrent callers — see sharedBucket.allow for why setting the TTL exactly once
// per window (rather than on every call) is load-bearing.
type rateLimitCmd interface {
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
}

// sharedBucket is a cross-replica fixed-window limiter (Layer 3, BREAK 3) backed by
// Redis. Each call runs fixedWindowScript: an atomic INCR plus a TTL set EXACTLY
// ONCE per window (and repaired only if the key ever lost its TTL). Because all
// replicas INCR the SAME key, a client gets its intended GLOBAL budget regardless of
// replica count (the per-process tokenBucket gave each client N× its budget across
// N replicas).
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

// fixedWindowScript is the atomic body of the limiter. It INCRements the window
// counter and sets the window TTL EXACTLY ONCE per window — on the first request of
// the window (c == 1), or to repair a key that somehow lost its TTL (PTTL == -1,
// e.g. a crash between a prior INCR and its EXPIRE). It deliberately does NOT
// re-stamp the TTL on every call.
//
// Why "once, not every call" is load-bearing: PEXPIRE resets the key's countdown to
// the full window from NOW. Re-stamping on every call rolls the expiry forward on
// each request, so under sustained traffic (any request arriving < window after the
// previous one) the key NEVER expires, the counter climbs without bound, and the
// client is locked out PERMANENTLY once it crosses the limit — regardless of how far
// below the limit its actual rate is. Setting the TTL once means the window closes a
// fixed `window` after its first request irrespective of later traffic: a true fixed
// window (≤2× only at a window boundary). Running INCR + PTTL + PEXPIRE in one script
// keeps the counter and its TTL atomic across concurrent replicas.
const fixedWindowScript = `
local c = redis.call('INCR', KEYS[1])
if c == 1 or redis.call('PTTL', KEYS[1]) == -1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return c`

// allow runs the atomic fixed-window script and decides admission from the returned
// count. resetAt is the (approximate) end of the current window, used for the
// X-RateLimit-* headers. On a Redis error it fails OPEN (allowed=true) and logs at
// ERROR so an outage degrades to per-process over-admission rather than a fleet-wide
// lockout — the limiter is a DoS backstop, not a security boundary.
func (b *sharedBucket) allow(now time.Time) (bool, int, time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	count, err := b.client.Eval(ctx, fixedWindowScript, []string{b.key}, b.window.Milliseconds()).Int64()
	if err != nil {
		slog.Default().Error("rate-limit store error; admitting request (fail-open)",
			"key", b.clientKey, "error", err)
		return true, b.limit, now.Add(b.window)
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
