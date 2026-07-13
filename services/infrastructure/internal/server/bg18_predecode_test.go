package server

// BG-18 attack differential. This file deliberately uses ONLY symbols that
// exist on the pre-fix tree (NewGRPCServer, NewVolunteerService, the generated
// client), so the closeout can run the SAME test against pre-fix code:
//
//   pre-fix: the server reads and decodes the full body, the (SubmitResult-
//            exempt) rate limiter waves the call through, and the AUTH
//            interceptor rejects it post-decode — "missing request metadata".
//   fixed:   the tap handle refuses the stream BEFORE the body is read; the
//            client sees the distinct "pre-decode admission:" status message.
//
// Both outcomes are codes.Unauthenticated; the message marker is what proves
// WHERE the refusal happened, which is the entire point of BG-18.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/transition"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startBG18Server boots a real gRPC server (production options via
// NewGRPCServer) with the plumbing-only volunteer service, and returns a
// client with NO signing interceptor — the unauthenticated attacker.
func startBG18Server(t *testing.T) lettucev1.VolunteerServiceClient {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)

	grpcServer, cleanup := NewGRPCServer(nil, logger, nil)
	t.Cleanup(cleanup)
	svc := NewVolunteerService(nil, "bg18-test", time.Now(), nil, nil, nil, nil, nil, nil, nil, nil, logger, transition.TrustPolicy{})
	lettucev1.RegisterVolunteerServiceServer(grpcServer, svc)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return lettucev1.NewVolunteerServiceClient(conn)
}

// TestBG18_UnauthenticatedStreamRefusedBeforeDecode re-runs the BG-18 attack:
// a client with no authentication metadata streams a ~1 MB SubmitResult — a
// method exempt from the pre-auth request limiter, so pre-fix NOTHING stood
// between the wire and a full 128 MB-budget decode + the auth layer's
// re-marshal + hash. The fix must refuse the stream at the tap handle, before
// the body is read.
func TestBG18_UnauthenticatedStreamRefusedBeforeDecode(t *testing.T) {
	client := startBG18Server(t)

	payload := make([]byte, 1<<20) // 1 MB of attacker-supplied body
	sum := sha256.Sum256(payload)
	req := &lettucev1.SubmitResultRequest{
		WorkUnitId:           "00000000-0000-0000-0000-000000000000",
		VolunteerId:          "00000000-0000-0000-0000-000000000000",
		PublicKey:            make([]byte, 32),
		OutputData:           payload,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.SubmitResult(ctx, req)

	if err == nil {
		t.Fatal("unauthenticated SubmitResult must be refused")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", st.Code(), err)
	}
	if !strings.Contains(st.Message(), "pre-decode admission") {
		t.Fatalf("BG-18 ATTACK LIVE: refusal came from a post-decode layer (server buffered and decoded "+
			"the attacker's body before rejecting it); want the tap handle's \"pre-decode admission:\" "+
			"marker, got: %q", st.Message())
	}
}

// TestBG18_PublicDiscoveryStillServesUnauthenticated pins the tap screen's
// scope: the public discovery methods carry no identity by design and must
// keep working without metadata (only the shape screen is skipped — the
// per-IP stream budget still applies to them).
func TestBG18_PublicDiscoveryStillServesUnauthenticated(t *testing.T) {
	client := startBG18Server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
	if err != nil {
		t.Fatalf("public GetServerStatus must not require auth metadata: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a status response")
	}
}
