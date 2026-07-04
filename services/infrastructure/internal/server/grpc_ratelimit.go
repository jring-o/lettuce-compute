package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// shedLogSampleN controls load-shed log sampling (H-5): the FIRST shed and every Nth
// shed thereafter are logged at Warn (first-then-every-N), so a sustained shed storm
// stays visible without flooding the log. Shared by the rate-limit interceptors and
// the dispatch-cache shed sites in volunteer-service.go.
const shedLogSampleN = 100

// grpcRateLimit is the pre-auth per-IP request budget (requests per minute). It
// is the global/unauthenticated ceiling and is the ONLY limiter that sheds
// Ed25519-verify cost, because it runs before the auth interceptor. It is a var
// (not const) solely so integration tests, which drive hundreds of RPCs from a
// single loopback IP in seconds, can raise it via the integration-only test seam
// (see grpc_auth_testsupport.go). Production never modifies it.
var grpcRateLimit = 60 // requests per minute per IP

// grpcPerPubkeyRateLimit is the post-auth per-authenticated-volunteer request
// budget (requests per minute). It runs AFTER the auth interceptor, keyed on the
// cryptographically verified Ed25519 public key, so a single misbehaving
// volunteer that floods from many source IPs is still bucketed as one client.
// It is a var for the same integration-test-seam reason as grpcRateLimit.
//
// Observability note: because this limiter sits post-auth (the pubkey is only
// trustworthy after signature verification), a rejected call has ALREADY paid one
// Ed25519 verify + replay-cache insert. The per-pubkey bucket therefore sheds
// only DB/handler cost, not crypto cost; the pre-auth per-IP limiter remains the
// only thing that sheds crypto cost.
var grpcPerPubkeyRateLimit = 120 // requests per minute per authenticated volunteer

// SetGRPCRateLimits overrides the per-IP and per-pubkey gRPC request budgets
// (requests per minute) when the supplied values are > 0. It exists so an
// operator can raise the limits for legitimate deployments where a large fleet
// shares one source IP (e.g. volunteers behind a single NAT, or a loopback load
// test) without rebuilding. A non-positive value leaves the corresponding limit
// at its default. Must be called during startup, before the gRPC server begins
// serving, since the limiter reads these vars per call without synchronization.
func SetGRPCRateLimits(perIPPerMin, perPubkeyPerMin int) {
	if perIPPerMin > 0 {
		grpcRateLimit = perIPPerMin
	}
	if perPubkeyPerMin > 0 {
		grpcPerPubkeyRateLimit = perPubkeyPerMin
	}
}

// rateLimitExemptMethods are the in-flight work-lifecycle RPCs a volunteer can
// only call for an assignment it ALREADY holds: StartWork (run-start on a unit it
// reserved), SubmitResult / AbandonWorkUnit (on a unit it is running), and the
// checkpoint pair. They are EXEMPT from both the per-IP and per-pubkey
// request-rate limiters (keyed by trailing method name, see isRateLimitExempt).
//
// Why exempt: their rate is already bounded by max_inflight_per_volunteer and the
// volunteer's genuine completion speed, and the Layer-2 dispatch-cache admission
// semaphore is the correct backpressure for the DB load they incur (it sheds them
// with ResourceExhausted ONLY under real DB-pool pressure). Shedding them via the
// request-rate limiter is never desirable: it drops prepared work at run-start,
// strands a completed result (credit loss on the redundancy-1 reassignment race),
// or leaks a stale reservation (a shed AbandonWorkUnit). Sub-second-unit leafs
// (beyblade) make a single honest volunteer emit 200+ of these per minute once
// batch sizes reached 64 — far past the 60/min per-IP ceiling — so the limiter was
// shedding volunteers trying to finish their own work (TODO #32).
//
// Only the new-work/discovery surface (RegisterVolunteer, RequestWorkUnit) and the
// public methods remain rate-limited. Residual: the per-IP limiter no longer caps
// Ed25519-verify cost for these methods, but a forged-signature flood is still
// rejected at the auth interceptor (one verify, no DB/handler work) and the
// expensive unauthenticated surfaces stay per-IP capped, so the crypto-DoS
// exposure increase is minor.
var rateLimitExemptMethods = map[string]struct{}{
	"StartWork":       {},
	"SubmitResult":    {},
	"AbandonWorkUnit": {},
	"SaveCheckpoint":  {},
	"GetCheckpoint":   {},
}

// isRateLimitExempt reports whether a gRPC full method (e.g.
// "/lettuce.v1.VolunteerService/StartWork") is an in-flight work-lifecycle RPC
// exempt from request-rate limiting. It matches on the trailing method name so it
// is independent of the proto package path.
func isRateLimitExempt(fullMethod string) bool {
	name := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		name = fullMethod[i+1:]
	}
	_, ok := rateLimitExemptMethods[name]
	return ok
}

