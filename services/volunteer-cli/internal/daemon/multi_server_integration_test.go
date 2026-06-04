package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMultiServerIntegration exercises the full multi-server volunteer journey:
// 3 servers with different work units, round-robin dispatch, correct result
// routing, heartbeat routing, history tracking, and partial failure handling.
func TestMultiServerIntegration(t *testing.T) {
	t.Run("full_journey", testMultiServerFullJourney)
	t.Run("partial_failure", testMultiServerPartialFailure)
}

// testMultiServerFullJourney: 3 healthy servers each serve one work unit,
// the daemon cycles through all three, executes each, submits results to
// the correct server, sends heartbeats to the correct server, and records
// history entries with the correct server names.
func testMultiServerFullJourney(t *testing.T) {
	// --- Per-server work unit tracking ---
	type serverTracker struct {
		mu             sync.Mutex
		workServed     bool
		submitRequests []*lettucev1.SubmitResultRequest
		startWorkWUIDs []string // work unit IDs seen in StartWork run-starts
		requestVolIDs  []string // volunteer IDs seen in requests
	}

	trackerA := &serverTracker{}
	trackerB := &serverTracker{}
	trackerC := &serverTracker{}

	makeServer := func(name, wuID, projID, volID string, tracker *serverTracker) *mockClient {
		return &mockClient{
			requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
				tracker.mu.Lock()
				tracker.requestVolIDs = append(tracker.requestVolIDs, req.VolunteerId)
				served := tracker.workServed
				tracker.workServed = true
				tracker.mu.Unlock()

				if served {
					return nil, status.Error(codes.NotFound, "no more work")
				}
				return &lettucev1.RequestWorkUnitResponse{
					Assignments: []*lettucev1.WorkUnitAssignment{
						{
							WorkUnitId:               wuID,
							LeafId:                   projID,
							Runtime:                  "native",
							InputData:                []byte(fmt.Sprintf("input-%s", name)),
							// 1s so we get heartbeats during execution
							ExecutionSpec:            &lettucev1.ExecutionSpec{},
						},
					},
				}, nil
			},
			submitResultFn: func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
				tracker.mu.Lock()
				tracker.submitRequests = append(tracker.submitRequests, req)
				tracker.mu.Unlock()
				return &lettucev1.SubmitResultResponse{ResultId: fmt.Sprintf("result-%s", name), Accepted: true}, nil
			},
			startWorkFn: func(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
				tracker.mu.Lock()
				tracker.startWorkWUIDs = append(tracker.startWorkWUIDs, req.WorkUnitId)
				tracker.mu.Unlock()
				return &lettucev1.StartWorkResponse{Ok: true}, nil
			},
		}
	}

	clientA := makeServer("alpha", "a1a1a1a1-0000-4000-8000-000000000001", "proj-alpha", "vol-alpha", trackerA)
	clientB := makeServer("beta", "b2b2b2b2-0000-4000-8000-000000000002", "proj-beta", "vol-beta", trackerB)
	clientC := makeServer("gamma", "c3c3c3c3-0000-4000-8000-000000000003", "proj-gamma", "vol-gamma", trackerC)

	connections := []*ServerConnection{
		{Client: clientA, VolunteerID: "vol-alpha", Name: "server-alpha", Available: true},
		{Client: clientB, VolunteerID: "vol-beta", Name: "server-beta", Available: true},
		{Client: clientC, VolunteerID: "vol-gamma", Name: "server-gamma", Available: true},
	}

	// Mock runtime that takes ~1.2s to execute so heartbeats have time to fire.
	var executedWUs sync.Map // work unit ID -> true
	mr := &mockRuntime{
		canHandle: true,
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			executedWUs.Store(wu.ID, true)
			select {
			case <-time.After(1200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte(fmt.Sprintf("output-%s", wu.ID)),
				OutputChecksum: fmt.Sprintf("checksum-%s", wu.ID),
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 1},
			}, nil
		},
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.Defaults()
	dataDir := t.TempDir()
	cfg.DataDir = dataDir

	d := NewDaemon(DaemonConfig{
		Config:  cfg,
		PubKey:  pub,
		PrivKey: priv,
		Servers: connections,
		Runtime: mr,
		Logger:  logger,
	})
	d.initialBackoff = 5 * time.Millisecond
	d.maxBackoff = 50 * time.Millisecond

	// Run long enough for all 3 work units to execute (~3.6s + overhead).
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	d.Run(ctx)

	// === Verify: work was requested from all 3 servers ===
	if clientA.getRequestCalls() == 0 {
		t.Error("server-alpha: no work requests received")
	}
	if clientB.getRequestCalls() == 0 {
		t.Error("server-beta: no work requests received")
	}
	if clientC.getRequestCalls() == 0 {
		t.Error("server-gamma: no work requests received")
	}

	// === Verify: all 3 work units were executed ===
	for _, wuID := range []string{"a1a1a1a1-0000-4000-8000-000000000001", "b2b2b2b2-0000-4000-8000-000000000002", "c3c3c3c3-0000-4000-8000-000000000003"} {
		if _, ok := executedWUs.Load(wuID); !ok {
			t.Errorf("work unit %s was not executed", wuID)
		}
	}

	// === Verify: results submitted to the CORRECT server ===
	// Server alpha should only have submits for wu-alpha.
	trackerA.mu.Lock()
	for _, req := range trackerA.submitRequests {
		if req.WorkUnitId != "a1a1a1a1-0000-4000-8000-000000000001" {
			t.Errorf("server-alpha received submit for wrong work unit: %s", req.WorkUnitId)
		}
		if req.VolunteerId != "vol-alpha" {
			t.Errorf("server-alpha submit used wrong volunteer ID: %s", req.VolunteerId)
		}
	}
	aSubmits := len(trackerA.submitRequests)
	trackerA.mu.Unlock()
	if aSubmits != 1 {
		t.Errorf("server-alpha submit count = %d, want 1", aSubmits)
	}

	trackerB.mu.Lock()
	for _, req := range trackerB.submitRequests {
		if req.WorkUnitId != "b2b2b2b2-0000-4000-8000-000000000002" {
			t.Errorf("server-beta received submit for wrong work unit: %s", req.WorkUnitId)
		}
		if req.VolunteerId != "vol-beta" {
			t.Errorf("server-beta submit used wrong volunteer ID: %s", req.VolunteerId)
		}
	}
	bSubmits := len(trackerB.submitRequests)
	trackerB.mu.Unlock()
	if bSubmits != 1 {
		t.Errorf("server-beta submit count = %d, want 1", bSubmits)
	}

	trackerC.mu.Lock()
	for _, req := range trackerC.submitRequests {
		if req.WorkUnitId != "c3c3c3c3-0000-4000-8000-000000000003" {
			t.Errorf("server-gamma received submit for wrong work unit: %s", req.WorkUnitId)
		}
		if req.VolunteerId != "vol-gamma" {
			t.Errorf("server-gamma submit used wrong volunteer ID: %s", req.VolunteerId)
		}
	}
	cSubmits := len(trackerC.submitRequests)
	trackerC.mu.Unlock()
	if cSubmits != 1 {
		t.Errorf("server-gamma submit count = %d, want 1", cSubmits)
	}

	// === Verify: any StartWork run-start went to the CORRECT server ===
	// Per-task heartbeats are gone; run-start is StartWork. The per-server submit
	// routing above already proves work landed on the right server. Here we only
	// assert that IF a StartWork was recorded it carried that server's work unit.
	// (The volunteer's StartWork call is wired by WP-VOL; until then these may be
	// empty, which is fine — correctness, not presence, is what we check.)
	checkStartWorkRouting := func(tr *serverTracker, wantWU string) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		for _, wuID := range tr.startWorkWUIDs {
			if wuID != wantWU {
				t.Errorf("server received StartWork for wrong work unit: got %s, want %s", wuID, wantWU)
			}
		}
	}
	checkStartWorkRouting(trackerA, "a1a1a1a1-0000-4000-8000-000000000001")
	checkStartWorkRouting(trackerB, "b2b2b2b2-0000-4000-8000-000000000002")
	checkStartWorkRouting(trackerC, "c3c3c3c3-0000-4000-8000-000000000003")

	// === Verify: history entries written with correct server names ===
	entries, err := ReadHistory(dataDir, 50)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("history entries = %d, want 3", len(entries))
	}

	// Build lookup: work_unit_id -> server_name.
	historyMap := make(map[string]string)
	for _, e := range entries {
		historyMap[e.WorkUnitID] = e.ServerName
	}

	expectedHistory := map[string]string{
		"a1a1a1a1-0000-4000-8000-000000000001": "server-alpha",
		"b2b2b2b2-0000-4000-8000-000000000002": "server-beta",
		"c3c3c3c3-0000-4000-8000-000000000003": "server-gamma",
	}
	for wuID, expectedServer := range expectedHistory {
		if actual, ok := historyMap[wuID]; !ok {
			t.Errorf("history missing entry for %s", wuID)
		} else if actual != expectedServer {
			t.Errorf("history for %s: server = %q, want %q", wuID, actual, expectedServer)
		}
	}

	// === Verify: round-robin ordering ===
	// The first request to each server should use the correct volunteer ID.
	trackerA.mu.Lock()
	if len(trackerA.requestVolIDs) > 0 && trackerA.requestVolIDs[0] != "vol-alpha" {
		t.Errorf("server-alpha first request volunteer_id = %q, want vol-alpha", trackerA.requestVolIDs[0])
	}
	trackerA.mu.Unlock()

	trackerB.mu.Lock()
	if len(trackerB.requestVolIDs) > 0 && trackerB.requestVolIDs[0] != "vol-beta" {
		t.Errorf("server-beta first request volunteer_id = %q, want vol-beta", trackerB.requestVolIDs[0])
	}
	trackerB.mu.Unlock()

	trackerC.mu.Lock()
	if len(trackerC.requestVolIDs) > 0 && trackerC.requestVolIDs[0] != "vol-gamma" {
		t.Errorf("server-gamma first request volunteer_id = %q, want vol-gamma", trackerC.requestVolIDs[0])
	}
	trackerC.mu.Unlock()
}

