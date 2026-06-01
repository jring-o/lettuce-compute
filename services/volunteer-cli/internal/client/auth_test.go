package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

const testSignMethod = "/lettuce.volunteer.v1.VolunteerService/SubmitResult"

// captureInvoker records the outgoing context (so the test can read the metadata
// the interceptor attached) and reports success without performing any RPC.
func captureInvoker(captured *context.Context) grpc.UnaryInvoker {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		*captured = ctx
		return nil
	}
}

func sampleSignReq() *lettucev1.SubmitResultRequest {
	return &lettucev1.SubmitResultRequest{
		WorkUnitId:           "wu-123",
		VolunteerId:          "vol-456",
		OutputChecksumSha256: "abc",
	}
}

// outgoingMeta extracts a single outgoing-metadata value attached by the
// interceptor. The interceptor uses AppendToOutgoingContext, so values live in the
// outgoing md.
func outgoingMeta(t *testing.T, ctx context.Context, key string) (string, bool) {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(key)
	if len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// TestSigningInterceptor_AttachesNonceAndVerifies asserts the interceptor attaches
// a 32-hex-char x-lettuce-nonce, and that the signature verifies against the
// with-nonce canonical form reconstructed from the attached metadata.
func TestSigningInterceptor_AttachesNonceAndVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	id := &Identity{PublicKey: pub, PrivateKey: priv}
	interceptor := signingClientInterceptor(id)

	req := sampleSignReq()
	var captured context.Context
	if err := interceptor(context.Background(), testSignMethod, req, nil, nil, captureInvoker(&captured)); err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	nonce, ok := outgoingMeta(t, captured, authNonceMeta)
	if !ok {
		t.Fatalf("no %s metadata attached", authNonceMeta)
	}
	if len(nonce) != 32 {
		t.Fatalf("nonce should be 32 hex chars (16 bytes), got %d: %q", len(nonce), nonce)
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Fatalf("nonce is not valid hex: %v", err)
	}

	tsStr, ok := outgoingMeta(t, captured, authTimestampMeta)
	if !ok {
		t.Fatalf("no %s metadata attached", authTimestampMeta)
	}
	sigStr, ok := outgoingMeta(t, captured, authSignatureMeta)
	if !ok {
		t.Fatalf("no %s metadata attached", authSignatureMeta)
	}
	pubStr, ok := outgoingMeta(t, captured, authPubKeyMeta)
	if !ok {
		t.Fatalf("no %s metadata attached", authPubKeyMeta)
	}
	if pubStr != string(pub) {
		t.Fatalf("attached pubkey does not match identity")
	}

	// Reconstruct the with-nonce canonical message and verify the signature. This
	// mirrors exactly what the server's with-nonce branch does, so a passing
	// verification proves byte-identical reconstruction is possible.
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(requestBytes)
	signed := fmt.Sprintf("%s:%s:%s:%s", tsStr, testSignMethod, hex.EncodeToString(sum[:]), nonce)
	if !ed25519.Verify(pub, []byte(signed), []byte(sigStr)) {
		t.Fatalf("signature does not verify against reconstructed with-nonce message")
	}
}

// TestSigningInterceptor_NonceUniquePerCall asserts two successive invocations on
// byte-identical requests produce DIFFERENT nonces AND DIFFERENT signatures. This
// is the property that prevents the server's signature-keyed replay cache from
// rejecting the second same-second request as a replay.
func TestSigningInterceptor_NonceUniquePerCall(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	id := &Identity{PublicKey: pub, PrivateKey: priv}
	interceptor := signingClientInterceptor(id)

	req := sampleSignReq()

	var c1, c2 context.Context
	if err := interceptor(context.Background(), testSignMethod, req, nil, nil, captureInvoker(&c1)); err != nil {
		t.Fatalf("call 1 error: %v", err)
	}
	if err := interceptor(context.Background(), testSignMethod, req, nil, nil, captureInvoker(&c2)); err != nil {
		t.Fatalf("call 2 error: %v", err)
	}

	n1, _ := outgoingMeta(t, c1, authNonceMeta)
	n2, _ := outgoingMeta(t, c2, authNonceMeta)
	if n1 == "" || n2 == "" {
		t.Fatalf("missing nonce(s): %q %q", n1, n2)
	}
	if n1 == n2 {
		t.Fatalf("nonces must differ across calls, both were %q", n1)
	}

	s1, _ := outgoingMeta(t, c1, authSignatureMeta)
	s2, _ := outgoingMeta(t, c2, authSignatureMeta)
	if s1 == s2 {
		t.Fatalf("signatures must differ across calls for identical bytes (replay spam guard)")
	}
}

// TestSigningInterceptor_PublicMethodNoAuth asserts public discovery methods carry
// no auth metadata (and thus no nonce).
func TestSigningInterceptor_PublicMethodNoAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := &Identity{PublicKey: pub, PrivateKey: priv}
	interceptor := signingClientInterceptor(id)

	const publicMethod = "/lettuce.volunteer.v1.VolunteerService/GetServerStatus"
	var captured context.Context
	if err := interceptor(context.Background(), publicMethod, &lettucev1.GetServerStatusRequest{}, nil, nil, captureInvoker(&captured)); err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if _, ok := outgoingMeta(t, captured, authNonceMeta); ok {
		t.Fatalf("public method should not attach a nonce")
	}
	if _, ok := outgoingMeta(t, captured, authSignatureMeta); ok {
		t.Fatalf("public method should not attach a signature")
	}
}