// grpcRateLimitInterceptor applies pre-auth per-IP rate limiting to gRPC calls.
// Uses the same rateLimitStore/tokenBucket pattern as the HTTP rate limiter.
//
// trustedProxies is the set of reverse-proxy networks whose forwarding metadata
// (x-forwarded-for / x-real-ip) may be trusted for client-IP extraction. When
// empty, forwarding metadata is ignored and the direct gRPC peer IP is used —
// the secure default that prevents metadata-spoofed bucket evasion. This mirrors
// the HTTP limiter's trust-aware extraction (clientIPFromForwarded).
//
// In-flight work-lifecycle RPCs are skipped (see rateLimitExemptMethods): a
// volunteer must never be shed while finishing work it already holds.
func grpcRateLimitInterceptor(store *rateLimitStore, trustedProxies []*net.IPNet, logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	var shedCount uint64
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isRateLimitExempt(info.FullMethod) {
			return handler(ctx, req)
		}
		ip := grpcClientIP(ctx, trustedProxies)
		// Stash the trust-aware client IP for handlers that need it (the registration
		// admission gates). Piggybacked here because this interceptor already computes
		// it for the bucket key; exempt (in-flight work-lifecycle) methods skip both
		// the limiter and the stash, and no handler on those paths reads it.
		ctx = context.WithValue(ctx, grpcClientIPCtxKey{}, ip)
		key := "grpc:" + ip

		bucket := store.getBucket(key, grpcRateLimit)
		allowed, _, _ := bucket.allow(time.Now())

		if !allowed {
			// H-5: load shedding was previously emitted at no level. Sampled Warn so a
			// per-IP flood is visible without one line per dropped request.
			if n := atomic.AddUint64(&shedCount, 1); n%shedLogSampleN == 1 {
				logger.Warn("shedding request: rate limited",
					"method", info.FullMethod, "ip", ip, "shed_count", n)
			}
			return nil, status.Errorf(codes.ResourceExhausted, "rate limit exceeded")
		}

		return handler(ctx, req)
	}
}

// grpcPerPubkeyRateLimitInterceptor applies post-auth per-volunteer rate limiting,
// keyed on the verified Ed25519 public key the auth interceptor placed in the
// context. It MUST be chained AFTER the auth interceptor. Public discovery methods
// carry no pubkey and are never throttled here (the per-IP ceiling still applies).
// In-flight work-lifecycle RPCs are also skipped (see rateLimitExemptMethods) so a
// volunteer is never shed while finishing work it already holds.
//
// No trailer/server-directed delay is stamped: ResourceExhausted is a pure
// load-shedding signal the caller treats as a fixed local backoff (the single
// server-directed-delay mechanism is the RequestWorkUnit response body field).
func grpcPerPubkeyRateLimitInterceptor(store *rateLimitStore, logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	var shedCount uint64
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isRateLimitExempt(info.FullMethod) {
			return handler(ctx, req)
		}
		pk, ok := GRPCAuthPublicKeyFromContext(ctx)
		if !ok {
			// Public methods carry no verified pubkey: no per-volunteer bucket.
			return handler(ctx, req)
		}
		key := "pubkey:" + hex.EncodeToString(pk)

		bucket := store.getBucket(key, grpcPerPubkeyRateLimit)
		allowed, _, _ := bucket.allow(time.Now())

		if !allowed {
			// H-5: sampled Warn so a single misbehaving volunteer flooding past its
			// per-pubkey budget is visible. SHORT pubkey prefix (8 hex chars), not the key.
			if n := atomic.AddUint64(&shedCount, 1); n%shedLogSampleN == 1 {
				logger.Warn("shedding request: rate limited",
					"method", info.FullMethod, "pubkey_prefix", hex.EncodeToString(pk[:4]), "shed_count", n)
			}
			return nil, status.Errorf(codes.ResourceExhausted, "per-volunteer rate limit exceeded")
		}

		return handler(ctx, req)
	}
}

// grpcClientIP extracts the trust-aware client IP for a gRPC call. The direct
// peer IP comes from peer.FromContext; x-forwarded-for / x-real-ip are read from
// incoming metadata and honored ONLY when the direct peer is within
// trustedProxies (see clientIPFromForwarded). Multiple x-forwarded-for metadata
// values are comma-joined into a single chain before the right-to-left walk.
func grpcClientIP(ctx context.Context, trustedProxies []*net.IPNet) string {
	peerIP := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr := p.Addr.String()
		if host, _, err := net.SplitHostPort(addr); err == nil {
			peerIP = host
		} else {
			peerIP = strings.TrimSpace(addr)
		}
	}

	var xff, xRealIP string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		// gRPC metadata may carry repeated values for a key; join the
		// x-forwarded-for values into one comma-separated chain so the
		// right-to-left walk sees the full hop list.
		if vals := md.Get("x-forwarded-for"); len(vals) > 0 {
			xff = strings.Join(vals, ",")
		}
		if vals := md.Get("x-real-ip"); len(vals) > 0 {
			xRealIP = vals[0]
		}
	}

	return clientIPFromForwarded(peerIP, xff, xRealIP, trustedProxies)
}

// grpcClientIPCtxKey keys the trust-aware client IP the pre-auth rate-limit interceptor
// stashes in the request context.
type grpcClientIPCtxKey struct{}

// GRPCClientIPFromContext returns the trust-aware client IP the rate-limit interceptor
// computed for this request. Present for every rate-limited (non-exempt) method — which
// includes RegisterVolunteer — and absent on the exempt in-flight work-lifecycle RPCs
// and in bare unit-test contexts. Note the underlying helper yields the literal
// "unknown" when the peer address is missing; consumers that need a REAL address (the
// registration admission gates) must treat unparseable values as absent and fail
// closed.
func GRPCClientIPFromContext(ctx context.Context) (string, bool) {
	ip, ok := ctx.Value(grpcClientIPCtxKey{}).(string)
	return ip, ok
}
