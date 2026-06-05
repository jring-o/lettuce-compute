package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// grpcMaxMsgSize is the absolute transport-level ceiling for gRPC message size
// (send and receive). It is set above the largest legitimate message — a 100MB
// checkpoint (FaultToleranceConfig.MaxCheckpointSizeBytes default) or a 100MB
// inline result output (DataConfig.MaxOutputSizeBytes default) — plus headroom
// for proto framing and metadata, so legitimate large messages are not broken
// while grossly oversized messages are rejected by the transport.
const grpcMaxMsgSize = 128 * 1024 * 1024 // 128 MB

// NewGRPCServer creates a configured gRPC server with interceptors.
//
// Per-client rate limiting: two token-bucket limiters bracket the Ed25519 auth
// interceptor.
//
//   - The PRE-auth per-IP limiter (grpcRateLimitInterceptor) is the global /
//     unauthenticated ceiling. It runs before auth so unauthenticated abuse —
//     and the Ed25519-verify cost it would otherwise incur — is shed. It is now
//     trust-aware: with trustedProxies configured (mirroring the HTTP limiter and
//     LETTUCE_TRUSTED_PROXIES), volunteers behind a reverse proxy (e.g. Caddy)
//     are bucketed per real client IP from x-forwarded-for / x-real-ip instead of
//     sharing one bucket keyed on the proxy address. With no trusted proxies, the
//     direct gRPC peer IP is used (the secure default).
//   - The POST-auth per-pubkey limiter (grpcPerPubkeyRateLimitInterceptor) is the
//     per-authenticated-volunteer budget, keyed on the verified Ed25519 key. It
//     MUST be last in the chain (the pubkey is only trustworthy after auth) and
//     sheds DB/handler cost only, not crypto cost.
//
// logging/recovery remain outermost so every request is still logged and panics
// recovered even when rate-limiting or authentication rejects the call.
//
// replay is the OPTIONAL cross-replica anti-replay store (Layer 3). When supplied
// (and non-nil) it is shared with the auth interceptor so a signature accepted by
// one replica is rejected by another. When omitted (the variadic is empty) or nil,
// a fresh in-process in-mem store is built — single-replica behavior, and what
// every existing call site that passes no store gets. Pass at most one store; any
// extra are ignored.
func NewGRPCServer(tlsCfg *tls.Config, logger *slog.Logger, trustedProxies []*net.IPNet, replay ...replayStore) (*grpc.Server, func()) {
	// Pre-auth per-IP rate limiter. Uses the same rateLimitStore/tokenBucket
	// pattern as the HTTP limiter, with its own store and cleanup goroutine.
	ipStore := newRateLimitStore()
	ipStop := make(chan struct{})
	go ipStore.startCleanup(bucketCleanupInterval, bucketStaleThreshold, ipStop)

	// Post-auth per-pubkey rate limiter. A SECOND store so pubkey buckets are
	// reaped independently; worst-case live buckets ≈ active fleet size × one
	// small struct, bounded by the 10-min stale-bucket reaper.
	pubkeyStore := newRateLimitStore()
	pubkeyStop := make(chan struct{})
	go pubkeyStore.startCleanup(bucketCleanupInterval, bucketStaleThreshold, pubkeyStop)

	// Ed25519 request authentication. The interceptor verifies a per-request
	// signature carried in gRPC metadata and binds the proven public key into the
	// context. Ordered after logging/recovery so requests are still logged and
	// panics recovered even when authentication fails.
	//
	// The replay store is shared with the auth interceptor (Layer 3): a supplied,
	// non-nil store (tests) makes the anti-replay dedup GLOBAL across replicas;
	// otherwise the package-level shared store installed by SetSharedReplayStore
	// (production multi-replica) is used; failing that, a fresh in-process in-mem
	// store gives single-replica behavior.
	var replayStr replayStore
	switch {
	case len(replay) > 0 && replay[0] != nil:
		replayStr = replay[0]
	case grpcSharedReplayStore != nil:
		replayStr = grpcSharedReplayStore
	default:
		replayStr = newInMemReplayStore(ed25519TimestampSkew)
	}
	authIntercept, authCleanup := authInterceptor(replayStr)

	opts := []grpc.ServerOption{
		// M3: bound memory from oversized messages. The default gRPC receive cap is
		// 4MB, which is too small for legitimate large messages (SaveCheckpoint
		// allows checkpoints up to MaxCheckpointSizeBytes, default 100MB per the
		// proto; SubmitResult inline output is capped per-leaf at
		// MaxOutputSizeBytes, default 100MB). We set the absolute ceiling to 128MB
		// (= 100MB largest legitimate payload + headroom for proto framing and
		// metadata) so legitimate 100MB checkpoints/outputs are never broken, while
		// still rejecting grossly oversized messages before they are fully buffered.
		// Per-leaf output limits are still enforced in SubmitResult; this is the
		// transport-level backstop. MaxSendMsgSize is raised to match so the server
		// can return large responses (e.g. checkpoint restore).
		grpc.MaxRecvMsgSize(grpcMaxMsgSize),
		grpc.MaxSendMsgSize(grpcMaxMsgSize),
		grpc.ChainUnaryInterceptor(
			loggingInterceptor(logger),
			recoveryInterceptor(logger),
			grpcRateLimitInterceptor(ipStore, trustedProxies),
			authIntercept,
			grpcPerPubkeyRateLimitInterceptor(pubkeyStore),
		),
	}

	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	cleanup := func() {
		close(ipStop)
		close(pubkeyStop)
		authCleanup()
	}
	return grpc.NewServer(opts...), cleanup
}

// loggingInterceptor logs gRPC method, duration, and status code.
// Extracts or generates a request ID (from "request-id" metadata) and stores
// it in context, mirroring the HTTP RequestIDMiddleware pattern.
func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		requestID := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("request-id"); len(vals) > 0 {
				requestID = vals[0]
			}
		}
		if requestID == "" {
			requestID = uuid.New().String()
		}
		ctx = logging.WithRequestID(ctx, requestID)

		start := time.Now()
		resp, err := handler(ctx, req)
		st, _ := status.FromError(err)
		attrs := []any{
			"method", info.FullMethod,
			"code", st.Code().String(),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			attrs = append(attrs, "peer_addr", p.Addr.String())
		}
		logging.LoggerFromContext(ctx, logger).Info("grpc request", attrs...)
		return resp, err
	}
}

// recoveryInterceptor catches panics in gRPC handlers.
func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("grpc panic recovered",
					"error", fmt.Sprintf("%v", rec),
					"method", info.FullMethod,
					"stack", string(debug.Stack()),
				)
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}
