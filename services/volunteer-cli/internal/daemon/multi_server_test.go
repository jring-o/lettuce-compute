package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- MultiServerClient tests ---

func TestMultiServerRoundRobin(t *testing.T) {
	// 3 servers, each has work â†’ work comes from servers in rotation.
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "aaaa0000-0000-4000-8000-00000000000a", ProjectId: "proj-a", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}
	serverB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "bbbb0000-0000-4000-8000-00000000000b", ProjectId: "proj-b", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}
	serverC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "cccc0000-0000-4000-8000-00000000000c", ProjectId: "proj-c", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	connections := []*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: serverC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	ctx := context.Background()

	// First request should come from server A (index 0).
	resp, conn, err := mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if resp.WorkUnitId != "aaaa0000-0000-4000-8000-00000000000a" {
		t.Errorf("request 1: got %s, want wu-a", resp.WorkUnitId)
	}
	if conn.Name != "server-a" {
		t.Errorf("request 1: conn = %s, want server-a", conn.Name)
	}

	// Second request should come from server B.
	resp, conn, err = mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if resp.WorkUnitId != "bbbb0000-0000-4000-8000-00000000000b" {
		t.Errorf("request 2: got %s, want wu-b", resp.WorkUnitId)
	}
	if conn.Name != "server-b" {
		t.Errorf("request 2: conn = %s, want server-b", conn.Name)
	}

	// Third request should come from server C.
	resp, conn, err = mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 3: %v", err)
	}
	if resp.WorkUnitId != "cccc0000-0000-4000-8000-00000000000c" {
		t.Errorf("request 3: got %s, want wu-c", resp.WorkUnitId)
	}
	if conn.Name != "server-c" {
		t.Errorf("request 3: conn = %s, want server-c", conn.Name)
	}

	// Fourth request wraps around to server A.
	resp, _, err = mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 4: %v", err)
	}
	if resp.WorkUnitId != "aaaa0000-0000-4000-8000-00000000000a" {
		t.Errorf("request 4: got %s, want wu-a", resp.WorkUnitId)
	}
}

func TestMultiServerSkipEmpty(t *testing.T) {
	// Server A has no work, server B has work â†’ returns B's work, next starts at C.
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	serverB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "bbbb0000-0000-4000-8000-00000000000b", ProjectId: "proj-b", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}
	serverC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "cccc0000-0000-4000-8000-00000000000c", ProjectId: "proj-c", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	connections := []*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: serverC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	ctx := context.Background()

	// First request: A has no work â†’ skipped â†’ B has work.
	resp, conn, err := mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if resp.WorkUnitId != "bbbb0000-0000-4000-8000-00000000000b" {
		t.Errorf("request 1: got %s, want wu-b", resp.WorkUnitId)
	}
	if conn.Name != "server-b" {
		t.Errorf("request 1: conn = %s, want server-b", conn.Name)
	}

	// Next request should start at C (round-robin advances past B).
	resp, conn, err = mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if resp.WorkUnitId != "cccc0000-0000-4000-8000-00000000000c" {
		t.Errorf("request 2: got %s, want wu-c", resp.WorkUnitId)
	}
	if conn.Name != "server-c" {
		t.Errorf("request 2: conn = %s, want server-c", conn.Name)
	}
}

