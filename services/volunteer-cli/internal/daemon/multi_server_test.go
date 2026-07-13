package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- MultiServerClient tests ---

func TestMultiServerSingleServer(t *testing.T) {
	// 1 server configured Ã¢â€ â€™ behaves identically to current (no regression).
	workCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			workCount++
			if workCount <= 2 {
				return &lettucev1.RequestWorkUnitResponse{
					Assignments: []*lettucev1.WorkUnitAssignment{
						{
							WorkUnitId: fmt.Sprintf("00000000-0000-4000-8000-%012d", workCount), LeafId: "proj-1",
							Runtime: "native", InputData: []byte("input"),
							ExecutionSpec: &lettucev1.ExecutionSpec{},
						},
					},
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
		Servers: grantAllRuntimeTrust([]*ServerConnection{{
			Client: mc, VolunteerID: "vol-single", Name: "single-server", Available: true,
		}}),
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
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId: "bbbb0000-0000-4000-8000-0000000f0b0b", LeafId: "proj-b",
						Runtime: "native", InputData: []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	serverC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}

	connections := grantAllRuntimeTrust([]*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: serverC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	})

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
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId: "11110000-0000-4000-8000-000000001515", LeafId: "proj-1",
						Runtime: "native", InputData: []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{},
					},
				},
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
		Servers: grantAllRuntimeTrust([]*ServerConnection{{
			Client: serverA, VolunteerID: "vol-hist", Name: "my-server", Available: true,
		}}),
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
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId: "bbbb0000-0000-4000-8000-00000000b00b", LeafId: "proj-b",
						Runtime: "native", InputData: []byte("input"),
						// 1 second for test
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}

	connections := grantAllRuntimeTrust([]*ServerConnection{
		{Client: serverA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: serverB, VolunteerID: "vol-b", Name: "server-b", Available: true},
	})

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

	// Server B (the one with work) should have received the result submission.
	// (Per-task heartbeats are gone; SubmitResult is the proof the unit ran here.)
	if serverB.getSubmitCalls() == 0 {
		t.Error("server B submit calls = 0, want >= 1")
	}

	// Server A should NOT have received any result submission.
	if serverA.getSubmitCalls() != 0 {
		t.Errorf("server A submit calls = %d, want 0", serverA.getSubmitCalls())
	}
}

// --- MultiServerClient edge-case tests ---

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
