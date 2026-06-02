package server

import (
	"context"
	"encoding/hex"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

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

// grpcRateLimitInterceptor applies pre-auth per-IP rate limiting to gRPC calls.
// Uses the same rateLimitStore/tokenBucket pattern as the HTTP rate limiter.
//
// trustedProxies is the set of reverse-proxy networks whose forwarding metadata
// (x-forwarded-for / x-real-ip) may be trusted for client-IP extraction. When
// empty, forwarding metadata is ignored and the direct gRPC peer IP is used —
// the secure default that prevents metadata-spoofed bucket evasion. This mirrors
// the HTTP limiter's trust-aware extraction (clientIPFromForwarded).
func grpcRateLimitInterceptor(store *rateLimitStore, trustedProxies []*net.IPNet) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ip := grpcClientIP(ctx, trustedProxies)
		key := "grpc:" + ip

		bucket := store.getBucket(key, grpcRateLimit)
		allowed, _, _ := bucket.allow(time.Now())

		if !allowed {
			return nil, status.Errorf(codes.ResourceExhausted, "rate limit exceeded")
		}

		return handler(ctx, req)
	}
}

// grpcPerPubkeyRateLimitInterceptor applies post-auth per-volunteer rate limiting,
// keyed on the verified Ed25519 public key the auth interceptor placed in the
// context. It MUST be chained AFTER the auth interceptor. Public discovery methods
// carry no pubkey and are never throttled here (the per-IP ceiling still applies).
//
// No trailer/server-directed delay is stamped: ResourceExhausted is a pure
// load-shedding signal the caller treats as a fixed local backoff (the single
// server-directed-delay mechanism is the RequestWorkUnit response body field).
func grpcPerPubkeyRateLimitInterceptor(store *rateLimitStore) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		pk, ok := GRPCAuthPublicKeyFromContext(ctx)
		if !ok {
			// Public methods carry no verified pubkey: no per-volunteer bucket.
			return handler(ctx, req)
		}
		key := "pubkey:" + hex.EncodeToString(pk)

		bucket := store.getBucket(key, grpcPerPubkeyRateLimit)
		allowed, _, _ := bucket.allow(time.Now())

		if !allowed {
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
