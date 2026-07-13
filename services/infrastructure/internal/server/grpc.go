package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/safego"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
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
	safego.Go(context.Background(), logger, "grpc-ip-ratelimit-reaper", func(context.Context) {
		ipStore.startCleanup(bucketCleanupInterval, bucketStaleThreshold, ipStop)
	})

	// Post-auth per-pubkey rate limiter. A SECOND store so pubkey buckets are
	// reaped independently; worst-case live buckets ≈ active fleet size × one
	// small struct, bounded by the 10-min stale-bucket reaper.
	pubkeyStore := newRateLimitStore()
	pubkeyStop := make(chan struct{})
	safego.Go(context.Background(), logger, "grpc-pubkey-ratelimit-reaper", func(context.Context) {
		pubkeyStore.startCleanup(bucketCleanupInterval, bucketStaleThreshold, pubkeyStop)
	})

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
		// transport-level backstop for the bulk methods — everything else gets the
		// tighter per-method gate below (BG-18). MaxSendMsgSize is raised to match
		// so the server can return large responses (e.g. checkpoint restore).
		grpc.MaxRecvMsgSize(grpcMaxMsgSize),
		grpc.MaxSendMsgSize(grpcMaxMsgSize),
		// BG-18 transport hardening: per-connection stream cap and keepalive
		// policy (previously unset — unlimited streams, ping floods unpunished,
		// dead/idle connections never reaped). See grpc_admission.go.
		grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             grpcKeepaliveMinPingInterval,
			PermitWithoutStream: false,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: grpcKeepaliveMaxConnectionIdle,
			Time:              grpcKeepaliveServerPingTime,
			Timeout:           grpcKeepaliveServerPingTimeout,
		}),
		// BG-18 pre-decode admission: the tap handle refuses a stream BEFORE its
		// message is read — per-IP stream budget over all methods (including the
		// request-limiter-exempt in-flight ones) plus an auth-metadata shape
		// screen for non-public methods, so an unauthenticated flood can no
		// longer make the server buffer and decode 128 MB bodies. Shares ipStore
		// (distinct "grpctap:" key prefix) so one reaper bounds both bucket sets.
		grpc.InTapHandle(grpcTapAdmission(ipStore, trustedProxies, logger)),
		grpc.ChainUnaryInterceptor(
			loggingInterceptor(logger),
			recoveryInterceptor(logger),
			// BG-18: per-method size gate — before rate-limit/auth so oversized
			// bodies never reach the auth layer's full-body re-marshal + hash.
			grpcPerMethodSizeGateInterceptor(logger),
			grpcRateLimitInterceptor(ipStore, trustedProxies, logger),
			authIntercept,
			grpcPerPubkeyRateLimitInterceptor(pubkeyStore, logger),
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

// highFrequencyMethods are the read-mostly / hot-poll RPCs whose per-call access log
// is demoted to Debug. RequestWorkUnit alone is 200+/min/volunteer; the status/info
// probes and the checkpoint pair are also high-rate and low-value at Info. The
// state-mutating lifecycle RPCs (RegisterVolunteer, StartWork, SubmitResult,
// AbandonWorkUnit) are deliberately absent so they stay at Info. Keyed by trailing
// method name so it is independent of the proto package path.
var highFrequencyMethods = map[string]struct{}{
	"RequestWorkUnit": {},
	"GetServerStatus": {},
	"GetHeadInfo":     {},
	"SaveCheckpoint":  {},
	"GetCheckpoint":   {},
}

// grpcAccessLogLevel returns the access-log level for a gRPC full method: Debug for
// the high-frequency poll/read methods above, Info for everything else (so the
// per-WU-lifecycle state-mutating RPCs remain visible at Info while the hot poll does
// not flood the log).
func grpcAccessLogLevel(fullMethod string) slog.Level {
	name := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		name = fullMethod[i+1:]
	}
	if _, ok := highFrequencyMethods[name]; ok {
		return slog.LevelDebug
	}
	return slog.LevelInfo
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
		// H-6: hot-poll/read methods (RequestWorkUnit and the status/info/checkpoint
		// reads) log at Debug; state-mutating lifecycle RPCs stay at Info.
		logging.LoggerFromContext(ctx, logger).Log(ctx, grpcAccessLogLevel(info.FullMethod), "grpc request", attrs...)
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