func TestMultiServerAllEmpty(t *testing.T) {
	// All 3 servers return NotFound â†’ returns NotFound.
	noWork := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}

	connections := []*ServerConnection{
		{Client: noWork, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: noWork, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: noWork, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	_, _, err := mc.RequestWork(context.Background(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when all servers empty")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestMultiServerUnavailable(t *testing.T) {
	// Server B has a connection error â†’ skipped with backoff, retried after backoff.
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	serverB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
	}
	serverC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "cccc0000-0000-4000-8000-00000000000c", ProjectId: "proj-c", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	connections := []*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: serverC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)
	mc.initialBackoff = 100 * time.Millisecond

	ctx := context.Background()

	// First round: A empty, B error (gets backoff), C has work.
	resp, conn, err := mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if resp.WorkUnitId != "cccc0000-0000-4000-8000-00000000000c" {
		t.Errorf("request 1: got %s, want wu-c", resp.WorkUnitId)
	}
	if conn.Name != "server-c" {
		t.Errorf("request 1: conn = %s, want server-c", conn.Name)
	}

	// Server B should be marked unavailable.
	if connections[1].Available {
		t.Error("server B should be unavailable")
	}
	if connections[1].Backoff == 0 {
		t.Error("server B should have backoff set")
	}

	// Second request immediately: B should be skipped (still in backoff).
	// A is empty, C has work.
	resp, _, err = mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if resp.WorkUnitId != "cccc0000-0000-4000-8000-00000000000c" {
		t.Errorf("request 2: got %s, want wu-c", resp.WorkUnitId)
	}

	// B was skipped â€” verify it wasn't contacted again.
	if serverB.getRequestCalls() != 1 {
		t.Errorf("server B request calls = %d, want 1 (should be skipped in backoff)", serverB.getRequestCalls())
	}

	// Wait for backoff to expire, then B should be retried.
	time.Sleep(150 * time.Millisecond)
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if serverB.getRequestCalls() < 2 {
		t.Errorf("server B request calls = %d, want >= 2 (should retry after backoff)", serverB.getRequestCalls())
	}
}

func TestMultiServerSingleServer(t *testing.T) {
	// 1 server configured â†’ behaves identically to current (no regression).
	workCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			workCount++
			if workCount <= 2 {
				return &lettucev1.RequestWorkUnitResponse{
					WorkUnitId: fmt.Sprintf("00000000-0000-4000-8000-%012d", workCount), ProjectId: "proj-1",
					Runtime: "native", InputData: []byte("input"),
					HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
				}, nil
			}
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()

	d := NewDaemon(DaemonConfig{
		Config: cfg,
		PubKey: pub, PrivKey: priv,
		Servers: []*ServerConnection{{
			Client: mc, VolunteerID: "vol-single", Name: "single-server", Available: true,
		}},
		Runtime: mr,
		Logger:  logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	d.Run(ctx)

	if mr.getExecuteCalls() != 2 {
		t.Errorf("execute calls = %d, want 2", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 2 {
		t.Errorf("submit calls = %d, want 2", mc.getSubmitCalls())
	}
}

func TestMultiServerSubmitToCorrectServer(t *testing.T) {
	// Work unit from server B is submitted to server B (not A or C).
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	serverB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "bbbb0000-0000-4000-8000-0000000f0b0b", ProjectId: "proj-b",
				Runtime: "native", InputData: []byte("input"),
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}
	serverC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}

	connections := []*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: serverC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()

	d := NewDaemon(DaemonConfig{
		Config: cfg,
		PubKey: pub, PrivKey: priv,
		Servers: connections,
		Runtime: mr,
		Logger:  logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// Server B should have received the submit (it provided the work unit).
	if serverB.getSubmitCalls() == 0 {
		t.Error("server B submit calls = 0, want >= 1 (work came from B)")
	}

	// Servers A and C should NOT have received any submits.
	if serverA.getSubmitCalls() != 0 {
		t.Errorf("server A submit calls = %d, want 0", serverA.getSubmitCalls())
	}
	if serverC.getSubmitCalls() != 0 {
		t.Errorf("server C submit calls = %d, want 0", serverC.getSubmitCalls())
	}

	// Verify the submit used B's volunteer ID.
	serverB.mu.Lock()
	lastReq := serverB.lastSubmitReq
	serverB.mu.Unlock()
	if lastReq != nil && lastReq.VolunteerId != "vol-b" {
		t.Errorf("submit volunteer_id = %q, want vol-b", lastReq.VolunteerId)
	}
}

func TestMultiServerHistoryTracksServer(t *testing.T) {
	// Verify history entries include the server name.
	workServed := false
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workServed = true
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "11110000-0000-4000-8000-000000001515", ProjectId: "proj-1",
				Runtime: "native", InputData: []byte("input"),
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := NewDaemon(DaemonConfig{
		Config: cfg,
		PubKey: pub, PrivKey: priv,
		Servers: []*ServerConnection{{
			Client: serverA, VolunteerID: "vol-hist", Name: "my-server", Available: true,
		}},
		Runtime: &mockRuntime{canHandle: true},
		Logger:  logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	entries, err := ReadHistory(dir, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("history entries = %d, want 1", len(entries))
	}
	if entries[0].ServerName != "my-server" {
		t.Errorf("server_name = %q, want my-server", entries[0].ServerName)
	}
}

func TestMultiServerHeartbeatToCorrectServer(t *testing.T) {
	// Heartbeats go to the server that provided the work unit.
	serverA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	serverB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "bbbb0000-0000-4000-8000-00000000b00b", ProjectId: "proj-b",
				Runtime: "native", InputData: []byte("input"),
				HeartbeatIntervalSeconds: 1, // 1 second for test
				ExecutionSpec:            &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	connections := []*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
	}

	mr := &mockRuntime{
		canHandle: true,
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			// Wait long enough for at least one heartbeat.
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
			}
			return &runtime.ExecutionResult{
				OutputData: []byte("result"), OutputChecksum: "abc",
				ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 1},
			}, nil
		},
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()

	d := NewDaemon(DaemonConfig{
		Config: cfg,
		PubKey: pub, PrivKey: priv,
		Servers: connections,
		Runtime: mr,
		Logger:  logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	d.Run(ctx)

	// Server B should have received heartbeats.
	if serverB.getHeartbeatCalls() == 0 {
		t.Error("server B heartbeat calls = 0, want >= 1")
	}

	// Server A should NOT have received heartbeats.
	if serverA.getHeartbeatCalls() != 0 {
		t.Errorf("server A heartbeat calls = %d, want 0", serverA.getHeartbeatCalls())
	}
}

// --- MultiServerClient edge-case tests ---

func TestMultiServerBackoffExponentialDoubling(t *testing.T) {
	// Verify backoff doubles on repeated failures and caps at maxBackoff.
	failing := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
	}

	conn := &ServerConnection{Client: failing, VolunteerID: "vol-a", Name: "server-a", Available: true}
	connections := []*ServerConnection{conn}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)
	mc.initialBackoff = 10 * time.Millisecond
	mc.maxBackoff = 50 * time.Millisecond

	ctx := context.Background()

	// First failure: backoff should be initialBackoff (10ms).
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 10*time.Millisecond {
		t.Errorf("backoff after 1st failure = %v, want 10ms", conn.Backoff)
	}

	// Reset availability and re-trigger (simulate backoff expiry).
	conn.LastError = time.Now().Add(-100 * time.Millisecond)
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 20*time.Millisecond {
		t.Errorf("backoff after 2nd failure = %v, want 20ms", conn.Backoff)
	}

	// Third failure: 40ms.
	conn.LastError = time.Now().Add(-100 * time.Millisecond)
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 40*time.Millisecond {
		t.Errorf("backoff after 3rd failure = %v, want 40ms", conn.Backoff)
	}

	// Fourth failure: should cap at maxBackoff (50ms), not 80ms.
	conn.LastError = time.Now().Add(-100 * time.Millisecond)
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 50*time.Millisecond {
		t.Errorf("backoff after 4th failure = %v, want 50ms (max cap)", conn.Backoff)
	}
}

func TestMultiServerBackoffResetOnSuccess(t *testing.T) {
	// After a server recovers, its backoff should reset to 0.
	callCount := 0
	recovering := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			if callCount <= 2 {
				return nil, status.Error(codes.Unavailable, "connection refused")
			}
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId: "11110000-0000-4000-8000-000000005ec0", ProjectId: "proj-1", Runtime: "native",
				HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
			}, nil
		},
	}

	conn := &ServerConnection{Client: recovering, VolunteerID: "vol-a", Name: "server-a", Available: true}
	connections := []*ServerConnection{conn}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)
	mc.initialBackoff = 10 * time.Millisecond
	mc.maxBackoff = 100 * time.Millisecond

	ctx := context.Background()

	// First two calls fail, building up backoff.
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 10*time.Millisecond {
		t.Fatalf("backoff after 1st failure = %v, want 10ms", conn.Backoff)
	}

	conn.LastError = time.Now().Add(-100 * time.Millisecond) // expire backoff
	mc.RequestWork(ctx, nil, nil, nil, nil)
	if conn.Backoff != 20*time.Millisecond {
		t.Fatalf("backoff after 2nd failure = %v, want 20ms", conn.Backoff)
	}

	// Third call succeeds.
	conn.LastError = time.Now().Add(-100 * time.Millisecond) // expire backoff
	resp, _, err := mc.RequestWork(ctx, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected success on 3rd call: %v", err)
	}
	if resp.WorkUnitId != "11110000-0000-4000-8000-000000005ec0" {
		t.Errorf("work unit = %q, want 11110000-0000-4000-8000-000000005ec0", resp.WorkUnitId)
	}
	if !conn.Available {
		t.Error("server should be marked available after success")
	}
	if conn.Backoff != 0 {
		t.Errorf("backoff should be 0 after success, got %v", conn.Backoff)
	}
}

