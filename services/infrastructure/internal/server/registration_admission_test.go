package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// --- Recording fake volunteer repository ---
//
// admissionRecordingVolunteerRepo embeds the same-package *bvMockVolunteerRepo to satisfy
// the ~20-method volunteer.Repository interface and overrides only the three methods the
// registration admission path drives: GetByPublicKey (scriptable NotFound-then-found for
// the create-race fallthrough), CreateAdmitted (records the gate argument the create path
// passes and returns a configurable error), and Update (records the update-path call).
type admissionRecordingVolunteerRepo struct {
	*bvMockVolunteerRepo

	// createErr is returned by CreateAdmitted (nil => the create succeeds and v is stamped).
	createErr error
	// existingVol is returned by GetByPublicKey once getPubKeyCalls exceeds notFoundBefore
	// (the re-fetch after a create-race Conflict); nil keeps every lookup a NotFound.
	existingVol    *volunteer.Volunteer
	notFoundBefore int

	// recordings
	getPubKeyCalls int
	createCalls    int
	recordedGate   *admission.CreateGate
	gateWasNil     bool
	updateCalls    int
}

func newAdmissionRecordingVolunteerRepo() *admissionRecordingVolunteerRepo {
	return &admissionRecordingVolunteerRepo{bvMockVolunteerRepo: newBVMockVolunteerRepo()}
}

func (m *admissionRecordingVolunteerRepo) GetByPublicKey(_ context.Context, _ []byte) (*volunteer.Volunteer, error) {
	m.getPubKeyCalls++
	if m.existingVol != nil && m.getPubKeyCalls > m.notFoundBefore {
		return m.existingVol, nil
	}
	return nil, apierror.NotFound("volunteer", "by-pubkey")
}

func (m *admissionRecordingVolunteerRepo) CreateAdmitted(_ context.Context, v *volunteer.Volunteer, gate *admission.CreateGate) error {
	m.createCalls++
	m.recordedGate = gate
	m.gateWasNil = gate == nil
	if m.createErr != nil {
		return m.createErr
	}
	v.ID = types.NewID()
	now := time.Now().UTC()
	v.CreatedAt = now
	v.RegisteredAt = now
	return nil
}

func (m *admissionRecordingVolunteerRepo) Update(_ context.Context, _ *volunteer.Volunteer) error {
	m.updateCalls++
	return nil
}

// --- gRPC test helpers ---

// newAdmissionTestService builds a bare *volunteerService over the given repo (nil pool =>
// no dispatch cache / host repo), so RegisterVolunteer can be driven as a direct method
// call with a hand-built context. Direct calls let a test stash an arbitrary client IP
// under grpcClientIPCtxKey{} (an in-package-only key), which an in-process TCP server
// cannot: its peer is always loopback.
func newAdmissionTestService(t *testing.T, repo volunteer.Repository) *volunteerService {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewVolunteerService(nil, "0.1.0-test", time.Now(), repo, nil, nil, nil, nil, nil, nil, nil, logger, transition.TrustPolicy{})
	vs, ok := svc.(*volunteerService)
	if !ok {
		t.Fatalf("NewVolunteerService did not return *volunteerService")
	}
	return vs
}

// admissionCtx builds a request context carrying the verified auth pubkey (bound to
// req.PublicKey by RegisterVolunteer) and, when ip != "", the trust-aware client IP the
// rate-limit interceptor would have stashed.
func admissionCtx(pub ed25519.PublicKey, ip string) context.Context {
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	if ip != "" {
		ctx = context.WithValue(ctx, grpcClientIPCtxKey{}, ip)
	}
	return ctx
}

// admissionRegisterReq builds a valid RegisterVolunteerRequest whose public key matches the
// authenticated key (RegisterVolunteer requires bytes.Equal(authedKey, req.PublicKey)).
func admissionRegisterReq(pub ed25519.PublicKey) *lettucev1.RegisterVolunteerRequest {
	return &lettucev1.RegisterVolunteerRequest{
		PublicKey:   []byte(pub),
		DisplayName: "Admission Test Volunteer",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      8,
			MaxCpuCores:   4,
			MemoryTotalMb: 32768,
			MaxMemoryMb:   16384,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	}
}

// setupAdmissionRegisterServer stands up the FULL in-process gRPC server (NewGRPCServer's
// real interceptor chain, including the pre-auth rate limiter that stashes the client IP)
// plus a signing client, and returns the concrete service so a test can set the admission
// policy. It proves the client IP is stashed end-to-end through the real chain.
func setupAdmissionRegisterServer(t *testing.T, repo volunteer.Repository) (lettucev1.VolunteerServiceClient, *volunteerService, ed25519.PublicKey, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate keypair: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	grpcServer, grpcCleanup := NewGRPCServer(nil, logger, nil)
	concrete := NewVolunteerService(nil, "0.1.0-test", time.Now(), repo, nil, nil, nil, nil, nil, nil, nil, logger, transition.TrustPolicy{})
	vs, ok := concrete.(*volunteerService)
	if !ok {
		t.Fatalf("NewVolunteerService did not return *volunteerService")
	}
	lettucev1.RegisterVolunteerServiceServer(grpcServer, concrete)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(testSigningClientInterceptor(priv, pub)),
	)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
		grpcCleanup()
	}
	return lettucev1.NewVolunteerServiceClient(conn), vs, pub, cleanup
}

