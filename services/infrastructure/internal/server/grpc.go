package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
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
// Rate limiting (H3): a per-IP token-bucket limiter is installed BEFORE the C1
// Ed25519 auth interceptor so that unauthenticated abuse is throttled, while
// logging/recovery remain outermost so every request is still logged and panics
// recovered. The limiter keys on the gRPC peer IP.
//
// Known limitation: behind a reverse proxy that terminates gRPC (e.g. Caddy),
// the gRPC peer IP seen here is the proxy's address, so this limiter becomes an
// effectively GLOBAL ceiling rather than per-client. It still protects
// direct-access deployments and provides a global rate ceiling; for true
// per-client gRPC limiting behind a proxy, configure rate limiting at the proxy.
func NewGRPCServer(tlsCfg *tls.Config, logger *slog.Logger) (*grpc.Server, func()) {
	// Per-IP rate limiter. Uses the same rateLimitStore/tokenBucket pattern as the
	// HTTP limiter, with its own store and cleanup goroutine.
	rlStore := newRateLimitStore()
	rlStop := make(chan struct{})
	go rlStore.startCleanup(bucketCleanupInterval, bucketStaleThreshold, rlStop)

	// Ed25519 request authentication. The interceptor verifies a per-request
	// signature carried in gRPC metadata and binds the proven public key into the
	// context. Ordered after logging/recovery so requests are still logged and
	// panics recovered even when authentication fails.
	authIntercept, authCleanup := authInterceptor()

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
			grpcRateLimitInterceptor(rlStore),
			authIntercept,
		),
	}

	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	cleanup := func() {
		close(rlStop)
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