func TestMultiServerAllUnavailable(t *testing.T) {
	// All servers return connection errors (not NotFound) â€” result is NotFound
	// (no work from any server), and all servers get backoff set.
	failing := func() *mockClient {
		return &mockClient{
			requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
				return nil, status.Error(codes.Unavailable, "connection refused")
			},
		}
	}

	connections := []*ServerConnection{
		{Client: failing(), VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: failing(), VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: failing(), VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)
	mc.initialBackoff = 10 * time.Millisecond

	_, _, err := mc.RequestWork(context.Background(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when all servers are unavailable")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound code, got %v", err)
	}

	// All servers should be marked unavailable with backoff.
	for _, conn := range connections {
		if conn.Available {
			t.Errorf("server %s should be unavailable", conn.Name)
		}
		if conn.Backoff == 0 {
			t.Errorf("server %s should have backoff set", conn.Name)
		}
	}
}

func TestMultiServerVolunteerIDPassedInRequest(t *testing.T) {
	// Verify that each server's VolunteerID is used in the RequestWorkUnit call.
	var receivedIDs []string
	var mu sync.Mutex

	makeClient := func() *mockClient {
		return &mockClient{
			requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
				mu.Lock()
				receivedIDs = append(receivedIDs, req.VolunteerId)
				mu.Unlock()
				return &lettucev1.RequestWorkUnitResponse{
					WorkUnitId: "dc5ff9da-f084-4dd7-86b8-e829669814f8", ProjectId: "proj-1", Runtime: "native",
					HeartbeatIntervalSeconds: 300, ExecutionSpec: &lettucev1.ExecutionSpec{},
				}, nil
			},
		}
	}

	connections := []*ServerConnection{
		{Client: makeClient(), VolunteerID: "vol-alpha", Name: "server-a", Available: true},
		{Client: makeClient(), VolunteerID: "vol-beta", Name: "server-b", Available: true},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	ctx := context.Background()

	// Request from server A.
	mc.RequestWork(ctx, nil, nil, nil, nil)
	// Request from server B.
	mc.RequestWork(ctx, nil, nil, nil, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(receivedIDs) != 2 {
		t.Fatalf("received %d IDs, want 2", len(receivedIDs))
	}
	if receivedIDs[0] != "vol-alpha" {
		t.Errorf("first request volunteer_id = %q, want vol-alpha", receivedIDs[0])
	}
	if receivedIDs[1] != "vol-beta" {
		t.Errorf("second request volunteer_id = %q, want vol-beta", receivedIDs[1])
	}
}

func TestMultiServerAllInBackoff(t *testing.T) {
	// When all servers are in backoff, they should all be skipped and return NotFound.
	failing := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
	}

	connections := []*ServerConnection{
		{Client: failing, VolunteerID: "vol-a", Name: "server-a", Available: false, LastError: time.Now(), Backoff: 1 * time.Hour},
		{Client: failing, VolunteerID: "vol-b", Name: "server-b", Available: false, LastError: time.Now(), Backoff: 1 * time.Hour},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	callsBefore := failing.getRequestCalls()
	_, _, err := mc.RequestWork(context.Background(), nil, nil, nil, nil)
	callsAfter := failing.getRequestCalls()

	if err == nil {
		t.Fatal("expected error when all servers in backoff")
	}
	// No actual requests should have been made (all skipped).
	if callsAfter != callsBefore {
		t.Errorf("request calls = %d, want %d (servers in backoff should be skipped)", callsAfter, callsBefore)
	}
}

func TestNewDaemonLegacyClientCompat(t *testing.T) {
	// Verify that using the legacy Client/VolunteerID fields creates a
	// single ServerConnection named "default".
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()

	mc := &mockClient{}
	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		VolunteerID: "legacy-vol-id",
		Runtime:     &mockRuntime{canHandle: true},
		Logger:      logger,
	})

	servers := d.multiClient.Servers()
	if len(servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(servers))
	}
	if servers[0].Name != "default" {
		t.Errorf("server name = %q, want 'default'", servers[0].Name)
	}
	if servers[0].VolunteerID != "legacy-vol-id" {
		t.Errorf("volunteer_id = %q, want 'legacy-vol-id'", servers[0].VolunteerID)
	}
	if !servers[0].Available {
		t.Error("legacy server should be Available=true")
	}
}