// --- gRPC path tests ---

// Cap disabled: registrationGate returns a nil gate (the byte-for-byte legacy create path),
// so CreateAdmitted is called with a nil gate and a brand-new volunteer is registered.
func TestRegisterVolunteerAdmission_CapDisabledInert(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo) // zero-value policy: cap off

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected Registered = true for a new volunteer")
	}
	if resp.VolunteerId == "" {
		t.Error("expected a non-empty volunteer_id")
	}
	if repo.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateAdmitted call, got %d", repo.createCalls)
	}
	if !repo.gateWasNil || repo.recordedGate != nil {
		t.Errorf("expected a nil admission gate while the cap is disabled, got %+v", repo.recordedGate)
	}
}

// Cap enabled + CreateAdmitted returns ErrCreationCapExceeded: the switch maps it to
// FailedPrecondition (never ResourceExhausted) with the pinned CapExceededMessage.
func TestRegisterVolunteerAdmission_CapExceeded(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	repo.createErr = admission.ErrCreationCapExceeded
	svc := newAdmissionTestService(t, repo)
	SetAdmissionPolicy(svc, admission.CapPolicy{Enabled: true, PerDay: 1})

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	// A stashed IP is required, else the enabled cap fails closed (see FailClosed test).
	_, err = svc.RegisterVolunteer(admissionCtx(pub, "198.51.100.7"), admissionRegisterReq(pub))
	if err == nil {
		t.Fatal("expected an error when the creation cap is exceeded")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", st.Code())
	}
	if st.Message() != admission.CapExceededMessage {
		t.Errorf("expected message %q, got %q", admission.CapExceededMessage, st.Message())
	}
}

// Cap enabled + a stashed client IP: the create path builds a gate bucketed to that IP,
// carrying the policy's per-day cap.
func TestRegisterVolunteerAdmission_GateConstruction(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo)
	SetAdmissionPolicy(svc, admission.CapPolicy{Enabled: true, PerDay: 5})

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	resp, err := svc.RegisterVolunteer(admissionCtx(pub, "203.0.113.9"), admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected Registered = true")
	}
	if repo.recordedGate == nil {
		t.Fatal("expected a non-nil admission gate while the cap is enabled")
	}
	if repo.recordedGate.Bucket != "203.0.113.9" {
		t.Errorf("expected gate bucket %q, got %q", "203.0.113.9", repo.recordedGate.Bucket)
	}
	if repo.recordedGate.CapPerDay != 5 {
		t.Errorf("expected gate CapPerDay 5, got %d", repo.recordedGate.CapPerDay)
	}
}

// Cap enabled but NO client IP in context: the gate resolution fails closed and the handler
// returns Internal rather than silently running uncapped. CreateAdmitted is never reached.
func TestRegisterVolunteerAdmission_FailClosedNoClientIP(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo)
	SetAdmissionPolicy(svc, admission.CapPolicy{Enabled: true, PerDay: 3})

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	_, err = svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err == nil {
		t.Fatal("expected an error when the cap is enabled but no client IP is present")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal (fail-closed), got %s", st.Code())
	}
	if repo.createCalls != 0 {
		t.Errorf("expected CreateAdmitted to be skipped on a fail-closed gate, got %d calls", repo.createCalls)
	}
}

// Create-race: CreateAdmitted loses the get-then-create race (Conflict), so the handler
// re-fetches the now-existing row and falls through to the UPDATE path, returning
// Registered = false with the existing id.
func TestRegisterVolunteerAdmission_CreateRaceFallthrough(t *testing.T) {
	existing := &volunteer.Volunteer{ID: types.NewID()}
	repo := newAdmissionRecordingVolunteerRepo()
	repo.createErr = apierror.Conflict("volunteer with this public key already exists", nil)
	repo.existingVol = existing
	repo.notFoundBefore = 1                 // first lookup NotFound (create branch), second returns existing
	svc := newAdmissionTestService(t, repo) // cap off: isolates the race, gate is nil

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if resp.Registered {
		t.Error("expected Registered = false on the create-race update fallthrough")
	}
	if resp.VolunteerId != existing.ID.String() {
		t.Errorf("expected the existing volunteer id %q, got %q", existing.ID.String(), resp.VolunteerId)
	}
	if repo.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateAdmitted call, got %d", repo.createCalls)
	}
	if repo.getPubKeyCalls != 2 {
		t.Errorf("expected 2 GetByPublicKey calls (lookup + re-fetch), got %d", repo.getPubKeyCalls)
	}
	if repo.updateCalls != 1 {
		t.Errorf("expected exactly 1 Update call on the fallthrough, got %d", repo.updateCalls)
	}
}

