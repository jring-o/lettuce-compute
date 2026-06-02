//go:build integration

package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestSwarmSimSmoke runs the simulator end to end against an externally-running
// head, in BOTH profiles, and asserts:
//
//	(a) every volunteer registered,
//	(b) some assignments were dispatched, and
//	(c) the buffered profile's total RequestWorkUnit count is strictly LOWER
//	    than the naive profile's for the same fleet size and duration.
//
// Unlike the head-side e2e harness, this test CANNOT stand up an in-process
// head: the simulator lives in the volunteer-cli module and cannot import the
// infrastructure internal packages. So it drives a head you stand up out of
// process (the smoke recipe in CONTRIBUTING.md / this work package's handoff):
// a throwaway podman Postgres, migrations applied, and `lettuce-server` running
// with TLS off on 127.0.0.1:9090 (gRPC) / :8080 (HTTP).
//
// It is gated on env vars so `go test` in CI without a head simply skips:
//
//	SWARM_SMOKE_GRPC   head gRPC addr     (e.g. 127.0.0.1:9090)
//	SWARM_SMOKE_HTTP   head HTTP base URL (e.g. http://127.0.0.1:8080)
//	SWARM_SMOKE_ADMIN  admin API key (or LETTUCE_ADMIN_API_KEY)
func TestSwarmSimSmoke(t *testing.T) {
	grpcAddr := os.Getenv("SWARM_SMOKE_GRPC")
	httpURL := os.Getenv("SWARM_SMOKE_HTTP")
	adminKey := os.Getenv("SWARM_SMOKE_ADMIN")
	if adminKey == "" {
		adminKey = os.Getenv("LETTUCE_ADMIN_API_KEY")
	}
	if grpcAddr == "" || httpURL == "" || adminKey == "" {
		t.Skip("SWARM_SMOKE_GRPC / SWARM_SMOKE_HTTP / SWARM_SMOKE_ADMIN not set; skipping live smoke")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	base := func(profile string) *options {
		return &options{
			headGRPC:      grpcAddr,
			headHTTP:      httpURL,
			adminKey:      adminKey,
			creatorID:     os.Getenv("SWARM_SIM_CREATOR_ID"),
			volunteers:    20,
			profile:       profile,
			duration:      5 * time.Second,
			seedLeaf:      "swarm-smoke",
			seedUnits:     50,
			naiveInterval: 200 * time.Millisecond,
			bufferHours:   2.0,
			maxAssign:     8,
			simFpops:      1.0e10, // fast pretend-compute so units flow in 5s
			maxCompute:    100 * time.Millisecond,
			report:        "text",
			quiet:         true,
		}
	}

	ctx := context.Background()

	naiveRep, err := runSimulation(ctx, base("naive"), logger)
	if err != nil {
		t.Fatalf("naive run failed: %v", err)
	}
	if naiveRep.Volunteers != 20 {
		t.Fatalf("naive: registered %d volunteers, want 20", naiveRep.Volunteers)
	}

	// Let the per-client rate-limit window drain between profile runs so the
	// buffered fleet's registration burst is not throttled by the naive run's
	// residual per-IP budget (both runs share the loopback source IP).
	time.Sleep(8 * time.Second)

	bufRep, err := runSimulation(ctx, base("buffered"), logger)
	if err != nil {
		t.Fatalf("buffered run failed: %v", err)
	}
	if bufRep.Volunteers != 20 {
		t.Fatalf("buffered: registered %d volunteers, want 20", bufRep.Volunteers)
	}

	naiveReqs := rpcCalls(naiveRep, "RequestWorkUnit")
	bufReqs := rpcCalls(bufRep, "RequestWorkUnit")

	// (b) some assignments dispatched across the two runs.
	if naiveRep.AssignmentsDispatched == 0 && bufRep.AssignmentsDispatched == 0 {
		t.Fatalf("no assignments dispatched in either profile (naive=%d buffered=%d)",
			naiveRep.AssignmentsDispatched, bufRep.AssignmentsDispatched)
	}

	// (c) buffered makes strictly fewer RequestWorkUnit calls than naive.
	if bufReqs >= naiveReqs {
		t.Fatalf("buffered RequestWorkUnit count (%d) not strictly lower than naive (%d)", bufReqs, naiveReqs)
	}

	t.Logf("smoke OK: naive RequestWorkUnit=%d buffered=%d; dispatched naive=%d buffered=%d",
		naiveReqs, bufReqs, naiveRep.AssignmentsDispatched, bufRep.AssignmentsDispatched)
}

func rpcCalls(rep report, name string) int64 {
	for _, r := range rep.RPCs {
		if r.RPC == name {
			return r.Calls
		}
	}
	return 0
}