// --- DaemonState persistence tests ---

func TestDaemonStateLifecycle(t *testing.T) {
	dir := t.TempDir()

	// Read nonexistent.
	state, err := ReadDaemonState(dir)
	if err != nil {
		t.Fatalf("ReadDaemonState empty: %v", err)
	}
	if state != nil {
		t.Error("expected nil state for nonexistent file")
	}

	// Write state.
	writeState := &DaemonState{
		Servers: []ServerState{
			{Name: "server-a", GRPCAddress: "a:9090", VolunteerID: "vol-a", Connected: true},
			{Name: "server-b", GRPCAddress: "b:9090", Connected: false},
		},
	}
	if err := WriteDaemonState(dir, writeState); err != nil {
		t.Fatalf("WriteDaemonState: %v", err)
	}

	// Read back.
	state, err = ReadDaemonState(dir)
	if err != nil {
		t.Fatalf("ReadDaemonState: %v", err)
	}
	if len(state.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(state.Servers))
	}
	if state.Servers[0].VolunteerID != "vol-a" {
		t.Errorf("server 0 volunteer_id = %q, want vol-a", state.Servers[0].VolunteerID)
	}
	if state.Servers[1].Connected {
		t.Error("server 1 should not be connected")
	}

	// Remove.
	RemoveDaemonState(dir)
	state, err = ReadDaemonState(dir)
	if err != nil {
		t.Fatalf("ReadDaemonState after remove: %v", err)
	}
	if state != nil {
		t.Error("expected nil state after removal")
	}
}

