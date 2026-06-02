package server

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

const (
	authenticatedRateLimit   = 100 // requests per minute
	unauthenticatedRateLimit = 20  // requests per minute
	bucketCleanupInterval    = 5 * time.Minute
	bucketStaleThreshold     = 10 * time.Minute
)

// tokenBucket implements a simple token bucket rate limiter.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newTokenBucket(maxTokens int) *tokenBucket {
	return &tokenBucket{
		tokens:     float64(maxTokens),
		maxTokens:  float64(maxTokens),
		refillRate: float64(maxTokens) / 60.0, // refill over 1 minute
		lastRefill: time.Now(),
	}
}

// allow checks if a request is allowed and consumes a token if so.
// Returns remaining tokens and the reset time. Thread-safe.
func (b *tokenBucket) allow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(b.maxTokens, b.tokens+elapsed*b.refillRate)
	b.lastRefill = now

	remaining = int(b.tokens)
	// Reset time: when bucket would be full again from current level.
	secondsToFull := (b.maxTokens - b.tokens) / b.refillRate
	resetAt = now.Add(time.Duration(secondsToFull) * time.Second)

	if b.tokens < 1 {
		return false, 0, resetAt
	}

	b.tokens--
	remaining = int(b.tokens)
	return true, remaining, resetAt
}

// rateLimitStore is an in-memory store of token buckets keyed by client identity.
type rateLimitStore struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

func newRateLimitStore() *rateLimitStore {
	return &rateLimitStore{
		buckets: make(map[string]*tokenBucket),
	}
}

func (s *rateLimitStore) getBucket(key string, maxTokens int) *tokenBucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[key]
	if !ok {
		b = newTokenBucket(maxTokens)
		s.buckets[key] = b
	}
	return b
}

// cleanup removes stale buckets that haven't been used recently.
func (s *rateLimitStore) cleanup(threshold time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-threshold)
	for key, b := range s.buckets {
		if b.lastRefill.Before(cutoff) {
			delete(s.buckets, key)
		}
	}
}

// startCleanup runs periodic cleanup of stale buckets.
func (s *rateLimitStore) startCleanup(interval, threshold time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanup(threshold)
		case <-stop:
			return
		}
	}
}

// rateLimitMiddleware applies per-client rate limiting.
// Authenticated users get 100 req/min, unauthenticated get 20 req/min.
//
// trustedProxies is the set of reverse-proxy networks whose forwarding headers
// (X-Forwarded-For / X-Real-IP) may be trusted for client-IP extraction. When
// empty, forwarding headers are ignored and the direct peer (RemoteAddr) is
// used — this is the secure default that prevents header-spoofed bucket evasion.
func rateLimitMiddleware(next http.Handler, trustedProxies []*net.IPNet) (http.Handler, func()) {
	store := newRateLimitStore()
	stop := make(chan struct{})
	go store.startCleanup(bucketCleanupInterval, bucketStaleThreshold, stop)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		var key string
		var limit int

		if user != nil {
			// Authenticated: rate limit by user ID.
			key = "user:" + user.ID.String()
			limit = authenticatedRateLimit
		} else {
			// Unauthenticated: rate limit by trust-aware client IP.
			ip := clientIPFromRequest(r, trustedProxies)
			key = "ip:" + ip
			limit = unauthenticatedRateLimit
		}

		bucket := store.getBucket(key, limit)

		now := time.Now()
		allowed, remaining, resetAt := bucket.allow(now)

		// Set rate limit headers on all responses.
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

		if !allowed {
			retryAfter := int(math.Ceil(resetAt.Sub(now).Seconds()))
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			apierror.WriteError(w, apierror.RateLimited(retryAfter))
			return
		}

		next.ServeHTTP(w, r)
	})

	cleanup := func() { close(stop) }
	return handler, cleanup
}

// clientIP extracts the client IP from the request WITHOUT trusting any
// forwarding headers. It is equivalent to clientIPFromRequest(r, nil) and always
// returns the direct peer IP (RemoteAddr). Use clientIPFromRequest when a set of
// trusted reverse-proxy networks is configured.
func clientIP(r *http.Request) string {
	return clientIPFromRequest(r, nil)
}

// clientIPFromRequest performs trust-aware client-IP extraction.
//
// Algorithm:
//  1. Parse RemoteAddr → the direct peer IP.
//  2. If the direct peer is NOT within trustedProxies, return the direct peer
//     IP and IGNORE X-Forwarded-For / X-Real-IP entirely (they are attacker-
//     controllable and untrusted). This is also the path taken when
//     trustedProxies is empty (the secure default).
//  3. If the direct peer IS trusted, parse X-Forwarded-For and walk it
//     RIGHT-to-LEFT, skipping entries that are themselves trusted proxies;
//     return the first (right-most) non-trusted, valid IP — the real client.
//     If no such entry exists, fall back to X-Real-IP (only because the peer is
//     trusted); failing that, the direct peer IP.
func clientIPFromRequest(r *http.Request, trustedProxies []*net.IPNet) string {
	return clientIPFromForwarded(
		remoteIP(r.RemoteAddr),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-IP"),
		trustedProxies,
	)
}

// clientIPFromForwarded is the transport-neutral core of trust-aware client-IP
// extraction shared by the HTTP limiter (clientIPFromRequest) and the gRPC
// limiter (grpcClientIP). It takes the already-extracted direct-peer IP plus the
// raw X-Forwarded-For and X-Real-IP header/metadata values and applies the same
// algorithm documented on clientIPFromRequest:
//
//  1. If peerIP is NOT within trustedProxies, return peerIP and IGNORE xff/xRealIP
//     (the secure default; also the path when trustedProxies is empty).
//  2. Otherwise walk xff RIGHT-to-LEFT, skipping entries that are themselves
//     trusted proxies, and return the first non-trusted, valid IP (the real
//     client). Failing that, fall back to xRealIP (trusted because the peer is),
//     then the direct peer IP.
//
// xff may contain multiple comma-separated entries; xRealIP is a single IP.
func clientIPFromForwarded(peerIP, xff, xRealIP string, trustedProxies []*net.IPNet) string {
	// If we cannot trust the peer (untrusted peer or no trusted proxies at all),
	// never honor forwarding headers — return the direct peer.
	if !ipInNets(net.ParseIP(peerIP), trustedProxies) {
		return peerIP
	}

	// Peer is a trusted proxy: walk X-Forwarded-For right-to-left for the
	// first non-trusted, valid IP (the real client).
	if xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			ip := net.ParseIP(candidate)
			if ip == nil {
				continue
			}
			if ipInNets(ip, trustedProxies) {
				// Another hop in our trusted proxy chain; keep walking left.
				continue
			}
			return candidate
		}
	}

	// No usable XFF entry. Fall back to X-Real-IP (trusted because the peer is).
	if xri := strings.TrimSpace(xRealIP); xri != "" {
		if net.ParseIP(xri) != nil {
			return xri
		}
	}

	// Last resort: the direct peer IP.
	return peerIP
}

// remoteIP extracts the host portion of a "host:port" RemoteAddr, falling back
// to the raw value when it has no port.
func remoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// ipInNets reports whether ip (parsed from a string) falls within any of nets.
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil || len(nets) == 0 {
		return false
	}
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
