package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strconv"
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
// the given method/timestamp using priv. Callers can then tamper with individual
// pieces to exercise failure paths.
func signedCtx(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, method string, ts int64, req proto.Message) context.Context {
	t.Helper()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signed := canonicalGRPCAuthMessage(ts, method, requestBytes)
	sig := ed25519.Sign(priv, []byte(signed))
	md := metadata.New(nil)
	md.Set(grpcAuthPubKeyMeta, string(pub))
	md.Set(grpcAuthTimestampMeta, strconv.FormatInt(ts, 10))
	md.Set(grpcAuthSignatureMeta, string(sig))
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
