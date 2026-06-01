package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// gRPC metadata keys for Ed25519 request authentication. The "-bin" suffix tells
// gRPC the value is raw binary (it base64-encodes/decodes transparently on the
// wire), so we never deal with text encoding for the key and signature ourselves.
const (
	grpcAuthPubKeyMeta    = "x-lettuce-pubkey-bin"
	grpcAuthTimestampMeta = "x-lettuce-timestamp"
	grpcAuthSignatureMeta = "x-lettuce-signature-bin"
	// grpcAuthNonceMeta carries an OPTIONAL fresh per-request nonce. It is a plain
	// (NOT "-bin") key because the value is lowercase-hex ASCII the client folded
	// verbatim into the signed bytes; a "-bin" key would be base64-decoded by gRPC
	// on read and would no longer match. When present, the canonical message is
	// reconstructed WITH the nonce; when absent (legacy/old volunteers), the
	// pre-nonce <ts>:<method>:<hash> form is used, preserving backward compat.
	grpcAuthNonceMeta = "x-lettuce-nonce"

	// grpcAuthMaxNonceLen bounds the nonce length accepted from metadata before it
	// is folded into the signed string. A well-behaved client sends 32 hex chars;
	// this caps the signed-string size against a hostile over-long metadata value.
	grpcAuthMaxNonceLen = 256
)

// grpcReplayDetectionEnabled gates the anti-replay cache. It is true in production
// and only flipped to false by the integration-only test seam (see
// grpc_auth_testsupport.go), which lets e2e tests replay byte-identical signed RPCs.
var grpcReplayDetectionEnabled = true

// grpcAuthPublicKeyContextKey is a typed context key for the verified public key.
type grpcAuthPublicKeyContextKey struct{}

// contextWithGRPCAuthPublicKey stores the cryptographically verified Ed25519 public
// key (proven by signature) in the context for handlers to bind to a volunteer.
func contextWithGRPCAuthPublicKey(ctx context.Context, pubKey ed25519.PublicKey) context.Context {
	return context.WithValue(ctx, grpcAuthPublicKeyContextKey{}, pubKey)
}

// GRPCAuthPublicKeyFromContext extracts the verified Ed25519 public key set by the
// auth interceptor. ok is false when the RPC was not authenticated (public methods).
func GRPCAuthPublicKeyFromContext(ctx context.Context) (ed25519.PublicKey, bool) {
	pk, ok := ctx.Value(grpcAuthPublicKeyContextKey{}).(ed25519.PublicKey)
	return pk, ok
}

// grpcPublicMethods are the discovery RPCs that carry no identity and require no
// authentication. They mirror the unauthenticated REST discovery endpoints.
var grpcPublicMethods = map[string]bool{
	"/lettuce.volunteer.v1.VolunteerService/GetServerStatus": true,
	"/lettuce.volunteer.v1.VolunteerService/GetHeadInfo":     true,
}

// canonicalGRPCAuthMessage builds the message that the client signs and the server
// verifies. It MUST match the client interceptor exactly. There are two forms,
// selected by whether a nonce is present:
//
//	with nonce:    <unix-ts>:<full-method>:<hex(sha256(deterministic-marshal(req)))>:<nonce-hex>
//	without nonce: <unix-ts>:<full-method>:<hex(sha256(deterministic-marshal(req)))>
//
// The empty-nonce branch reproduces the pre-nonce protocol BYTE-FOR-BYTE so that
// old volunteers (which send no x-lettuce-nonce) still verify. New volunteers send
// the nonce both in the signed bytes and in metadata, so the server reconstructs
// the with-nonce form identically. requestBytes uses deterministic protobuf
// marshaling, which is stable across the shared protobuf-go version in this
// workspace, so both sides hash identical bytes.
func canonicalGRPCAuthMessage(unixTs int64, fullMethod string, requestBytes []byte, nonce string) string {
	sum := sha256.Sum256(requestBytes)
	if nonce == "" {
		return fmt.Sprintf("%d:%s:%s", unixTs, fullMethod, hex.EncodeToString(sum[:]))
	}
	return fmt.Sprintf("%d:%s:%s:%s", unixTs, fullMethod, hex.EncodeToString(sum[:]), nonce)
}

// replayCache is a small, bounded, mutex-guarded set of recently-seen signatures
// used to reject replayed requests within the allowed clock-skew window. Entries
// expire after ed25519TimestampSkew; expired entries are evicted on insert.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

func newReplayCache(ttl time.Duration) *replayCache {
	return &replayCache{
		seen: make(map[string]time.Time),
		ttl:  ttl,
	}
}

