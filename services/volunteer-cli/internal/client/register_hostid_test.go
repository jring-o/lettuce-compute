package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/pow"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Per-head host-id persistence (BG-25) ---

func newTestStore(t *testing.T) *identity.HostIDStore {
	t.Helper()
	return identity.NewHostIDStore(filepath.Join(t.TempDir(), "host-ids.json"))
}

// A first-contact register echoes an EMPTY host id (mint request) and persists exactly
// the minted id the head returns.
func TestRegister_PersistsMintedHostID(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{registerResp: &lettucev1.RegisterVolunteerResponse{
		VolunteerId: "vol-1", Registered: true, HostId: "minted-1",
	}}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)
	const headKey = "head-a:443"

	_, _, hostID, err := Register(context.Background(), client, pub, store, headKey, cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if mock.registerReq.HostId != "" {
		t.Errorf("request HostId = %q, want empty (mint request)", mock.registerReq.HostId)
	}
	if hostID != "minted-1" {
		t.Errorf("returned hostID = %q, want minted-1", hostID)
	}
	if got, _ := store.Get(headKey); got != "minted-1" {
		t.Errorf("persisted id = %q, want minted-1", got)
	}
}

// A re-register echoes the stored id and, when the head confirms it (echo), the stored
// id is unchanged.
func TestRegister_EchoesKnownHostID(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{registerResp: &lettucev1.RegisterVolunteerResponse{
		VolunteerId: "vol-1", Registered: false, HostId: "known-1",
	}}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)
	const headKey = "head-a:443"
	if err := store.Set(headKey, "known-1"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	_, _, hostID, err := Register(context.Background(), client, pub, store, headKey, cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if mock.registerReq.HostId != "known-1" {
		t.Errorf("request HostId = %q, want known-1 (echo)", mock.registerReq.HostId)
	}
	if hostID != "known-1" {
		t.Errorf("returned hostID = %q, want known-1", hostID)
	}
	if got, _ := store.Get(headKey); got != "known-1" {
		t.Errorf("persisted id = %q, want known-1 (unchanged)", got)
	}
}

// When the head answers with an EMPTY host id (the echoed id is unknown/revoked, or the
// account is at its host cap), the client discards its stored id and runs host-less.
func TestRegister_DiscardsHostIDOnEmptyResponse(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{registerResp: &lettucev1.RegisterVolunteerResponse{
		VolunteerId: "vol-1", Registered: false, HostId: "",
	}}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)
	const headKey = "head-a:443"
	if err := store.Set(headKey, "stale-1"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	_, _, hostID, err := Register(context.Background(), client, pub, store, headKey, cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if mock.registerReq.HostId != "stale-1" {
		t.Errorf("request HostId = %q, want stale-1 (echoed before discard)", mock.registerReq.HostId)
	}
	if hostID != "" {
		t.Errorf("returned hostID = %q, want empty", hostID)
	}
	if got, _ := store.Get(headKey); got != "" {
		t.Errorf("stale id not discarded: store still holds %q", got)
	}
}

// --- Reactive registration proof-of-work solver (BG-25 §5) ---

// powMockService drives the reactive PoW flow: it refuses the first (solution-less)
// RegisterVolunteer with the pow-required precondition, serves challenges, and can
// reject the first submitted solution once to exercise the fresh-challenge retry.
type powMockService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	mu sync.Mutex

	difficulty          uint32
	rejectFirstSolution bool

	registerCalls  int
	challengeCalls int

	issued map[string][]byte // challenge_id -> challenge bytes handed out
}

func newPowMock(difficulty uint32) *powMockService {
	return &powMockService{difficulty: difficulty, issued: map[string][]byte{}}
}

func (s *powMockService) GetServerStatus(_ context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	return &lettucev1.GetServerStatusResponse{Status: "ok", Version: "test"}, nil
}

func (s *powMockService) GetRegistrationChallenge(_ context.Context, _ *lettucev1.GetRegistrationChallengeRequest) (*lettucev1.GetRegistrationChallengeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challengeCalls++
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("chal-%d", s.challengeCalls)
	s.issued[id] = buf
	return &lettucev1.GetRegistrationChallengeResponse{
		ChallengeId:    id,
		Challenge:      buf,
		DifficultyBits: s.difficulty,
		ExpiresAtUnix:  time.Now().Add(time.Hour).Unix(),
	}, nil
}

func (s *powMockService) RegisterVolunteer(_ context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerCalls++
	if req.PowChallengeId == "" {
		// New key, no solution: the pow-required refusal (message carries "outdated" so
		// pre-issuance builds fall to the update hint; new builds match the prefix).
		return nil, status.Error(codes.FailedPrecondition,
			"registration requires proof-of-work: this volunteer build is outdated — run update")
	}
	challenge, ok := s.issued[req.PowChallengeId]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "registration proof-of-work rejected: unknown challenge")
	}
	if s.rejectFirstSolution {
		s.rejectFirstSolution = false
		return nil, status.Error(codes.InvalidArgument, "registration proof-of-work rejected: stale challenge")
	}
	if !pow.VerifySolution(challenge, req.PublicKey, req.PowNonce, int(s.difficulty)) {
		return nil, status.Error(codes.InvalidArgument, "registration proof-of-work rejected: solution does not meet the target")
	}
	return &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-pow-ok", Registered: true, HostId: "minted-pow"}, nil
}

