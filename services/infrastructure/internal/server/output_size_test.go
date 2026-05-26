package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// --- M3: per-leaf max_output_size_bytes enforcement on result submission ---
//
// These tests exercise the size gate added to both submit paths. The gate runs
// BEFORE the DB transaction is opened, so the rejection path is fully
// exercisable without a database. The "within limit" / "unlimited" paths fall
// through to the transaction stage (pool is nil in unit tests), which we detect
// via recover — proving the size gate did NOT reject the submission.

// grpcServiceForSizeTest builds a *volunteerService wired with the bv* mocks
// (defined in browser_volunteer_test.go, same package) and seeds a volunteer,
// work unit, and leaf with the given output cap.
func grpcServiceForSizeTest(t *testing.T, maxOutput int64) (*volunteerService, ed25519.PublicKey, types.ID, types.ID) {
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
		ID:         leafID,
		DataConfig: leaf.DataConfig{MaxOutputSizeBytes: maxOutput},
	}

	wuID := types.NewID()
	wuRepo.wus = append(wuRepo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})

	// Seed an active assignment so a passing-size submission would proceed past
	// the assignment check (only relevant for the fall-through cases).
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

func grpcSubmitReq(pub ed25519.PublicKey, volID, wuID types.ID, output []byte) *lettucev1.SubmitResultRequest {
	sum := sha256.Sum256(output)
	return &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID.String(),
		VolunteerId:          volID.String(),
		PublicKey:            pub,
		OutputData:           output,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1},
	}
}

func TestSubmitResult_GRPC_RejectsOversizedOutput(t *testing.T) {
	const maxOut = 1024
	svc, pub, volID, wuID := grpcServiceForSizeTest(t, maxOut)

	output := make([]byte, maxOut+1) // one byte over the cap
	req := grpcSubmitReq(pub, volID, wuID, output)
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected oversized output to be rejected, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "max_output_size_bytes") {
		t.Errorf("expected error to mention max_output_size_bytes, got: %v", err)
	}
}

func TestSubmitResult_GRPC_AcceptsOutputWithinLimit(t *testing.T) {
	const maxOut = 1024
	svc, pub, volID, wuID := grpcServiceForSizeTest(t, maxOut)

	output := make([]byte, maxOut) // exactly at the cap — allowed
	req := grpcSubmitReq(pub, volID, wuID, output)
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// Output is within the limit, so the size gate must NOT reject. The handler
	// then proceeds to s.pool.Begin (nil pool) and panics — which proves the
	// gate let it through. Any size-rejection would have returned an error
	// before reaching the transaction.
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected handler to proceed past size gate to the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

func TestSubmitResult_GRPC_ZeroMaxIsUnlimited(t *testing.T) {
	// MaxOutputSizeBytes == 0 means "unlimited" per the config semantics.
	svc, pub, volID, wuID := grpcServiceForSizeTest(t, 0)

	output := make([]byte, 5*1024*1024) // 5MB, would exceed any small cap
	req := grpcSubmitReq(pub, volID, wuID, output)
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected handler to proceed past size gate (max=0 means unlimited); it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

// --- Browser path ---

func browserDepsForSizeTest(t *testing.T, maxOutput int64) (*browserVolunteerDeps, ed25519.PublicKey, types.ID) {
	t.Helper()
	deps, volRepo, wuRepo, leafRepo, assignRepo, _ := testBrowserDeps()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	vol := &volunteer.Volunteer{PublicKey: pub, IsActive: true}
	if cErr := volRepo.Create(context.Background(), vol); cErr != nil {
		t.Fatalf("seed volunteer: %v", cErr)
	}

	leafID := types.NewID()
	leafRepo.leafs[leafID] = &leaf.Leaf{
		ID:         leafID,
		DataConfig: leaf.DataConfig{MaxOutputSizeBytes: maxOutput},
	}

	wuID := types.NewID()
	wuRepo.wus = append(wuRepo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})

	_ = assignRepo.Create(context.Background(), &assignment.AssignmentHistoryEntry{
		WorkUnitID:  wuID,
		VolunteerID: vol.ID,
	})
	return deps, pub, wuID
}

func browserSubmitBody(t *testing.T, wuID types.ID, output []byte) string {
	t.Helper()
	sum := sha256.Sum256(output)
	b64 := base64.StdEncoding.EncodeToString(output)
	return `{"work_unit_id":"` + wuID.String() + `","output_data":"` + b64 +
		`","output_checksum":"` + hex.EncodeToString(sum[:]) + `","metrics":{"wall_clock_seconds":1}}`
}

func TestBrowserSubmitResult_RejectsOversizedOutput(t *testing.T) {
	const maxOut = 1024
	deps, pub, wuID := browserDepsForSizeTest(t, maxOut)
	handler := handleBrowserSubmitResult(deps)

	output := make([]byte, maxOut+1)
	body := browserSubmitBody(t, wuID, output)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	req = req.WithContext(ContextWithEd25519PubKey(req.Context(), pub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized output, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_output_size_bytes") {
		t.Errorf("expected body to mention max_output_size_bytes, got: %s", rec.Body.String())
	}
}

func TestBrowserSubmitResult_AcceptsOutputWithinLimit(t *testing.T) {
	const maxOut = 1024
	deps, pub, wuID := browserDepsForSizeTest(t, maxOut)
	handler := handleBrowserSubmitResult(deps)

	output := make([]byte, maxOut) // at the cap — allowed
	body := browserSubmitBody(t, wuID, output)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	req = req.WithContext(ContextWithEd25519PubKey(req.Context(), pub))
	rec := httptest.NewRecorder()

	// Within-limit submission must pass the size gate and proceed to the
	// (nil-pool) transaction, which panics. The panic proves the gate let it
	// through; a size rejection would have written a 400 and returned instead.
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected handler to proceed past size gate to the (nil-pool) transaction; got response %d: %s", rec.Code, rec.Body.String())
		}
	}()
	handler.ServeHTTP(rec, req)
}

func TestBrowserSubmitResult_ZeroMaxIsUnlimited(t *testing.T) {
	deps, pub, wuID := browserDepsForSizeTest(t, 0)
	handler := handleBrowserSubmitResult(deps)

	output := make([]byte, 5*1024*1024) // 5MB, exceeds any small cap
	body := browserSubmitBody(t, wuID, output)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	req = req.WithContext(ContextWithEd25519PubKey(req.Context(), pub))
	rec := httptest.NewRecorder()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected handler to proceed past size gate (max=0 means unlimited); got response %d: %s", rec.Code, rec.Body.String())
		}
	}()
	handler.ServeHTTP(rec, req)
}

// TestGRPCMaxMsgSize documents the chosen transport ceiling: it must be at or
// above the largest legitimate message (100MB checkpoint / 100MB inline output).
func TestGRPCMaxMsgSize(t *testing.T) {
	const hundredMB = 100 * 1024 * 1024
	if grpcMaxMsgSize < hundredMB {
		t.Fatalf("grpcMaxMsgSize (%d) must be >= 100MB (%d) to not break legitimate checkpoints/outputs", grpcMaxMsgSize, hundredMB)
	}
}
