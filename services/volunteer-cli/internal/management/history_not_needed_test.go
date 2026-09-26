package management

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// A run whose result the head did not need (the unit was already finalized) is
// recorded with "outcome":"not_needed" and result_accepted false. The history
// route must serve it as its own status, not as "rejected", and the local
// credit fallback must keep ignoring it. The lines are written raw so the test
// runs unchanged against the code before the outcome field existed.
func TestHistoryAPIServesNotNeededAsItsOwnStatus(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: "localhost:50051", Name: "head-alpha"}}
	d := daemon.NewDaemon(daemon.DaemonConfig{Config: cfg, Logger: logger})

	// The local credit fallback applies when the head cannot answer.
	d.SetMultiClientForTest(daemon.NewMultiServerClient([]*daemon.ServerConnection{{
		Name: "head-alpha", Available: true,
		Client: &e2eMockWorkClient{
			getMyContributionFn: func(context.Context, *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
				return nil, fmt.Errorf("rpc error: code = Unimplemented")
			},
		},
	}}, logger))

	lines := `{"work_unit_id":"unit-accepted","leaf_id":"leaf-a","server_name":"head-alpha","completed_at":"2026-09-25T10:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":true}` + "\n" +
		`{"work_unit_id":"unit-rejected","leaf_id":"leaf-a","server_name":"head-alpha","completed_at":"2026-09-25T11:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":false}` + "\n" +
		`{"work_unit_id":"unit-not-needed","leaf_id":"leaf-a","server_name":"head-alpha","completed_at":"2026-09-25T12:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":false,"outcome":"not_needed"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}

	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))
	resp := bridge.GetHistory("", 10, "", "", "")
	got := make(map[string]string)
	for _, e := range resp.Entries {
		got[e.WorkUnitID] = e.ValidationStatus
	}
	want := map[string]string{
		"unit-accepted":   "accepted",
		"unit-rejected":   "rejected",
		"unit-not-needed": "not_needed",
	}
	for unit, status := range want {
		if got[unit] != status {
			t.Errorf("%s: validation_status = %q, want %q", unit, got[unit], status)
		}
	}

	summary := bridge.GetCredit()
	if summary.Source != "local" {
		t.Fatalf("credit source = %q, want the local fallback", summary.Source)
	}
	if summary.TotalCredit != 1 {
		t.Errorf("local credit = %v, want 1: only the accepted entry counts", summary.TotalCredit)
	}
}
