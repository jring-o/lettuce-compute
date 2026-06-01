package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const testAuthMethod = "/lettuce.volunteer.v1.VolunteerService/SubmitResult"

// signedCtx builds an incoming-metadata context with a valid signature over req for
// the given method/timestamp using priv, signing the legacy (no-nonce) canonical
// form. Callers can then tamper with individual pieces to exercise failure paths.
func signedCtx(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, method string, ts int64, req proto.Message) context.Context {
	t.Helper()
	return signedCtxNonce(t, priv, pub, method, ts, req, "")
}

// signedCtxNonce is like signedCtx but signs the WITH-nonce canonical form (when
// nonce != "") and attaches the nonce as x-lettuce-nonce metadata, exactly as the
// real client does. nonce == "" reproduces the legacy no-nonce request (no nonce
// metadata attached, legacy canonical form signed).
func signedCtxNonce(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, method string, ts int64, req proto.Message, nonce string) context.Context {
	t.Helper()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signed := canonicalGRPCAuthMessage(ts, method, requestBytes, nonce)
	sig := ed25519.Sign(priv, []byte(signed))
	md := metadata.New(nil)
	md.Set(grpcAuthPubKeyMeta, string(pub))
	md.Set(grpcAuthTimestampMeta, strconv.FormatInt(ts, 10))
	md.Set(grpcAuthSignatureMeta, string(sig))
	if nonce != "" {
		md.Set(grpcAuthNonceMeta, nonce)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func sampleReq() *lettucev1.SubmitResultRequest {
	return &lettucev1.SubmitResultRequest{
		WorkUnitId:           "wu-123",
		VolunteerId:          "vol-456",
		OutputChecksumSha256: "abc",
	}
}

func codeOf(err error) codes.Code {
	st, _ := status.FromError(err)
	return st.Code()
}

func TestGRPCAuth_ValidSignaturePasses(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)

	var gotKey ed25519.PublicKey
	handler := func(ctx context.Context, _ any) (any, error) {
		gotKey, _ = GRPCAuthPublicKeyFromContext(ctx)
		return "ok", nil
	}
	resp, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("handler not called")
	}
	if string(gotKey) != string(pub) {
		t.Fatalf("verified key not propagated to context")
	}
}

func TestGRPCAuth_TamperedBodyFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)

	// Tamper with the request after signing — the verified hash will not match.
	tampered := sampleReq()
	tampered.VolunteerId = "attacker"

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err := interceptor(ctx, tampered, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for tampered body, got %v", err)
	}
}

func TestGRPCAuth_WrongKeyFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	// Sign with privA but present pubB as the claimed key.
	_, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, _, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, privA, pubB, testAuthMethod, time.Now().Unix(), req)

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for wrong key, got %v", err)
	}
}

func TestGRPCAuth_ExpiredTimestampFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	oldTs := time.Now().Add(-2 * ed25519TimestampSkew).Unix()
	ctx := signedCtx(t, priv, pub, testAuthMethod, oldTs, req)

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for expired timestamp, got %v", err)
	}
}

func TestGRPCAuth_ReplayedSignatureFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)
	handler := func(context.Context, any) (any, error) { return "ok", nil }

	// First call succeeds.
	if _, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler); err != nil {
		t.Fatalf("first call should succeed, got %v", err)
	}
	// Replay of the identical signature is rejected.
	_, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for replayed signature, got %v", err)
	}
}

func TestGRPCAuth_PublicMethodSkipsAuth(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	called := false
	handler := func(ctx context.Context, _ any) (any, error) {
		called = true
		// Public methods are not authenticated, so no key in context.
		if _, ok := GRPCAuthPublicKeyFromContext(ctx); ok {
			t.Error("public method should not have an authenticated key in context")
		}
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/GetServerStatus"}
	if _, err := interceptor(context.Background(), &lettucev1.GetServerStatusRequest{}, info, handler); err != nil {
		t.Fatalf("public method should pass without auth, got %v", err)
	}
	if !called {
		t.Fatal("handler not called for public method")
	}
}

func TestGRPCAuth_MissingMetadataFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err := interceptor(context.Background(), sampleReq(), &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for missing metadata, got %v", err)
	}
}

// --- Nonce (TODO #18 fix 1) tests ---

// TestGRPCAuth_WithNonceVerifies proves a request that signs the with-nonce
// canonical form and attaches x-lettuce-nonce verifies and propagates the key.
func TestGRPCAuth_WithNonceVerifies(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtxNonce(t, priv, pub, testAuthMethod, time.Now().Unix(), req, "00112233445566778899aabbccddeeff")

	var gotKey ed25519.PublicKey
	handler := func(ctx context.Context, _ any) (any, error) {
		gotKey, _ = GRPCAuthPublicKeyFromContext(ctx)
		return "ok", nil
	}
	resp, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if err != nil {
		t.Fatalf("expected success with nonce, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("handler not called")
	}
	if string(gotKey) != string(pub) {
		t.Fatalf("verified key not propagated to context")
	}
}

// TestGRPCAuth_NoNonceBackwardCompat proves an old volunteer that sends NO nonce
// (and signs the legacy <ts>:<method>:<hash> form) still verifies unchanged. This
// is the load-bearing backward-compatibility guarantee.
func TestGRPCAuth_NoNonceBackwardCompat(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtxNonce(t, priv, pub, testAuthMethod, time.Now().Unix(), req, "")

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	if _, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler); err != nil {
		t.Fatalf("expected success for legacy no-nonce request, got %v", err)
	}
}

