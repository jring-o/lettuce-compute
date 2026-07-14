package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	"google.golang.org/grpc/codes"
)

// --- Submit-door output content gate (design §4.3) ---
//
// Both submit surfaces enforce at the door exactly what the leaf's own comparator will later
// require of an inline output: non-empty well-formed JSON for every leaf, plus — for a
// NUMERIC_TOLERANCE leaf — the numeric flatten under the leaf's ignore/compare fields (so a
// float64-overflow numeric like 1e400 is refused). The gate runs BEFORE the transaction, so
// every reject path is checkable without a database; a submission that clears the gate falls
// through to the (nil-pool) transaction and panics, which we detect via recover to prove the
// gate let it through. One malformed/empty/non-finite output used to abort the whole
// comparison and park its unit COMPLETED forever (BG-21a).

// grpcServiceForContentGateTest builds a *volunteerService (bv* mocks) seeding a volunteer, a
// work unit, and a leaf with the given comparator mode and a generous inline size cap, so only
// the content gate decides these submissions.
func grpcServiceForContentGateTest(t *testing.T, mode string) (*volunteerService, ed25519.PublicKey, types.ID, types.ID) {
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
		ValidationConfig: leaf.ValidationConfig{ComparisonMode: mode},
		DataConfig:       leaf.DataConfig{MaxOutputSizeBytes: 100 * 1024 * 1024},
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

func TestSubmitDoor_GRPCEmptyInlineRefused(t *testing.T) {
	// An empty inline output (no output_data_url) can never corroborate anything and is
	// refused at the door. On the gRPC surface the empty-and-no-URL case is caught by the
	// handler's top-of-function "either output_data or output_data_url must be provided"
	// check (it predates E1); we assert the refusal code, not its exact message.
	svc, pub, volID, wuID := grpcServiceForContentGateTest(t, leaf.ComparisonExact)

	req := grpcSubmitReq(pub, volID, wuID, []byte{})
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected an empty inline submission to be refused, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
}

func TestSubmitDoor_GRPCMalformedJSONRefused(t *testing.T) {
	svc, pub, volID, wuID := grpcServiceForContentGateTest(t, leaf.ComparisonExact)

	req := grpcSubmitReq(pub, volID, wuID, []byte("{not valid json"))
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected malformed inline JSON to be refused, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "well-formed JSON") {
		t.Errorf("expected a JSON well-formedness error, got: %v", err)
	}
}

func TestSubmitDoor_GRPCOverflowNumericRefusedForNumericTolerance(t *testing.T) {
	// 1e400 is valid JSON but decodes to +Inf; a NUMERIC_TOLERANCE leaf flattens it and
	// the finiteness check refuses it at the door instead of parking the unit later.
	svc, pub, volID, wuID := grpcServiceForContentGateTest(t, leaf.ComparisonNumericTolerance)

	req := grpcSubmitReq(pub, volID, wuID, []byte(`{"x":1e400}`))
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.SubmitResult(ctx, req)
	if err == nil {
		t.Fatal("expected a 1e400 overflow to be refused on a NUMERIC_TOLERANCE leaf, got nil error")
	}
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "numeric validation") {
		t.Errorf("expected a numeric-validation error, got: %v", err)
	}
}

func TestSubmitDoor_GRPCOverflowNumericAcceptedForExact(t *testing.T) {
	// EXACT leaves never flatten, so the door demands only non-empty well-formed JSON.
	// 1e400 is valid JSON and must pass the gate — reaching the (nil-pool) transaction and
	// panicking — so the door does not newly refuse submissions today's validation accepts.
	svc, pub, volID, wuID := grpcServiceForContentGateTest(t, leaf.ComparisonExact)

	req := grpcSubmitReq(pub, volID, wuID, []byte(`{"x":1e400}`))
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected an EXACT-leaf 1e400 submission to pass the content gate to the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

func TestSubmitDoor_GRPCRefOnlyBypassesOutputGate(t *testing.T) {
	// A ref-only submission (external output_data_url, no inline bytes) is owned by the
	// content-verification pipeline and must bypass the inline content gate — reaching the
	// (nil-pool) transaction rather than being refused as empty. Set up the external-output
	// gate to pass (leaf opted in + head knob on + allowlisted host).
	svc, pub, volID, wuID := grpcServiceForExternalOutputTest(t, true)
	SetContentFetchPolicy(svc, true)

	req := grpcSubmitURLReq(pub, volID, wuID, "https://storage.example.com/results/wu.json")
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected a ref-only submission to bypass the inline content gate and reach the (nil-pool) transaction; it returned without panicking")
		}
	}()
	_, _ = svc.SubmitResult(ctx, req)
}