func TestDaemonStateCorruptedJSON(t *testing.T) {
	dir := t.TempDir()

	// Write valid state first, then corrupt it.
	if err := WriteDaemonState(dir, &DaemonState{
		Servers: []ServerState{{Name: "test", Connected: true}},
	}); err != nil {
		t.Fatalf("WriteDaemonState: %v", err)
	}

	// Overwrite with garbage.
	path := daemonStatePath(dir)
	if err := os.WriteFile(path, []byte("not valid json{{{"), 0644); err != nil {
		t.Fatalf("writing corrupt data: %v", err)
	}

	_, err := ReadDaemonState(dir)
	if err == nil {
		t.Error("ReadDaemonState should return error on corrupt JSON")
	}
}

func TestDaemonStateEmptyServers(t *testing.T) {
	dir := t.TempDir()

	// Write state with no servers.
	if err := WriteDaemonState(dir, &DaemonState{Servers: nil}); err != nil {
		t.Fatalf("WriteDaemonState: %v", err)
	}

	state, err := ReadDaemonState(dir)
	if err != nil {
		t.Fatalf("ReadDaemonState: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if len(state.Servers) != 0 {
		t.Errorf("servers = %d, want 0", len(state.Servers))
	}
}

func TestHistoryServerNameBackwardCompat(t *testing.T) {
	// History entries written before multi-server support lack server_name.
	// They should deserialize with ServerName="" (omitempty).
	dir := t.TempDir()
	path := HistoryFilePath(dir)

	// Write a JSON line without server_name field.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"work_unit_id":"wu-old","leaf_id":"proj-1","completed_at":"2026-01-01T00:00:00Z","wall_clock_seconds":10,"result_accepted":true}` + "\n")
	// Write a JSON line with server_name field.
	f.WriteString(`{"work_unit_id":"wu-new","leaf_id":"proj-1","server_name":"my-server","completed_at":"2026-01-02T00:00:00Z","wall_clock_seconds":20,"result_accepted":true}` + "\n")
	f.Close()

	entries, err := ReadHistory(dir, 50)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Newest first.
	if entries[0].ServerName != "my-server" {
		t.Errorf("new entry server_name = %q, want my-server", entries[0].ServerName)
	}
	if entries[1].ServerName != "" {
		t.Errorf("old entry server_name = %q, want empty string", entries[1].ServerName)
	}
}

func TestMultiServerServersAccessor(t *testing.T) {
	connections := []*ServerConnection{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(connections, logger)

	servers := mc.Servers()
	if len(servers) != 3 {
		t.Fatalf("Servers() length = %d, want 3", len(servers))
	}
	// Verify the returned slice is the same backing data.
	if servers[0].Name != "a" || servers[1].Name != "b" || servers[2].Name != "c" {
		t.Error("Servers() returned unexpected names")
	}
}

func TestMultiServerSetBackoff(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMultiServerClient(nil, logger)

	mc.SetBackoff(42*time.Second, 99*time.Second)
	if mc.initialBackoff != 42*time.Second {
		t.Errorf("initialBackoff = %v, want 42s", mc.initialBackoff)
	}
	if mc.maxBackoff != 99*time.Second {
		t.Errorf("maxBackoff = %v, want 99s", mc.maxBackoff)
	}
}