// End-to-end through the real NewGRPCServer chain: the pre-auth rate-limit interceptor
// stashes the trust-aware client IP, so an enabled cap resolves a real (loopback) bucket
// without a hand-built context. This is the integration proof that the stash is wired.
func TestRegisterVolunteerAdmission_RealChainStashesClientIP(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	client, svc, pub, cleanup := setupAdmissionRegisterServer(t, repo)
	defer cleanup()
	// PerDay high so the recording fake (which does not itself enforce the cap) admits;
	// the point is the gate the handler builds from the stashed IP.
	SetAdmissionPolicy(svc, admission.CapPolicy{Enabled: true, PerDay: 100})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.RegisterVolunteer(ctx, admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected Registered = true")
	}
	if repo.recordedGate == nil {
		t.Fatal("expected a non-nil gate: the real interceptor chain should have stashed a client IP")
	}
	if repo.recordedGate.Bucket != "127.0.0.1" {
		t.Errorf("expected the loopback bucket %q from the real chain, got %q", "127.0.0.1", repo.recordedGate.Bucket)
	}
}

// --- REST path helpers ---

func admissionBrowserDeps(repo volunteer.Repository, policy admission.CapPolicy) *browserVolunteerDeps {
	return &browserVolunteerDeps{
		volunteerRepo:   repo,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		registrationCap: policy,
	}
}

func admissionBrowserRegisterBody(pub ed25519.PublicKey) string {
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	return `{"public_key":"` + pubB64 + `","display_name":"Admission Test","hardware":{"cpu_cores":4,"memory_mb":8192,"available_runtimes":["WASM"]}}`
}

// --- REST path tests ---

// Cap disabled: the browser register path also passes a nil gate and returns 201 Created.
func TestBrowserRegisterAdmission_CapDisabledInert(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	deps := admissionBrowserDeps(repo, admission.CapPolicy{}) // cap off
	handler := handleBrowserRegister(deps)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateAdmitted call, got %d", repo.createCalls)
	}
	if !repo.gateWasNil || repo.recordedGate != nil {
		t.Errorf("expected a nil admission gate while the cap is disabled, got %+v", repo.recordedGate)
	}
}

// Cap enabled + CreateAdmitted returns ErrCreationCapExceeded: the browser path returns the
// pinned 429 refusal (code REGISTRATION_CAP_EXCEEDED + CapExceededMessage).
func TestBrowserRegisterAdmission_CapExceeded(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	repo.createErr = admission.ErrCreationCapExceeded
	deps := admissionBrowserDeps(repo, admission.CapPolicy{Enabled: true, PerDay: 1})
	handler := handleBrowserRegister(deps)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	var body apierror.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body.Error.Code != "REGISTRATION_CAP_EXCEEDED" {
		t.Errorf("expected code REGISTRATION_CAP_EXCEEDED, got %q", body.Error.Code)
	}
	if body.Error.Message != admission.CapExceededMessage {
		t.Errorf("expected message %q, got %q", admission.CapExceededMessage, body.Error.Message)
	}
}

// Cap enabled: the gate bucket is derived from the request's client IP, with the port
// stripped and an IPv6 host collapsed to its /64 (clientIPFromRequest -> BucketForIP).
func TestBrowserRegisterAdmission_GateBucketing(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		wantBucket string
	}{
		{"ipv4", "203.0.113.9:1234", "203.0.113.9"},
		{"ipv6", "[2001:db8::1]:443", "2001:db8::/64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newAdmissionRecordingVolunteerRepo()
			deps := admissionBrowserDeps(repo, admission.CapPolicy{Enabled: true, PerDay: 100})
			handler := handleBrowserRegister(deps)

			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
			}
			if repo.recordedGate == nil {
				t.Fatal("expected a non-nil gate while the cap is enabled")
			}
			if repo.recordedGate.Bucket != tc.wantBucket {
				t.Errorf("expected gate bucket %q, got %q", tc.wantBucket, repo.recordedGate.Bucket)
			}
		})
	}
}

// Create-race on the browser path: CreateAdmitted loses to a concurrent registration
// (Conflict), so the handler re-fetches and returns 409 with the existing id — not 500.
func TestBrowserRegisterAdmission_CreateRaceFallthrough(t *testing.T) {
	existing := &volunteer.Volunteer{ID: types.NewID()}
	repo := newAdmissionRecordingVolunteerRepo()
	repo.createErr = apierror.Conflict("volunteer with this public key already exists", nil)
	repo.existingVol = existing
	repo.notFoundBefore = 1                                   // first lookup NotFound (create branch), second returns existing
	deps := admissionBrowserDeps(repo, admission.CapPolicy{}) // cap off: isolates the race
	handler := handleBrowserRegister(deps)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on the create-race fallthrough, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp browserRegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.VolunteerID != existing.ID.String() {
		t.Errorf("expected the existing volunteer id %q, got %q", existing.ID.String(), resp.VolunteerID)
	}
	if repo.getPubKeyCalls != 2 {
		t.Errorf("expected 2 GetByPublicKey calls (lookup + re-fetch), got %d", repo.getPubKeyCalls)
	}
}
