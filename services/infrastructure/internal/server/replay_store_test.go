package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestInMemReplayStore_SeenWithin verifies the in-mem store reports a signature as
// new on first sight and already-seen on the second, and never errors.
func TestInMemReplayStore_SeenWithin(t *testing.T) {
	store := newInMemReplayStore(ed25519TimestampSkew)
	sig := []byte("signature-bytes-AAAA")

	seen, err := store.SeenWithin(context.Background(), sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen {
		t.Fatal("first sight should report not-already-seen")
	}

	seen, err = store.SeenWithin(context.Background(), sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !seen {
		t.Fatal("second sight should report already-seen (a replay)")
	}

	// A different signature is independent.
	seen, _ = store.SeenWithin(context.Background(), []byte("different-sig-BBBB"), ed25519TimestampSkew)
	if seen {
		t.Fatal("distinct signature must be treated as new")
	}
}

// TestCrossReplicaReplay_GRPC_SharedStore is the BREAK-2 DoD proof WITHOUT Redis:
// two independent gRPC auth interceptors (modeling two head replicas) SHARE ONE
// replay store. A byte-identical signed RPC accepted by replica A is rejected by
// replica B with "replayed signature". This is the cross-replica replay rejection
// the per-process status quo could not provide.
func TestCrossReplicaReplay_GRPC_SharedStore(t *testing.T) {
	// Detection must be ON for this test (the integration seam disables it). Save
	// and restore so test ordering is unaffected.
	prev := grpcReplayDetectionEnabled
	grpcReplayDetectionEnabled = true
	defer func() { grpcReplayDetectionEnabled = prev }()

	shared := newInMemReplayStore(ed25519TimestampSkew)
	replicaA, cleanupA := authInterceptor(shared)
	defer cleanupA()
	replicaB, cleanupB := authInterceptor(shared)
	defer cleanupB()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)

	okHandler := func(context.Context, any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: testAuthMethod}

	// Replica A accepts the (first-seen) signed request.
	if _, err := replicaA(ctx, req, info, okHandler); err != nil {
		t.Fatalf("replica A should accept first request, got %v", err)
	}

	// Replica B receives the byte-identical replay and MUST reject it, because the
	// signature is already recorded in the SHARED store.
	_, err := replicaB(ctx, req, info, okHandler)
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("replica B should reject cross-replica replay with Unauthenticated, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "replayed signature") {
		t.Fatalf("expected \"replayed signature\" from replica B, got %v", err)
	}
}

// TestCrossReplicaReplay_HTTP_SharedStore is the HTTP/REST (browser/WASM) analog:
// two ed25519AuthRequired handlers share ONE store; the second rejects the replay
// with "replayed request".
func TestCrossReplicaReplay_HTTP_SharedStore(t *testing.T) {
	prevStore := ed25519ReplayStore
	prevEnabled := ed25519ReplayDetectionEnabled
	ed25519ReplayDetectionEnabled = true
	shared := newInMemReplayStore(ed25519TimestampSkew)
	ed25519ReplayStore = shared
	defer func() {
		ed25519ReplayStore = prevStore
		ed25519ReplayDetectionEnabled = prevEnabled
	}()

	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Unix()
	body := `{"x":1}`
	path := "/api/v1/volunteers/request-work"
	auth := signRequest(t, privKey, pubKey, "POST", path, body, ts)

	// Both replicas use the SAME global store (set above), so a fresh handler models
	// a second replica.
	replicaA := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	replicaB := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	reqA := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	reqA.Header.Set("Authorization", auth)
	recA := httptest.NewRecorder()
	replicaA.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("replica A should accept first request, got %d", recA.Code)
	}

	reqB := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	reqB.Header.Set("Authorization", auth)
	recB := httptest.NewRecorder()
	replicaB.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusUnauthorized {
		t.Fatalf("replica B should reject cross-replica replay, got %d", recB.Code)
	}
}

// errReplayStore always returns a store error, to exercise fail-open / fail-closed.
type errReplayStore struct{}

func (errReplayStore) SeenWithin(context.Context, []byte, time.Duration) (bool, error) {
	return false, errors.New("simulated store outage")
}

// TestReplayStoreError_FailOpenAdmits verifies that on a store error the default
// fail-open policy ADMITS the request (favoring availability), and fail-closed
// REJECTS it.
func TestReplayStoreError_FailOpenAdmits(t *testing.T) {
	prevDetect := grpcReplayDetectionEnabled
	prevOpen := replayFailsOpen
	grpcReplayDetectionEnabled = true
	defer func() {
		grpcReplayDetectionEnabled = prevDetect
		replayFailsOpen = prevOpen
	}()

	intercept, cleanup := authInterceptor(errReplayStore{})
	defer cleanup()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := sampleReq()
	ctx := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)
	okHandler := func(context.Context, any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: testAuthMethod}

	// Fail-open: store error admits the request.
	replayFailsOpen = true
	if _, err := intercept(ctx, req, info, okHandler); err != nil {
		t.Fatalf("fail-open should admit on store error, got %v", err)
	}

	// Fail-closed: store error rejects (Unavailable). Re-sign to avoid any
	// timestamp edge; the signature is irrelevant since the store always errors.
	ctx2 := signedCtx(t, priv, pub, testAuthMethod, time.Now().Unix(), req)
	replayFailsOpen = false
	_, err := intercept(ctx2, req, info, okHandler)
	if codeOf(err) != codes.Unavailable {
		t.Fatalf("fail-closed should reject on store error with Unavailable, got %v", err)
	}
}
