package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	"google.golang.org/grpc/codes"
)

// --- External-output submission gate ---
//
// An external output (output_data_url) may enter the pipeline only when three ordered
// checks pass (design §10.2): the leaf opted in (allow_external_output), the head knob
// LETTUCE_HEAD_CONTENT_FETCH_ENABLED is on, and the URL matches the leaf's host
// allowlist. Only then is the reference HELD for content verification (the head fetches
// and hashes the served bytes before it may vote). These tests exercise that gate on the
// gRPC submit path. Like the size gate, it runs BEFORE the DB transaction, so every
// reject path is fully checkable without a database; a submission that clears the gate
// falls through to s.pool.Begin (nil pool) and panics, which we detect via recover to
// prove the gate let it through.

// grpcServiceForExternalOutputTest builds a *volunteerService with the bv* mocks
// (browser_volunteer_test.go, same package) and seeds a volunteer, work unit, and
// leaf whose ValidationConfig.allow_external_output is set as requested.
func grpcServiceForExternalOutputTest(t *testing.T, allowExternal bool) (*volunteerService, ed25519.PublicKey, types.ID, types.ID) {
	t.Helper()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	volRepo := newBVMockVolunteerRepo()
	wuRepo := &bvMockWURepo{}
	leafRepo := newBVMockLeafRepo()
	assignRepo := &bvMockAssignRepo{}
	resultRepo := &bvMockResultRepo{}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	vol := &volunteer.Volunteer{PublicKey: pub, IsActive: true}
	if cErr := volRepo.Create(context.Background(), vol); cErr != nil {
		t.Fatalf("seed volunteer: %v", cErr)
	}

	// An opted-in leaf must declare a host allowlist (D10); the URL-submission helpers
	// below target storage.example.com so a valid ref clears the allowlist check.
	vc := leaf.ValidationConfig{AllowExternalOutput: allowExternal}
	if allowExternal {
		vc.ExternalOutputHosts = []string{"storage.example.com"}
	}
	leafID := types.NewID()
	leafRepo.leafs[leafID] = &leaf.Leaf{
		ID:               leafID,
		ValidationConfig: vc,
		// A generous inline cap so the size gate never interferes with these tests.
		DataConfig: leaf.DataConfig{MaxOutputSizeBytes: 100 * 1024 * 1024},
	}

	wuID := types.NewID()
	wuRepo.wus = append(wuRepo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})

	_ = assignRepo.Create(context.Background(), &assignment.AssignmentHistoryEntry{
		WorkUnitID:  wuID,
		VolunteerID: vol.ID,
	})

	svc := &volunteerService{
		pool:          nil,
		volunteerRepo: volRepo,
		wuRepo:        wuRepo,
		leafRepo:      leafRepo,
		assignRepo:    assignRepo,
		resultRepo:    resultRepo,
		logger:        logger,
	}
	return svc, pub, vol.ID, wuID
}

// grpcSubmitURLReq builds a URL-only submission with a well-formed (but
// unverified) checksum, so the external-reference gate — not the checksum-format
// gate — is what decides the request.
func grpcSubmitURLReq(pub ed25519.PublicKey, volID, wuID types.ID, url string) *lettucev1.SubmitResultRequest {
	sum := sha256.Sum256([]byte("external output placeholder"))
	return &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID.String(),
		VolunteerId:          volID.String(),
		PublicKey:            pub,
		OutputDataUrl:        url,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1},
	}
}

func TestSubmitResult_GRPC_RejectsExternalOutputByDefault(t *testing.T) {
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, false)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://storage.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected external output_data_url to be rejected on a leaf that has not opted in, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "inline output_data only") {
		t.Errorf("expected error to explain the leaf is inline-only, got: %v", err)
	}
}

func TestSubmitResult_GRPC_RefusesExternalOutputWhenKnobOff(t *testing.T) {
	// Opted-in leaf, allowlisted host, but the head knob is OFF (the zero-value
	// default). The reference must be refused at the front door with FailedPrecondition
	// — the default state closes BG-02b even for an opted-in leaf (§10.2 step 2).
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, true)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://storage.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected external output to be refused while content fetch is disabled, got nil error")
	}
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "disabled on this head") {
		t.Errorf("expected error to explain content verification is disabled, got: %v", err)
	}
}

func TestSubmitResult_GRPC_AllowsExternalOutputWhenOptedInAndKnobOn(t *testing.T) {
	// §10.11 (vii) submit seam: opted-in leaf + knob ON + allowlisted host. The gate
	// must let the reference through; the handler then proceeds to s.pool.Begin (nil
	// pool) and panics, proving all three checks passed and the submission reached the
	// held-insert path. The +0 pendingDelta arithmetic end-to-end (a held insert does
	// not complete a unit) needs the real insert and is asserted in the DB-backed
	// integration test — the nil pool cannot exercise it here.
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, true)
	SetContentFetchPolicy(svc, true)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://storage.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected handler to proceed past the external-output gate to the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

func TestSubmitResult_GRPC_RefusesExternalOutputHostNotInAllowlist(t *testing.T) {
	// Opted-in leaf + knob ON, but the URL host is not in the leaf's allowlist. The D10
	// URL check must refuse it with InvalidArgument (§10.2 step 3) — the reference never
	// reaches the transaction.
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, true)
	SetContentFetchPolicy(svc, true)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://not-allowed.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected a URL whose host is not in the allowlist to be refused, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("expected an allowlist refusal message, got: %v", err)
	}
}

func TestSubmitResult_GRPC_InlineAcceptedOnDefaultLeaf(t *testing.T) {
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, false)

	req := grpcSubmitReq(pub, volID, wuID, []byte(`{"answer":42}`))
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// Inline output on a leaf that has NOT opted into external output must still be
	// accepted: the gate targets output_data_url only. Proceeds past the gate to the
	// (nil-pool) transaction and panics.
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected inline submission to proceed past the output gate to the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

func TestSubmitResult_GRPC_InlineChecksumStillVerified(t *testing.T) {
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, false)

	req := grpcSubmitReq(pub, volID, wuID, []byte(`{"answer":42}`))
	req.OutputChecksumSha256 = strings.Repeat("a", 64) // well-formed hex, wrong digest
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// Inline sha256 verification runs before the leaf gate and must still reject a
	// mismatched checksum — the new gate does not weaken inline verification.
	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected inline checksum mismatch to be rejected, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a checksum mismatch error, got: %v", err)
	}
}
