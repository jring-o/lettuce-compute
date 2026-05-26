package server

import (
	"context"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// grpcRateLimit is the per-IP request budget (requests per minute). It is a var
// (not const) solely so integration tests, which drive hundreds of RPCs from a
// single loopback IP in seconds, can raise it via the integration-only test seam
// (see grpc_auth_testsupport.go). Production never modifies it.
var grpcRateLimit = 60 // requests per minute per IP

// grpcRateLimitInterceptor applies per-IP rate limiting to gRPC calls.
// Uses the same rateLimitStore/tokenBucket pattern as the HTTP rate limiter.
func grpcRateLimitInterceptor(store *rateLimitStore) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ip := grpcClientIP(ctx)
		key := "grpc:" + ip

		bucket := store.getBucket(key, grpcRateLimit)
		allowed, _, _ := bucket.allow(time.Now())

		if !allowed {
			return nil, status.Errorf(codes.ResourceExhausted, "rate limit exceeded")
		}

		return handler(ctx, req)
	}
}

// grpcClientIP extracts the client IP from gRPC peer info.
func grpcClientIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	addr := p.Addr.String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return host
}