// pow-required => fetch a challenge, solve it, retry once and succeed.
func TestRegister_PowRequired_SolvesAndRetries(t *testing.T) {
	withMockHardware(t)
	svc := newPowMock(8) // low difficulty: sub-millisecond solve
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)

	volID, registered, hostID, err := Register(context.Background(), client, pub, store, "head-a:443", cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Register with pow-required: %v", err)
	}
	if volID != "vol-pow-ok" || !registered {
		t.Errorf("Register = (%q, %v), want (vol-pow-ok, true)", volID, registered)
	}
	if hostID != "minted-pow" {
		t.Errorf("hostID = %q, want minted-pow", hostID)
	}
	if svc.registerCalls != 2 {
		t.Errorf("register calls = %d, want 2 (pow-required + solved retry)", svc.registerCalls)
	}
	if svc.challengeCalls != 1 {
		t.Errorf("challenge fetches = %d, want 1", svc.challengeCalls)
	}
}

// A rejected first solution => fetch a FRESH challenge, re-solve, retry once more and
// succeed.
func TestRegister_PowRejected_FetchesFreshChallenge(t *testing.T) {
	withMockHardware(t)
	svc := newPowMock(8)
	svc.rejectFirstSolution = true
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)

	volID, _, _, err := Register(context.Background(), client, pub, store, "head-a:443", cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Register with pow-rejected-then-accepted: %v", err)
	}
	if volID != "vol-pow-ok" {
		t.Errorf("volID = %q, want vol-pow-ok", volID)
	}
	if svc.registerCalls != 3 {
		t.Errorf("register calls = %d, want 3 (required + rejected + accepted)", svc.registerCalls)
	}
	if svc.challengeCalls != 2 {
		t.Errorf("challenge fetches = %d, want 2 (fresh challenge after rejection)", svc.challengeCalls)
	}
}

// nonPowFailService returns a non-pow error and records whether the solver ever tried
// to fetch a challenge — it must not, since the error is not the pow-required signal.
type nonPowFailService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	mu             sync.Mutex
	challengeCalls int
}

func (s *nonPowFailService) GetServerStatus(_ context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	return &lettucev1.GetServerStatusResponse{Status: "ok", Version: "test"}, nil
}

func (s *nonPowFailService) RegisterVolunteer(_ context.Context, _ *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	return nil, status.Error(codes.Unavailable, "head down")
}

func (s *nonPowFailService) GetRegistrationChallenge(_ context.Context, _ *lettucev1.GetRegistrationChallengeRequest) (*lettucev1.GetRegistrationChallengeResponse, error) {
	s.mu.Lock()
	s.challengeCalls++
	s.mu.Unlock()
	return &lettucev1.GetRegistrationChallengeResponse{}, nil
}

// A non-pow error surfaces immediately and never engages the solver.
func TestRegister_NonPowError_DoesNotSolve(t *testing.T) {
	withMockHardware(t)
	svc := &nonPowFailService{}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	store := newTestStore(t)

	_, _, _, err := Register(context.Background(), client, pub, store, "head-a:443", cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil {
		t.Fatal("expected the non-pow error to surface")
	}
	if svc.challengeCalls != 0 {
		t.Errorf("challenge fetches = %d, want 0 (solver must not engage on a non-pow error)", svc.challengeCalls)
	}
}
