package server

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestBG18_TapPerIPStreamBudget proves the pre-decode per-IP stream budget:
// with the budget lowered, a burst from one IP is refused at the tap — even
// for a method that is EXEMPT from the request-rate limiters (SubmitResult was
// the audit's exact attack surface) and for public methods.
func TestBG18_TapPerIPStreamBudget(t *testing.T) {
	origBudget := grpcStreamRateLimit
	grpcStreamRateLimit = 3
	defer func() { grpcStreamRateLimit = origBudget }()

	client := startBG18Server(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The first 3 streams are admitted at the tap (they then fail later layers
	// or succeed — irrelevant here); the 4th must be refused pre-decode.
	for i := 0; i < 3; i++ {
		_, err := client.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
		if err != nil {
			t.Fatalf("stream %d within budget must be admitted: %v", i+1, err)
		}
	}
	_, err := client.SubmitResult(ctx, &lettucev1.SubmitResultRequest{})
	if err == nil {
		t.Fatal("stream over budget must be refused")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted || !strings.Contains(st.Message(), "pre-decode admission") {
		t.Fatalf("expected pre-decode ResourceExhausted refusal, got %s: %q", st.Code(), st.Message())
	}
}

// TestBG18_ShapeScreen pins screenAuthMetadataShape: a stream without a
// plausibly-authenticating header set is refused; a well-formed set passes
// (verification stays with the post-decode auth interceptor).
func TestBG18_ShapeScreen(t *testing.T) {
	now := timeNow().Unix()
	valid := func() metadata.MD {
		return metadata.MD{
			grpcAuthPubKeyMeta:    []string{string(make([]byte, ed25519.PublicKeySize))},
			grpcAuthSignatureMeta: []string{string(make([]byte, ed25519.SignatureSize))},
			grpcAuthTimestampMeta: []string{strconv.FormatInt(now, 10)},
			grpcAuthNonceMeta:     []string{"00112233445566778899aabbccddeeff"},
		}
	}

	if err := screenAuthMetadataShape(valid()); err != nil {
		t.Fatalf("well-formed metadata must pass the shape screen: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(metadata.MD)
	}{
		{"missing pubkey", func(md metadata.MD) { delete(md, grpcAuthPubKeyMeta) }},
		{"short pubkey", func(md metadata.MD) { md[grpcAuthPubKeyMeta] = []string{"short"} }},
		{"missing signature", func(md metadata.MD) { delete(md, grpcAuthSignatureMeta) }},
		{"short signature", func(md metadata.MD) { md[grpcAuthSignatureMeta] = []string{"short"} }},
		{"missing timestamp", func(md metadata.MD) { delete(md, grpcAuthTimestampMeta) }},
		{"non-numeric timestamp", func(md metadata.MD) { md[grpcAuthTimestampMeta] = []string{"yesterday"} }},
		{"stale timestamp", func(md metadata.MD) {
			md[grpcAuthTimestampMeta] = []string{strconv.FormatInt(now-int64(ed25519TimestampSkew.Seconds())-60, 10)}
		}},
		{"missing nonce", func(md metadata.MD) { delete(md, grpcAuthNonceMeta) }},
		{"over-long nonce", func(md metadata.MD) { md[grpcAuthNonceMeta] = []string{strings.Repeat("a", grpcAuthMaxNonceLen+1)} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := valid()
			tc.mutate(md)
			err := screenAuthMetadataShape(md)
			if err == nil {
				t.Fatal("malformed metadata must be refused pre-decode")
			}
			if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
				t.Fatalf("expected Unauthenticated, got %s", st.Code())
			}
		})
	}
}

// TestBG18_PerMethodSizeGate pins the per-method receive ceiling: a small
// method carrying a bulk-sized body is rejected BEFORE the handler (and
// therefore before rate-limit/auth in the real chain), while the documented
// bulk methods still accept large payloads.
func TestBG18_PerMethodSizeGate(t *testing.T) {
	gate := grpcPerMethodSizeGateInterceptor(slog.New(slog.DiscardHandler))
	bigBody := strings.Repeat("x", 2<<20) // 2 MB — over the 1 MB small-method ceiling

	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return nil, nil
	}

	// A 2 MB RegisterVolunteer (small method) must be rejected without reaching
	// the handler.
	_, err := gate(context.Background(),
		&lettucev1.RegisterVolunteerRequest{DisplayName: bigBody},
		&grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/RegisterVolunteer"},
		handler)
	if err == nil || handlerCalled {
		t.Fatalf("oversized small-method request must be rejected before the handler (err=%v called=%v)", err, handlerCalled)
	}
	if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %s", st.Code())
	}

	// The same 2 MB is legitimate on the bulk methods (both services'
	// SubmitResult and SaveCheckpoint).
	for _, tc := range []struct {
		method string
		req    any
	}{
		{lettucev1.VolunteerService_SubmitResult_FullMethodName, &lettucev1.SubmitResultRequest{OutputData: []byte(bigBody)}},
		{lettucev1.VolunteerService_SaveCheckpoint_FullMethodName, &lettucev1.SaveCheckpointRequest{CheckpointData: []byte(bigBody)}},
		{lettucev1.AuditService_SubmitResult_FullMethodName, &lettucev1.SubmitAuditResultRequest{OutputData: []byte(bigBody)}},
	} {
		handlerCalled = false
		if _, err := gate(context.Background(), tc.req, &grpc.UnaryServerInfo{FullMethod: tc.method}, handler); err != nil {
			t.Fatalf("bulk method %s must accept a 2 MB body: %v", tc.method, err)
		}
		if !handlerCalled {
			t.Fatalf("bulk method %s must reach the handler", tc.method)
		}
	}
}

// TestBG18_ServerHardeningPosture pins the transport hardening numbers the
// server is constructed with (grpc.Server does not expose its options, so the
// posture is asserted at the source — the C-cluster TestNewGuardedHTTPClientPosture
// style). Changing any of these is a deliberate, reviewed decision.
func TestBG18_ServerHardeningPosture(t *testing.T) {
	if grpcMaxConcurrentStreams != 100 {
		t.Errorf("per-connection stream cap: got %d, want 100", grpcMaxConcurrentStreams)
	}
	if grpcKeepaliveMinPingInterval != time.Minute {
		t.Errorf("keepalive enforcement MinTime: got %v, want 1m", grpcKeepaliveMinPingInterval)
	}
	if grpcKeepaliveMaxConnectionIdle != 15*time.Minute {
		t.Errorf("MaxConnectionIdle: got %v, want 15m", grpcKeepaliveMaxConnectionIdle)
	}
	if grpcKeepaliveServerPingTime != 2*time.Hour || grpcKeepaliveServerPingTimeout != 20*time.Second {
		t.Errorf("server keepalive ping: got %v/%v, want 2h/20s", grpcKeepaliveServerPingTime, grpcKeepaliveServerPingTimeout)
	}
	if grpcStreamRateLimit != 600 {
		t.Errorf("pre-decode per-IP stream budget: got %d, want 600", grpcStreamRateLimit)
	}
	if grpcDefaultMethodMaxMsgSize != 1<<20 {
		t.Errorf("small-method size ceiling: got %d, want 1MB", grpcDefaultMethodMaxMsgSize)
	}
	// The bulk table must cover exactly the documented 100 MB-class methods, at
	// the transport ceiling, resolved via trailing-name matching for both
	// services' SubmitResult.
	for _, m := range []string{
		lettucev1.VolunteerService_SubmitResult_FullMethodName,
		lettucev1.VolunteerService_SaveCheckpoint_FullMethodName,
		lettucev1.AuditService_SubmitResult_FullMethodName,
	} {
		if got := methodMaxMsgSize(m); got != grpcMaxMsgSize {
			t.Errorf("bulk method %s ceiling: got %d, want %d", m, got, grpcMaxMsgSize)
		}
	}
	if got := methodMaxMsgSize(lettucev1.VolunteerService_RequestWorkUnit_FullMethodName); got != grpcDefaultMethodMaxMsgSize {
		t.Errorf("RequestWorkUnit ceiling: got %d, want %d", got, grpcDefaultMethodMaxMsgSize)
	}
	if len(grpcBulkMethodMaxMsgSize) != 2 {
		t.Errorf("bulk-method table has %d entries, want 2 (SubmitResult, SaveCheckpoint) — additions are deliberate decisions", len(grpcBulkMethodMaxMsgSize))
	}
}
