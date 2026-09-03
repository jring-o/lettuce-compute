package management

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

func tb46Daemon(t *testing.T) (*daemon.Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: "localhost:50051", Name: "head-alpha"}}
	return daemon.NewDaemon(daemon.DaemonConfig{Config: cfg, Logger: logger}), dir
}

// TestTB46_HistoryAPIPrefersRecordedLeafName: a history line that carries the leaf
// name the daemon knew at completion must be served under that name even when the
// live cache no longer knows the leaf (daemon restarted, head detached, leaf
// retired). Before TB-46 the API resolved the id against the cache alone and fell
// back to the UUID. The line is written raw so the test compiles against the
// pre-fix entry type and fails on the served name.
func TestTB46_HistoryAPIPrefersRecordedLeafName(t *testing.T) {
	d, dir := tb46Daemon(t)
	line := `{"work_unit_id":"unit-25","leaf_id":"4ba0f9cb-0d6e-4f7e-9d0e-1c2b3a4d5e6f",` +
		`"leaf_name":"Extract2 Student Crowd F14 GPU","server_name":"SciOS Compute",` +
		`"completed_at":"2026-08-10T10:25:00Z","wall_clock_seconds":400,"cpu_seconds":390,"result_accepted":true}`
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(line+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))
	resp := bridge.GetHistory("", 10, "", "", "")
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	if got := resp.Entries[0].LeafName; got != "Extract2 Student Crowd F14 GPU" {
		t.Errorf("leaf_name = %q; the name recorded at completion must win over the cache's id fallback (TB-46)", got)
	}

	// The local credit fallback names its per-leaf rows from the same record.
	summary := bridge.GetCredit()
	if len(summary.ByLeaf) != 1 || summary.ByLeaf[0].LeafName != "Extract2 Student Crowd F14 GPU" {
		t.Errorf("credit by_leaf = %+v; want the recorded leaf name", summary.ByLeaf)
	}
}
