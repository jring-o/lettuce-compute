package server

// gRPC pre-decode admission and transport hardening (BG-18).
//
// Unary interceptors run AFTER a message is fully read and decoded, so before
// this file every defense (rate limit, auth) was post-decode: an
// unauthenticated client could stream 128 MB SubmitResult bodies the server
// buffered, unmarshaled, deterministically re-marshaled, and SHA-256-hashed
// before anything rejected them — and the big-payload methods were exactly the
// ones exempt from the pre-auth request limiter (rateLimitExemptMethods). The
// gRPC auth signature covers the request body hash, so full verification
// cannot run pre-decode; what CAN run pre-decode, via grpc.InTapHandle (which
// fires on stream creation, before the body is read), is:
//
//  1. a per-IP stream budget over ALL methods — the flood backstop, deliberately
//     far above any honest volunteer's cadence (it is not a request budget; the
//     per-IP/per-pubkey request limiters keep that role), and
//  2. an auth-metadata SHAPE screen for non-public methods — a stream whose
//     headers do not even carry a well-formed pubkey/signature/timestamp/nonce
//     is refused before the server reads its body. No crypto runs here; a
//     forger who fakes the shape still pays the per-IP budget and still fails
//     Ed25519 verification after decode.
//
// Alongside the tap handle, the server sets a per-connection concurrent-stream
// cap and keepalive policy (both previously unset — unlimited streams, ping
// floods unpunished, dead connections never reaped), and a per-method message
// size gate: grpc-go has no per-method transport-level receive cap, so the gate
// is post-decode by necessity, but it runs BEFORE rate-limit/auth so an
// oversized body on a small method is dropped before the auth layer's
// full-body re-marshal + hash.

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
	"google.golang.org/protobuf/proto"
)

const (
	// grpcMaxConcurrentStreams caps concurrently open streams per connection at
	// the transport (excess streams are refused with REFUSED_STREAM before a
	// handler goroutine is spawned). grpc-go's default is effectively unlimited;
	// 100 is the conventional HTTP/2 value and far above the volunteer client's
	// unary call concurrency.
	grpcMaxConcurrentStreams = 100

	// grpcKeepaliveMinPingInterval is the minimum client ping interval the
	// server tolerates (keepalive.EnforcementPolicy.MinTime); clients pinging
	// faster are disconnected (GOAWAY ENHANCE_YOUR_CALM). PermitWithoutStream
	// stays false: a connection with no active stream has no business pinging.
	grpcKeepaliveMinPingInterval = time.Minute

	// grpcKeepaliveMaxConnectionIdle reaps connections with no active stream —
	// volunteers poll far more often than this, so only abandoned or leaked
	// connections accumulate against it.
	grpcKeepaliveMaxConnectionIdle = 15 * time.Minute

	// grpcKeepaliveServerPingTime / Timeout detect dead peers: after 2h of
	// silence the server pings, and a peer that doesn't answer within 20s is
	// closed (the grpc-go defaults, set explicitly so the posture is pinned).
	grpcKeepaliveServerPingTime    = 2 * time.Hour
	grpcKeepaliveServerPingTimeout = 20 * time.Second

	// grpcDefaultMethodMaxMsgSize is the per-method receive ceiling for every
	// method NOT in grpcBulkMethodMaxMsgSize. The largest legitimate small-method
	// request (RegisterVolunteer with full hardware inventory) is a few KB; 1 MB
	// leaves generous headroom while keeping a 128 MB body off the auth layer's
	// re-marshal + hash for methods that never legitimately carry one.
	grpcDefaultMethodMaxMsgSize = 1 << 20 // 1 MB
)

// grpcBulkMethodMaxMsgSize lists the methods that legitimately carry payloads
// up to the transport ceiling, keyed by trailing method name (independent of
// the proto package path): SubmitResult (100 MB inline output — both the
// volunteer service and the audit service's re-execution output submit) and
// SaveCheckpoint (100 MB checkpoint).
var grpcBulkMethodMaxMsgSize = map[string]int{
	"SubmitResult":   grpcMaxMsgSize,
	"SaveCheckpoint": grpcMaxMsgSize,
}

// grpcStreamRateLimit is the pre-decode per-IP stream budget (streams per
// minute) enforced in the tap handle over ALL methods, including the methods
// exempt from the request-rate limiters. It is a flood backstop, not a request
// budget: the default sits far above an honest volunteer's worst-case cadence
// (sub-second work units peak around 200+ RPCs/min) while bounding how many
// bodies per minute a single IP can make the server decode. A var for the same
// operator-override reason as grpcRateLimit (NAT'ed fleets share one IP).
var grpcStreamRateLimit = 600

// SetGRPCStreamRateLimit overrides the pre-decode per-IP stream budget
// (streams per minute) when the supplied value is > 0. Must be called during
// startup, before the gRPC server begins serving (read per stream without
// synchronization). Companion to SetGRPCRateLimits.
func SetGRPCStreamRateLimit(perIPPerMin int) {
	if perIPPerMin > 0 {
		grpcStreamRateLimit = perIPPerMin
	}
}

// methodMaxMsgSize resolves the per-method receive ceiling for a gRPC full
// method, matching on the trailing method name.
func methodMaxMsgSize(fullMethod string) int {
	name := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		name = fullMethod[i+1:]
	}
	if limit, ok := grpcBulkMethodMaxMsgSize[name]; ok {
		return limit
	}
	return grpcDefaultMethodMaxMsgSize
}