// --- Browser surface ---

// browserDepsForContentGateTest mirrors grpcServiceForContentGateTest for the browser handler.
func browserDepsForContentGateTest(t *testing.T, mode string) (*browserVolunteerDeps, ed25519.PublicKey, types.ID) {
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
		ID:               leafID,
		ValidationConfig: leaf.ValidationConfig{ComparisonMode: mode},
		DataConfig:       leaf.DataConfig{MaxOutputSizeBytes: 100 * 1024 * 1024},
	}

	wuID := types.NewID()
	wuRepo.wus = append(wuRepo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})

	_ = assignRepo.Create(context.Background(), &assignment.AssignmentHistoryEntry{
		WorkUnitID:  wuID,
		VolunteerID: vol.ID,
	})
	return deps, pub, wuID
}

func browserContentGateRequest(t *testing.T, pub ed25519.PublicKey, wuID types.ID, output []byte) *http.Request {
	t.Helper()
	body := browserSubmitBody(t, wuID, output)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	return req.WithContext(ContextWithEd25519PubKey(req.Context(), pub))
}

func TestSubmitDoor_BrowserEmptyInlineRefused(t *testing.T) {
	deps, pub, wuID := browserDepsForContentGateTest(t, leaf.ComparisonExact)
	handler := handleBrowserSubmitResult(deps)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, browserContentGateRequest(t, pub, wuID, []byte{}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty inline output, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "non-empty") {
		t.Errorf("expected a non-empty-output error, got: %s", rec.Body.String())
	}
}

func TestSubmitDoor_BrowserMalformedJSONRefused(t *testing.T) {
	deps, pub, wuID := browserDepsForContentGateTest(t, leaf.ComparisonExact)
	handler := handleBrowserSubmitResult(deps)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, browserContentGateRequest(t, pub, wuID, []byte("{not valid json")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "well-formed JSON") {
		t.Errorf("expected a JSON well-formedness error, got: %s", rec.Body.String())
	}
}

func TestSubmitDoor_BrowserOverflowNumericRefusedForNumericTolerance(t *testing.T) {
	deps, pub, wuID := browserDepsForContentGateTest(t, leaf.ComparisonNumericTolerance)
	handler := handleBrowserSubmitResult(deps)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, browserContentGateRequest(t, pub, wuID, []byte(`{"x":1e400}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a 1e400 overflow on a NUMERIC_TOLERANCE leaf, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "numeric validation") {
		t.Errorf("expected a numeric-validation error, got: %s", rec.Body.String())
	}
}

func TestSubmitDoor_BrowserOverflowNumericAcceptedForExact(t *testing.T) {
	deps, pub, wuID := browserDepsForContentGateTest(t, leaf.ComparisonExact)
	handler := handleBrowserSubmitResult(deps)

	rec := httptest.NewRecorder()
	req := browserContentGateRequest(t, pub, wuID, []byte(`{"x":1e400}`))

	// An EXACT leaf never flattens, so 1e400 (valid JSON) must pass the content gate and
	// reach the (nil-pool) transaction, which panics — proving the gate let it through.
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected an EXACT-leaf 1e400 submission to pass the content gate to the (nil-pool) transaction; got %d: %s", rec.Code, rec.Body.String())
		}
	}()
	handler.ServeHTTP(rec, req)
}
