package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
)

// --- FIX 2 (bounded write-path auth) + FIX 3 (Submit/Abandon graceful shedding) ---
//
// These tests exercise the write-path admission/auth changes without a database.
// The service is wired with a nil pool: a shed (ResourceExhausted) must return
// BEFORE any pool touch, so a clean ResourceExhausted proves the gate fired before
// pool.Begin/FindActive (which would otherwise panic on the nil pool). The bounded
// auth path resolves identity from the warmed dispatch-cache snapshot (zero DB).

// shedTestService builds a *volunteerService wired with the bv* mocks plus a
// dispatch cache whose identity snapshot is pre-warmed for the returned volunteer,
// so resolveAuthedVolunteer resolves in memory (FIX 2) and the handler's shed gate
// (FIX 3) is the only thing under test.
func shedTestService(t *testing.T) (*volunteerService, *dispatchCache, ed25519.PublicKey, types.ID, types.ID) {
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

	vol := &volunteer.Volunteer{PublicKey: pub, IsActive: true, AvailableRuntimes: []string{leaf.RuntimeNative}}
	if cErr := volRepo.Create(context.Background(), vol); cErr != nil {
		t.Fatalf("seed volunteer: %v", cErr)
	}

	leafID := types.NewID()
	leafRepo.leafs[leafID] = &leaf.Leaf{ID: leafID, DataConfig: leaf.DataConfig{MaxOutputSizeBytes: 1 << 20}}
	wuID := types.NewID()
	wuRepo.wus = append(wuRepo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})

	svc := &volunteerService{
		pool:          nil,
		volunteerRepo: volRepo,
		wuRepo:        wuRepo,
		leafRepo:      leafRepo,
		assignRepo:    assignRepo,
		resultRepo:    resultRepo,
		logger:        logger,
	}

	// A dispatch cache with a warmed identity for the seeded volunteer (so auth
	// resolves with zero DB) and a small client admission budget we can saturate.
	cache := newDispatchCache(dispatchCacheConfig{admissionCap: 2}, dispatchDeps{
		wuRepo:        wuRepo,
		leafRepo:      leafRepo,
		assignRepo:    assignRepo,
		volunteerRepo: volRepo,
	}, logger)
	cache.putIdentity(vol)
	svc.dispatchCache = cache

	return svc, cache, pub, vol.ID, wuID
}

func saturateClientAdmission(c *dispatchCache) {
	for i := 0; i < cap(c.admission); i++ {
		c.admission <- struct{}{}
	}
}

func shedSubmitReq(pub ed25519.PublicKey, volID, wuID types.ID) *lettucev1.SubmitResultRequest {
	output := []byte(`{"x":1}`)
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

// TestSubmitResultShedsUnderSaturation: with the client admission budget saturated,
// SubmitResult returns ResourceExhausted BEFORE any pool touch (the nil pool would
// panic otherwise), proving the FIX-3 shed fires before opening a tx.
func TestSubmitResultShedsUnderSaturation(t *testing.T) {
	svc, cache, pub, volID, wuID := shedTestService(t)
	saturateClientAdmission(cache)

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	_, err := svc.SubmitResult(ctx, shedSubmitReq(pub, volID, wuID))
	if err == nil {
		t.Fatal("expected ResourceExhausted under saturation, got nil")
	}
	if codeOf(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %s: %v", codeOf(err), err)
	}
}

// TestAbandonWorkUnitShedsUnderSaturation: same property for AbandonWorkUnit.
func TestAbandonWorkUnitShedsUnderSaturation(t *testing.T) {
	svc, cache, pub, volID, wuID := shedTestService(t)
	saturateClientAdmission(cache)

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	_, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volID.String(),
		Reason:      "test",
	})
	if err == nil {
		t.Fatal("expected ResourceExhausted under saturation, got nil")
	}
	if codeOf(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %s: %v", codeOf(err), err)
	}
}

// TestResolveAuthedVolunteerWarmedNoDB: a warmed identity snapshot resolves auth with
// ZERO volunteer-repo DB touches (FIX 2 warm-path property).
func TestResolveAuthedVolunteerWarmedNoDB(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	cache := newTestCache(wuRepo, leafRepo, assignRepo)
	volRepo := &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{}}
	cache.deps.volunteerRepo = volRepo

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	volID := types.NewID()
	cache.putIdentity(&volunteer.Volunteer{ID: volID, PublicKey: pub})

	svc := &volunteerService{logger: slog.New(slog.NewTextHandler(os.Stdout, nil)), dispatchCache: cache}
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	if err := svc.resolveAuthedVolunteer(ctx, volID, "test"); err != nil {
		t.Fatalf("warmed auth should succeed, got %v", err)
	}
	if volRepo.calls() != 0 {
		t.Fatalf("warmed auth must not touch the volunteer repo, got %d calls", volRepo.calls())
	}
}

// TestResolveAuthedVolunteerColdMissSheds: a cold miss with a saturated admission
// budget returns ResourceExhausted (FIX 2 corrected semantics: shed, not collapse).
func TestResolveAuthedVolunteerColdMissSheds(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	cache := newTestCache(wuRepo, leafRepo, assignRepo)
	cache.deps.volunteerRepo = &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{}}
	for i := 0; i < cap(cache.admission); i++ {
		cache.admission <- struct{}{}
	}

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	svc := &volunteerService{logger: slog.New(slog.NewTextHandler(os.Stdout, nil)), dispatchCache: cache}
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	err := svc.resolveAuthedVolunteer(ctx, types.NewID(), "test")
	if codeOf(err) != codes.ResourceExhausted {
		t.Fatalf("cold-miss under saturation should shed ResourceExhausted, got %s: %v", codeOf(err), err)
	}
}

// TestResolveAuthedVolunteerMismatchAndUnauthed: key mismatch -> PermissionDenied;
// missing proven key -> Unauthenticated (FIX 2 preserved codes).
func TestResolveAuthedVolunteerMismatchAndUnauthed(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	cache := newTestCache(wuRepo, leafRepo, assignRepo)
	cache.deps.volunteerRepo = &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{}}

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	volID := types.NewID()
	cache.putIdentity(&volunteer.Volunteer{ID: volID, PublicKey: pub})

	svc := &volunteerService{logger: slog.New(slog.NewTextHandler(os.Stdout, nil)), dispatchCache: cache}

	// Mismatched proven key.
	ctx := contextWithGRPCAuthPublicKey(context.Background(), other)
	if err := svc.resolveAuthedVolunteer(ctx, volID, "test"); codeOf(err) != codes.PermissionDenied {
		t.Fatalf("key mismatch should be PermissionDenied, got %s: %v", codeOf(err), err)
	}

	// No proven key at all.
	if err := svc.resolveAuthedVolunteer(context.Background(), volID, "test"); codeOf(err) != codes.Unauthenticated {
		t.Fatalf("missing proven key should be Unauthenticated, got %s: %v", codeOf(err), err)
	}
}
