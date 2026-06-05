//go:build integration

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// testSignerKeyCtx is a context key used by integration tests to pass the Ed25519
// private+public key the client interceptor should sign the current RPC with. This
// allows a single gRPC client to act as different volunteers across calls.
type testSignerKeyCtx struct{}

// TestSignerKeys carries the keypair used to sign an outgoing RPC.
type TestSignerKeys struct {
	Pub  ed25519.PublicKey
	Priv ed25519.PrivateKey
}

// ContextWithTestSigner returns a context that instructs TestSigningInterceptor to
// sign the outgoing RPC with the given keys. Used by integration tests only.
func ContextWithTestSigner(ctx context.Context, pub ed25519.PublicKey, priv ed25519.PrivateKey) context.Context {
	return context.WithValue(ctx, testSignerKeyCtx{}, TestSignerKeys{Pub: pub, Priv: priv})
}

// SetGRPCSecurityForIntegrationTests relaxes two server-side abuse-prevention
// mechanisms that are incompatible with the e2e harness's call pattern (hundreds
// of RPCs, many byte-identical, from a single loopback IP in seconds), WITHOUT
// changing production behavior:
//
//   - anti-replay: e2e tests legitimately replay byte-identical signed RPCs (e.g.
//     repeated RequestWorkUnit for the same volunteer), which the replay cache would
//     reject. Disabled here so the auth signature itself is still verified.
//   - per-IP gRPC rate limit: raised far above the 60/min production budget so a
//     burst of test RPCs from 127.0.0.1 is not throttled.
//   - per-pubkey gRPC rate limit: raised far above the 120/min production budget
//     so a burst of multi-RPC integration calls signed by one volunteer key is
//     not throttled by the post-auth per-volunteer limiter.
//
// This is integration-build-only (see the //go:build integration tag) and is never
// linked into the production server.
func SetGRPCSecurityForIntegrationTests() {
	grpcReplayDetectionEnabled = false
	ed25519ReplayDetectionEnabled = false
	grpcRateLimit = 1_000_000
	grpcPerPubkeyRateLimit = 1_000_000
}

// InstallSharedInMemReplayStoreForTests builds ONE in-memory replay store and
// installs it as the shared cross-replica store for BOTH the gRPC auth path
// (grpcSharedReplayStore, consumed by NewGRPCServer when no explicit store is
// passed) and the HTTP/REST auth path (ed25519ReplayStore). It models the Layer-3
// scale-out wiring where N head replicas share ONE global store: a signature
// accepted by any replica is then rejected by every replica (key = signature alone,
// GLOBAL), WITHOUT Redis. The in-process two-replica scale-out test calls this once
// so both replicas' gRPC servers (built via NewGRPCServer) and HTTP handlers dedup
// against the same store. Integration-only.
func InstallSharedInMemReplayStoreForTests() {
	store := newInMemReplayStore(ed25519TimestampSkew)
	grpcSharedReplayStore = store
	ed25519ReplayStore = store
}

// SetReplayDetectionForTests toggles BOTH replay-detection gates (gRPC + REST).
// The integration harness disables detection globally (byte-identical loopback
// replays), so the cross-replica replay-rejection test re-enables it for its
// duration via t.Cleanup. Integration-only.
func SetReplayDetectionForTests(enabled bool) {
	grpcReplayDetectionEnabled = enabled
	ed25519ReplayDetectionEnabled = enabled
}

// SignedAuthMetadataForTests builds the EXACT gRPC auth metadata (pubkey,
// timestamp, signature, nonce) for one signed RPC, using the volunteer's keypair
// and a single fresh nonce. Because the returned metadata.MD carries fixed
// signature bytes, sending it to two replicas replays the SAME signature — the
// cross-replica replay the shared store must reject. It mirrors
// TestSigningInterceptor's canonical form exactly. Integration-only.
//
// fullMethod is the gRPC full method name (e.g.
// lettucev1.VolunteerService_RequestWorkUnit_FullMethodName); req is the request
// message; pub/priv are the volunteer's Ed25519 keypair.
func SignedAuthMetadataForTests(fullMethod string, req proto.Message, pub ed25519.PublicKey, priv ed25519.PrivateKey) (metadata.MD, error) {
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	unixTs := timeNow().Unix()
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	signed := canonicalGRPCAuthMessage(unixTs, fullMethod, requestBytes, nonce)
	sig := ed25519.Sign(priv, []byte(signed))
	return metadata.New(map[string]string{
		grpcAuthPubKeyMeta:    string(pub),
		grpcAuthTimestampMeta: strconv.FormatInt(unixTs, 10),
		grpcAuthSignatureMeta: string(sig),
		grpcAuthNonceMeta:     nonce,
	}), nil
}

// TestSigningInterceptor returns a UnaryClientInterceptor that signs outgoing
// (non-public) RPCs using the keypair carried in the context (see
// ContextWithTestSigner), matching the server's canonical message format. Calls
// without signer keys in context are sent unsigned (for public discovery methods).
func TestSigningInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		keys, ok := ctx.Value(testSignerKeyCtx{}).(TestSignerKeys)
		if !ok || grpcPublicMethods[method] {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		msg, ok := req.(proto.Message)
		if !ok {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		if err != nil {
			return err
		}
		unixTs := timeNow().Unix()
		// The nonce is REQUIRED on every signed RPC (the legacy no-nonce form was
		// removed). Emit a fresh 128-bit nonce per call exactly as the real client
		// does, signing the with-nonce canonical form and attaching the same hex
		// string as x-lettuce-nonce metadata so the server reconstructs identical
		// bytes. (Anti-replay is disabled for integration tests, so the fresh nonce
		// does not interfere with the harness's intentional byte-identical replays.)
		var nonceBytes [16]byte
		if _, err := rand.Read(nonceBytes[:]); err != nil {
			return err
		}
		nonce := hex.EncodeToString(nonceBytes[:])
		signed := canonicalGRPCAuthMessage(unixTs, method, requestBytes, nonce)
		sig := ed25519.Sign(keys.Priv, []byte(signed))
		ctx = metadata.AppendToOutgoingContext(ctx,
			grpcAuthPubKeyMeta, string(keys.Pub),
			grpcAuthTimestampMeta, strconv.FormatInt(unixTs, 10),
			grpcAuthSignatureMeta, string(sig),
			grpcAuthNonceMeta, nonce,
		)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