// grpcTapAdmission builds the tap.ServerInHandle that admits or refuses a
// stream BEFORE its message is read from the wire. It runs on the connection's
// I/O goroutine, so it does only non-blocking work: a token-bucket check and
// header-shape checks. A returned status error is delivered to the client
// verbatim (every message carries the "pre-decode admission:" prefix so a
// refusal here is distinguishable from the post-decode interceptors — the
// BG-18 regression tests key on exactly that).
func grpcTapAdmission(store *rateLimitStore, trustedProxies []*net.IPNet, logger *slog.Logger) tap.ServerInHandle {
	if logger == nil {
		logger = slog.Default()
	}
	var shedCount uint64
	return func(ctx context.Context, info *tap.Info) (context.Context, error) {
		// (1) Per-IP stream budget, ALL methods. Uses the same trust-aware client
		// IP extraction as the request limiter; the "grpctap:" key prefix keeps
		// these buckets distinct from the post-decode per-IP request buckets.
		ip := grpcClientIP(ctx, trustedProxies)
		bucket := store.getBucket("grpctap:"+ip, grpcStreamRateLimit)
		allowed, _, _ := bucket.allow(time.Now())
		if !allowed {
			if n := atomic.AddUint64(&shedCount, 1); n%shedLogSampleN == 1 {
				logger.Warn("refusing stream before decode: per-IP stream budget exceeded",
					"method", info.FullMethodName, "ip", ip, "shed_count", n)
			}
			return nil, status.Error(codes.ResourceExhausted,
				"pre-decode admission: per-IP stream budget exceeded; back off and retry")
		}

		// (2) Auth-metadata shape screen, non-public methods. The metadata is
		// already decoded (headers only — the message body is not yet read).
		if !grpcPublicMethods[info.FullMethodName] {
			if err := screenAuthMetadataShape(info.Header); err != nil {
				if n := atomic.AddUint64(&shedCount, 1); n%shedLogSampleN == 1 {
					logger.Warn("refusing stream before decode: malformed auth metadata",
						"method", info.FullMethodName, "ip", ip, "error", err, "shed_count", n)
				}
				return nil, err
			}
		}
		return ctx, nil
	}
}

// screenAuthMetadataShape refuses a stream whose headers cannot possibly
// authenticate: missing or wrong-size public key / signature, a timestamp
// outside the accepted skew, or a missing/over-long nonce. Shape only — no
// signature verification (the body the signature covers is not yet read) and
// no replay check (the store must never be populated by unverified input).
// The auth interceptor remains the authority after decode.
func screenAuthMetadataShape(md metadata.MD) error {
	if len(md.Get(grpcAuthPubKeyMeta)) == 0 || len(md.Get(grpcAuthSignatureMeta)) == 0 || len(md.Get(grpcAuthTimestampMeta)) == 0 {
		return status.Error(codes.Unauthenticated, "pre-decode admission: missing authentication metadata")
	}
	if pk := md.Get(grpcAuthPubKeyMeta)[0]; len(pk) != ed25519.PublicKeySize {
		return status.Errorf(codes.Unauthenticated, "pre-decode admission: public key must be %d bytes", ed25519.PublicKeySize)
	}
	if sig := md.Get(grpcAuthSignatureMeta)[0]; len(sig) != ed25519.SignatureSize {
		return status.Errorf(codes.Unauthenticated, "pre-decode admission: signature must be %d bytes", ed25519.SignatureSize)
	}
	ts, err := strconv.ParseInt(md.Get(grpcAuthTimestampMeta)[0], 10, 64)
	if err != nil {
		return status.Error(codes.Unauthenticated, "pre-decode admission: invalid timestamp")
	}
	skew := timeNow().Sub(time.Unix(ts, 0))
	if skew < -ed25519TimestampSkew || skew > ed25519TimestampSkew {
		return status.Error(codes.Unauthenticated, "pre-decode admission: timestamp expired or too far in the future")
	}
	nonce := md.Get(grpcAuthNonceMeta)
	if len(nonce) == 0 || nonce[0] == "" {
		return status.Error(codes.Unauthenticated, "pre-decode admission: missing per-request nonce")
	}
	if len(nonce[0]) > grpcAuthMaxNonceLen {
		return status.Error(codes.Unauthenticated, "pre-decode admission: nonce too long")
	}
	return nil
}

// grpcPerMethodSizeGateInterceptor rejects a decoded request larger than its
// method's ceiling. Chained after recovery and BEFORE rate-limit/auth: the
// decode cost is already paid (grpc-go offers no per-method transport cap),
// but the rejection still spares the auth interceptor's deterministic
// re-marshal + SHA-256 of the full body, and keeps oversized garbage away
// from every later layer. The transport-level 128 MB ceiling remains the
// absolute backstop for the bulk methods.
func grpcPerMethodSizeGateInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	var shedCount uint64
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		limit := methodMaxMsgSize(info.FullMethod)
		if msg, ok := req.(proto.Message); ok {
			if size := proto.Size(msg); size > limit {
				if n := atomic.AddUint64(&shedCount, 1); n%shedLogSampleN == 1 {
					logger.Warn("rejecting request over per-method size ceiling",
						"method", info.FullMethod, "size", size, "limit", limit, "shed_count", n)
				}
				return nil, status.Errorf(codes.ResourceExhausted,
					"request size %d bytes exceeds the %d-byte limit for %s", size, limit, info.FullMethod)
			}
		}
		return handler(ctx, req)
	}
}