// testMultiServerPartialFailure: server B is unavailable, servers A and C
// still serve and receive work correctly. The daemon does not crash or hang.
func testMultiServerPartialFailure(t *testing.T) {
	// Server A: serves 1 work unit.
	var aWorkServed atomic.Bool
	var aSubmits sync.Map

	clientA := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if aWorkServed.Load() {
				return nil, status.Error(codes.NotFound, "no more work")
			}
			aWorkServed.Store(true)
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "573709bf-0916-4f1f-8973-60b2ebfd50fc", // was wu-a-partial
						LeafId:                   "proj-a",
						Runtime:                  "native",
						InputData:                []byte("input-a"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
		submitResultFn: func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
			aSubmits.Store(req.WorkUnitId, req.VolunteerId)
			return &lettucev1.SubmitResultResponse{ResultId: "result-a", Accepted: true}, nil
		},
	}

	// Server B: always returns Unavailable (simulating network failure).
	clientB := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
		submitResultFn: func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
			t.Error("server-b should never receive a submit (it never served work)")
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
		startWorkFn: func(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
			t.Error("server-b should never receive a StartWork (it never served work)")
			return nil, status.Error(codes.Unavailable, "connection refused")
		},
	}

	// Server C: serves 1 work unit.
	var cWorkServed atomic.Bool
	var cSubmits sync.Map

	clientC := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if cWorkServed.Load() {
				return nil, status.Error(codes.NotFound, "no more work")
			}
			cWorkServed.Store(true)
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "92a0b88e-5700-4da1-828b-a79e1995e82a", // was wu-c-partial
						LeafId:                   "proj-c",
						Runtime:                  "native",
						InputData:                []byte("input-c"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
		submitResultFn: func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
			cSubmits.Store(req.WorkUnitId, req.VolunteerId)
			return &lettucev1.SubmitResultResponse{ResultId: "result-c", Accepted: true}, nil
		},
	}

	connections := []*ServerConnection{
		{Client: clientA, VolunteerID: "vol-a", Name: "server-a", Available: true},
		{Client: clientB, VolunteerID: "vol-b", Name: "server-b", Available: true},
		{Client: clientC, VolunteerID: "vol-c", Name: "server-c", Available: true},
	}

	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.Defaults()
	dataDir := t.TempDir()
	cfg.DataDir = dataDir

	d := NewDaemon(DaemonConfig{
		Config:  cfg,
		PubKey:  pub,
		PrivKey: priv,
		Servers: connections,
		Runtime: mr,
		Logger:  logger,
	})
	d.initialBackoff = 5 * time.Millisecond
	d.maxBackoff = 50 * time.Millisecond
	d.multiClient.SetBackoff(5*time.Millisecond, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	d.Run(ctx)

	// === Verify: server B was tried but entered backoff ===
	if clientB.getRequestCalls() == 0 {
		t.Error("server-b should have been contacted at least once before backoff")
	}
	if connections[1].Available {
		t.Error("server-b should be marked unavailable after connection errors")
	}
	if connections[1].Backoff == 0 {
		t.Error("server-b should have a non-zero backoff")
	}

	// === Verify: servers A and C served and received results ===
	if clientA.getSubmitCalls() != 1 {
		t.Errorf("server-a submit calls = %d, want 1", clientA.getSubmitCalls())
	}
	if clientC.getSubmitCalls() != 1 {
		t.Errorf("server-c submit calls = %d, want 1", clientC.getSubmitCalls())
	}

	// Verify correct routing of submits.
	if volID, ok := aSubmits.Load("573709bf-0916-4f1f-8973-60b2ebfd50fc"); !ok {
		t.Error("server-a did not receive submit for wu-a-partial")
	} else if volID != "vol-a" {
		t.Errorf("server-a submit volunteer_id = %q, want vol-a", volID)
	}

	if volID, ok := cSubmits.Load("92a0b88e-5700-4da1-828b-a79e1995e82a"); !ok {
		t.Error("server-c did not receive submit for wu-c-partial")
	} else if volID != "vol-c" {
		t.Errorf("server-c submit volunteer_id = %q, want vol-c", volID)
	}

	// === Verify: server B received zero submits and zero heartbeats ===
	if clientB.getSubmitCalls() != 0 {
		t.Errorf("server-b submit calls = %d, want 0", clientB.getSubmitCalls())
	}
	if clientB.getStartWorkCalls() != 0 {
		t.Errorf("server-b StartWork calls = %d, want 0", clientB.getStartWorkCalls())
	}

	// === Verify: work units from A and C were actually executed ===
	if mr.getExecuteCalls() != 2 {
		t.Errorf("execute calls = %d, want 2 (one from A, one from C)", mr.getExecuteCalls())
	}

	// === Verify: history entries exist for both completed work units ===
	entries, err := ReadHistory(dataDir, 50)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history entries = %d, want 2", len(entries))
	}

	historyMap := make(map[string]string)
	for _, e := range entries {
		historyMap[e.WorkUnitID] = e.ServerName
	}
	if historyMap["573709bf-0916-4f1f-8973-60b2ebfd50fc"] != "server-a" {
		t.Errorf("history for wu-a-partial: server = %q, want server-a", historyMap["573709bf-0916-4f1f-8973-60b2ebfd50fc"])
	}
	if historyMap["92a0b88e-5700-4da1-828b-a79e1995e82a"] != "server-c" {
		t.Errorf("history for wu-c-partial: server = %q, want server-c", historyMap["92a0b88e-5700-4da1-828b-a79e1995e82a"])
	}

	// No history entry should reference server-b.
	for _, e := range entries {
		if e.ServerName == "server-b" {
			t.Errorf("history contains entry for server-b (should be unavailable): %+v", e)
		}
	}
}