// TestGRPCAuth_LegacyCanonicalFormIsByteStable asserts the empty-nonce branch
// reproduces the pre-nonce string BYTE-FOR-BYTE. If this ever drifts, every old
// volunteer breaks. The expected string is computed independently here, not via
// canonicalGRPCAuthMessage, so the test is a true oracle.
func TestGRPCAuth_LegacyCanonicalFormIsByteStable(t *testing.T) {
	req := sampleReq()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(requestBytes)
	const ts int64 = 1700000000
	want := fmt.Sprintf("%d:%s:%s", ts, testAuthMethod, hex.EncodeToString(sum[:]))
	got := canonicalGRPCAuthMessage(ts, testAuthMethod, requestBytes, "")
	if got != want {
		t.Fatalf("legacy canonical form drifted:\n got=%q\nwant=%q", got, want)
	}
	// And the with-nonce form must be want + ":" + nonce.
	const nonce = "deadbeefdeadbeefdeadbeefdeadbeef"
	gotN := canonicalGRPCAuthMessage(ts, testAuthMethod, requestBytes, nonce)
	wantN := want + ":" + nonce
	if gotN != wantN {
		t.Fatalf("with-nonce canonical form wrong:\n got=%q\nwant=%q", gotN, wantN)
	}
}

// TestGRPCAuth_SameBytesDifferentNoncesBothPass is the regression test for the
// benign "replayed signature" spam: two byte-identical requests in the SAME second
// but with DIFFERENT nonces produce DIFFERENT signatures, so BOTH are accepted.
// (With replay detection ENABLED, mirroring production.)
func TestGRPCAuth_SameBytesDifferentNoncesBothPass(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ts := time.Now().Unix()
	handler := func(context.Context, any) (any, error) { return "ok", nil }

	ctx1 := signedCtxNonce(t, priv, pub, testAuthMethod, ts, req, "11111111111111111111111111111111")
	ctx2 := signedCtxNonce(t, priv, pub, testAuthMethod, ts, req, "22222222222222222222222222222222")

	if _, err := interceptor(ctx1, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler); err != nil {
		t.Fatalf("first (nonce A) should pass, got %v", err)
	}
	if _, err := interceptor(ctx2, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler); err != nil {
		t.Fatalf("second (nonce B, same bytes/ts) should ALSO pass — spam is gone; got %v", err)
	}
}

// TestGRPCAuth_IdenticalNonceReplayStillRejected proves a TRUE replay (identical
// nonce + ts + bytes => identical signature) is still rejected, so the nonce does
// not weaken anti-replay.
func TestGRPCAuth_IdenticalNonceReplayStillRejected(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtxNonce(t, priv, pub, testAuthMethod, time.Now().Unix(), req, "abababababababababababababababab")
	handler := func(context.Context, any) (any, error) { return "ok", nil }

	if _, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler); err != nil {
		t.Fatalf("first call should succeed, got %v", err)
	}
	_, err := interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for identical-nonce replay, got %v", err)
	}
	if st, _ := status.FromError(err); st.Message() != "replayed signature" {
		t.Fatalf("expected \"replayed signature\", got %q", st.Message())
	}
}

// TestGRPCAuth_TamperedNonceFails proves the nonce is inside the SIGNED bytes:
// signing with nonce A but sending metadata nonce B makes the server reconstruct a
// different message, so verification fails. This guards the security pitfall where
// a metadata-only nonce would let an attacker swap the nonce and replay an old sig.
func TestGRPCAuth_TamperedNonceFails(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ts := time.Now().Unix()

	// Sign the with-nonce form using nonce A.
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signed := canonicalGRPCAuthMessage(ts, testAuthMethod, requestBytes, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	sig := ed25519.Sign(priv, []byte(signed))

	// Attach a DIFFERENT nonce B in metadata.
	md := metadata.New(nil)
	md.Set(grpcAuthPubKeyMeta, string(pub))
	md.Set(grpcAuthTimestampMeta, strconv.FormatInt(ts, 10))
	md.Set(grpcAuthSignatureMeta, string(sig))
	md.Set(grpcAuthNonceMeta, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err = interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for tampered nonce, got %v", err)
	}
	if st, _ := status.FromError(err); st.Message() != "invalid signature" {
		t.Fatalf("expected \"invalid signature\", got %q", st.Message())
	}
}

// TestGRPCAuth_OverlongNonceRejected proves the length cap is enforced before
// signature verification.
func TestGRPCAuth_OverlongNonceRejected(t *testing.T) {
	interceptor, cleanup := authInterceptor()
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ts := time.Now().Unix()
	overlong := strings.Repeat("a", grpcAuthMaxNonceLen+1)

	// Sign the with-nonce form using the overlong nonce so the signature itself is
	// valid for that message — the server must still reject on the length cap.
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signed := canonicalGRPCAuthMessage(ts, testAuthMethod, requestBytes, overlong)
	sig := ed25519.Sign(priv, []byte(signed))
	md := metadata.New(nil)
	md.Set(grpcAuthPubKeyMeta, string(pub))
	md.Set(grpcAuthTimestampMeta, strconv.FormatInt(ts, 10))
	md.Set(grpcAuthSignatureMeta, string(sig))
	md.Set(grpcAuthNonceMeta, overlong)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err = interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testAuthMethod}, handler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for overlong nonce, got %v", err)
	}
}
