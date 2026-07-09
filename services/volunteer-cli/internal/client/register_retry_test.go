package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rateLimitedRegisterService is a mock VolunteerService whose RegisterVolunteer
// returns failCode for its first failUntil calls, then succeeds. GetServerStatus
// always succeeds (the connect probe is not what we are exercising here). It
// mirrors countingService (retry_test.go) but drives the RegisterVolunteer path,
// which is the authenticated bootstrap RPC a single-head daemon must not give up
// on during a rate-limit window (TODO #64).
type rateLimitedRegisterService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	mu        sync.Mutex
	calls     int
	failUntil int
	failCode  codes.Code
}

func (s *rateLimitedRegisterService) GetServerStatus(_ context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	return &lettucev1.GetServerStatusResponse{Status: "ok", Version: "test"}, nil
}

func (s *rateLimitedRegisterService) RegisterVolunteer(_ context.Context, _ *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()

	if n <= s.failUntil {
		code := s.failCode
		if code == codes.OK {
			code = codes.ResourceExhausted
		}
		return nil, status.Error(code, "slow down")
	}
	return &lettucev1.RegisterVolunteerResponse{VolunteerId: "rl-vol-ok", Registered: true}, nil
}

func (s *rateLimitedRegisterService) registerCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestRegisterRidesOutRateLimit is the regression test for TODO #64: a head that
// rate-limits the authenticated RegisterVolunteer RPC (codes.ResourceExhausted)
// must not be treated as fatal. Before the fix, Register returned the first
// ResourceExhausted to its caller, which — for a single configured head — made
// `start` exit with "could not connect to any configured server". Now Register
// backs off a rate-limit window and keeps retrying until the window clears.
func TestRegisterRidesOutRateLimit(t *testing.T) {
	withMockHardware(t)

	// Shrink the rate-limit backoff (defaults to a 30s window) via the test seam.
	old := connectRateLimitBackoff
	connectRateLimitBackoff = 10 * time.Millisecond
	defer func() { connectRateLimitBackoff = old }()

	// Rate-limited for the first 3 register calls, then OK.
	svc := &rateLimitedRegisterService{failUntil: 3, failCode: codes.ResourceExhausted}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	volID, registered, _, err := Register(context.Background(), client, pub, nil, "", cfg, configPath)
	if err != nil {
		t.Fatalf("Register should ride out the rate limit, got error: %v", err)
	}
	if volID != "rl-vol-ok" || !registered {
		t.Errorf("Register = (%q, %v), want (%q, true)", volID, registered, "rl-vol-ok")
	}
	if got := svc.registerCallCount(); got != 4 {
		t.Errorf("RegisterVolunteer calls = %d, want 4 (3 rate-limited + 1 success)", got)
	}
}

// TestRegisterDoesNotRetryGenuineError guards the other half of the contract: a
// non-rate-limit error (here codes.Unavailable, a genuinely unreachable head) is
// surfaced to the caller immediately — it must NOT be masked by the rate-limit
// retry loop, or a down head would hang the daemon forever instead of being
// reported.
func TestRegisterDoesNotRetryGenuineError(t *testing.T) {
	withMockHardware(t)

	old := connectRateLimitBackoff
	connectRateLimitBackoff = 10 * time.Millisecond
	defer func() { connectRateLimitBackoff = old }()

	// Would "fail" many times, but with Unavailable — which must not be retried.
	svc := &rateLimitedRegisterService{failUntil: 100, failCode: codes.Unavailable}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	_, _, _, err = Register(context.Background(), client, pub, nil, "", cfg, configPath)
	if err == nil {
		t.Fatal("expected error for a genuinely unreachable head, got nil")
	}
	if got := svc.registerCallCount(); got != 1 {
		t.Errorf("RegisterVolunteer calls = %d, want 1 (a genuine error must not be retried)", got)
	}
}

// TestRegisterRateLimitRespectsContextCancel verifies the retry loop exits when
// the caller's context is cancelled mid-window (e.g. the daemon is shutting down),
// rather than spinning forever.
func TestRegisterRateLimitRespectsContextCancel(t *testing.T) {
	withMockHardware(t)

	// Long backoff so we are reliably parked in the wait when the ctx is cancelled.
	old := connectRateLimitBackoff
	connectRateLimitBackoff = 10 * time.Second
	defer func() { connectRateLimitBackoff = old }()

	svc := &rateLimitedRegisterService{failUntil: 100, failCode: codes.ResourceExhausted}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, _, _, e := Register(ctx, client, pub, nil, "", cfg, configPath)
		done <- e
	}()

	select {
	case e := <-done:
		if e == nil {
			t.Fatal("expected error after context cancellation, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not return after context cancellation (retry loop ignored ctx)")
	}
}
