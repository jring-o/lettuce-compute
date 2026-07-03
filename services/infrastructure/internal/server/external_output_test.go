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
// An external output (output_data_url) is stored with a volunteer-claimed
// checksum that the head never verifies against the referenced bytes, so it may
// not enter validation unless the leaf has set allow_external_output. These tests
// exercise that gate on the gRPC submit path. Like the size gate, it runs BEFORE
// the DB transaction, so the reject path is fully checkable without a database;
// the accept path falls through to s.pool.Begin (nil pool) and panics, which we
// detect via recover to prove the gate let the submission through.

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

	leafID := types.NewID()
	leafRepo.leafs[leafID] = &leaf.Leaf{
		ID:               leafID,
		ValidationConfig: leaf.ValidationConfig{AllowExternalOutput: allowExternal},
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

func TestSubmitResult_GRPC_AllowsExternalOutputWhenOptedIn(t *testing.T) {
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, true)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://storage.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// The leaf opted in, so the gate must NOT reject. The handler then proceeds to
	// s.pool.Begin (nil pool) and panics — proving the URL submission passed the gate
	// and reached the storage path (where output_data_ref is set from output_data_url).
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected handler to proceed past the external-output gate to the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
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
