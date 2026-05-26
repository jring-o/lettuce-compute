package client

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// countingService is a mock that fails for the first N GetServerStatus calls.
type countingService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	mu        sync.Mutex
	calls     int
	failUntil int
}

func (s *countingService) GetServerStatus(_ context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()

	if n <= s.failUntil {
		return nil, status.Error(codes.Unavailable, "not ready yet")
	}
	return &lettucev1.GetServerStatusResponse{
		Status:  "ok",
		Version: "test",
	}, nil
}

func TestConnectWithRetrySuccess(t *testing.T) {
	svc := &countingService{failUntil: 0}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 2 * time.Second,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}, slog.Default())
	if err != nil {
		t.Fatalf("ConnectWithRetry: %v", err)
	}
	defer client.Close()

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestConnectWithRetryAfterFailures(t *testing.T) {
	svc := &countingService{failUntil: 3}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 2 * time.Second,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}, slog.Default())
	if err != nil {
		t.Fatalf("ConnectWithRetry: %v", err)
	}
	defer client.Close()

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 4 {
		t.Errorf("calls = %d, want 4 (3 failures + 1 success)", calls)
	}
}

func TestConnectWithRetryMaxRetries(t *testing.T) {
	svc := &countingService{failUntil: 100}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	_, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 2 * time.Second,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
		MaxRetries:     3,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (max retries)", calls)
	}
}

func TestConnectWithRetryContextCancel(t *testing.T) {
	svc := &countingService{failUntil: 1000}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := ConnectWithRetry(ctx, ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 50 * time.Millisecond,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	svc := &countingService{failUntil: 10}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	start := time.Now()
	_, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 50 * time.Millisecond,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond, // should cap here
		Multiplier:     2.0,
		MaxRetries:     8,
	}, slog.Default())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error (max retries), got nil")
	}

	// With 10ms initial, 2x multiplier, 50ms cap, 8 retries:
	// 10 + 20 + 40 + 50 + 50 + 50 + 50 = 270ms base (7 sleeps for 8 attempts)
	// Plus up to 25% jitter on each. Should complete well under 1s.
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected under 2s (backoff not capping properly)", elapsed)
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	cfg := DefaultRetryConfig()
	if cfg.InitialBackoff != 1*time.Second {
		t.Errorf("InitialBackoff = %v, want 1s", cfg.InitialBackoff)
	}
	if cfg.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff = %v, want 30s", cfg.MaxBackoff)
	}
	if cfg.Multiplier != 2.0 {
		t.Errorf("Multiplier = %v, want 2.0", cfg.Multiplier)
	}
	if cfg.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0", cfg.MaxRetries)
	}
}

func TestConnectWithRetryZeroConfigDefaults(t *testing.T) {
	// All zero RetryConfig fields should get defaults applied internally.
	svc := &countingService{failUntil: 0}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL: addr,
		Insecure:  true,
	}, RetryConfig{}, slog.Default()) // all zero values
	if err != nil {
		t.Fatalf("ConnectWithRetry: %v", err)
	}
	defer client.Close()

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestConnectWithRetryMaxRetriesExactlyOne(t *testing.T) {
	// MaxRetries=1 should attempt exactly once then fail.
	svc := &countingService{failUntil: 100}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	_, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 1 * time.Second,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
		MaxRetries:     1,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (max retries = 1)", calls)
	}
}

func TestConnectWithRetryPreCancelledContext(t *testing.T) {
	// A pre-cancelled context should fail after the first attempt's backoff wait.
	svc := &countingService{failUntil: 100}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := ConnectWithRetry(ctx, ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 50 * time.Millisecond,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestConnectWithRetrySucceedsExactlyAtMaxRetries(t *testing.T) {
	// Server becomes available on attempt 3. MaxRetries is 3 => should succeed.
	svc := &countingService{failUntil: 2}
	addr, cleanup := startMockServer(t, svc)
	defer cleanup()

	client, err := ConnectWithRetry(context.Background(), ClientConfig{
		ServerURL:   addr,
		Insecure:    true,
		ConnTimeout: 2 * time.Second,
	}, RetryConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
		MaxRetries:     3,
	}, slog.Default())
	if err != nil {
		t.Fatalf("ConnectWithRetry: %v", err)
	}
	defer client.Close()

	svc.mu.Lock()
	calls := svc.calls
	svc.mu.Unlock()
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}