// checkAndAdd evicts expired entries, then records the signature. It returns false
// if the signature was already present (a replay), true if it is new.
func (c *replayCache) checkAndAdd(sig []byte, now time.Time) bool {
	key := string(sig)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict expired entries (cheap; the cache stays bounded by the skew window).
	for k, t := range c.seen {
		if now.Sub(t) > c.ttl {
			delete(c.seen, k)
		}
	}

	if _, exists := c.seen[key]; exists {
		return false
	}
	c.seen[key] = now
	return true
}

// authInterceptor returns a UnaryServerInterceptor that requires and verifies an
// Ed25519 request signature for every RPC except the public discovery methods. On
// success it stores the verified public key in the context. It also returns a
// cleanup func that stops the replay-cache janitor goroutine.
func authInterceptor() (grpc.UnaryServerInterceptor, func()) {
	cache := newReplayCache(ed25519TimestampSkew)

	// Janitor periodically evicts expired entries so the cache cannot grow without
	// bound during quiet periods between inserts.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(ed25519TimestampSkew)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				now := timeNow()
				cache.mu.Lock()
				for k, t := range cache.seen {
					if now.Sub(t) > cache.ttl {
						delete(cache.seen, k)
					}
				}
				cache.mu.Unlock()
			}
		}
	}()

	cleanup := func() {
		close(stop)
		wg.Wait()
	}

	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if grpcPublicMethods[info.FullMethod] {
			return handler(ctx, req)
		}

		pubKey, err := verifyGRPCAuth(ctx, info.FullMethod, req, cache)
		if err != nil {
			return nil, err
		}
		return handler(contextWithGRPCAuthPublicKey(ctx, pubKey), req)
	}

	return interceptor, cleanup
}

// verifyGRPCAuth parses and verifies the Ed25519 signature carried in gRPC metadata
// for the given request. On success it returns the verified public key.
func verifyGRPCAuth(ctx context.Context, fullMethod string, req any, cache *replayCache) (ed25519.PublicKey, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing request metadata")
	}

	pubVals := md.Get(grpcAuthPubKeyMeta)
	sigVals := md.Get(grpcAuthSignatureMeta)
	tsVals := md.Get(grpcAuthTimestampMeta)
	if len(pubVals) == 0 || len(sigVals) == 0 || len(tsVals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing authentication metadata")
	}

	pubKeyBytes := []byte(pubVals[0])
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return nil, status.Errorf(codes.Unauthenticated, "invalid public key: must be %d bytes, got %d", ed25519.PublicKeySize, len(pubKeyBytes))
	}

	sigBytes := []byte(sigVals[0])
	if len(sigBytes) != ed25519.SignatureSize {
		return nil, status.Errorf(codes.Unauthenticated, "invalid signature: must be %d bytes, got %d", ed25519.SignatureSize, len(sigBytes))
	}

	ts, err := strconv.ParseInt(tsVals[0], 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid timestamp: %v", err)
	}

	// Nonce is OPTIONAL: old volunteers send none and verify against the legacy
	// (empty-nonce) canonical form. When present, it is folded into the signed
	// bytes so it is bound by ed25519.Verify (a tampered metadata nonce yields a
	// different reconstructed message and fails verification). Cap the length to
	// bound the signed-string size from a hostile metadata value.
	var nonce string
	if nonceVals := md.Get(grpcAuthNonceMeta); len(nonceVals) > 0 {
		nonce = nonceVals[0]
		if len(nonce) > grpcAuthMaxNonceLen {
			return nil, status.Error(codes.Unauthenticated, "invalid nonce: too long")
		}
	}

	now := timeNow()
	reqTime := time.Unix(ts, 0)
	skew := now.Sub(reqTime)
	if skew < -ed25519TimestampSkew || skew > ed25519TimestampSkew {
		return nil, status.Error(codes.Unauthenticated, "timestamp expired or too far in the future")
	}

	// Deterministic marshal must match the client. Both modules share the workspace
	// protobuf-go version, so the byte output (and therefore the hash) is identical.
	msg, ok := req.(proto.Message)
	if !ok {
		return nil, status.Error(codes.Internal, "request is not a protobuf message")
	}
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal request for verification: %v", err)
	}

	message := canonicalGRPCAuthMessage(ts, fullMethod, requestBytes, nonce)

	pubKey := ed25519.PublicKey(pubKeyBytes)
	if !ed25519.Verify(pubKey, []byte(message), sigBytes) {
		return nil, status.Error(codes.Unauthenticated, "invalid signature")
	}

	// Anti-replay: reject a signature we have already accepted within the skew window.
	// grpcReplayDetectionEnabled is always true in production; integration tests turn
	// it off via the integration-only test seam because they intentionally replay
	// byte-identical signed RPCs (e.g. repeated RequestWorkUnit for the same volunteer).
	if grpcReplayDetectionEnabled && !cache.checkAndAdd(sigBytes, now) {
		return nil, status.Error(codes.Unauthenticated, "replayed signature")
	}

	return pubKey, nil
}
