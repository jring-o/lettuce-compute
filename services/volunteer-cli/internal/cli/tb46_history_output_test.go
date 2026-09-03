package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
)

// Regression tests for TB-46: `history` is the one self-serve answer to "how many
// work units have I processed?" and it printed twenty rows with no total, the
// leaf as a truncated UUID, and an ACCEPTED column nobody could tell from credit.
//
// The history file is written raw (with the leaf_name key this fix records) so the
// tests compile against the pre-fix code and fail on what it prints.

const tb46LeafID = "4ba0f9cb-0d6e-4f7e-9d0e-1c2b3a4d5e6f"

// tb46WriteHistory writes n entries newest-last, one of them rejected by the head.
// withNames records the leaf name as a post-fix daemon does; without it the lines
// are the legacy shape (id only).
func tb46WriteHistory(t *testing.T, dir string, n int, withNames bool) {
	t.Helper()
	var sb strings.Builder
	for i := 0; i < n; i++ {
		name := ""
		if withNames {
			name = `"leaf_name":"Extract2 Student Crowd F14 GPU",`
		}
		fmt.Fprintf(&sb, `{"work_unit_id":"unit-%02d","leaf_id":"%s",%s"server_name":"SciOS Compute",`+
			`"completed_at":"2026-08-10T%02d:25:00Z","wall_clock_seconds":400,"cpu_seconds":390,"result_accepted":%v}`+"\n",
			i, tb46LeafID, name, i%24, i != 3)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func tb46Run(t *testing.T, dir string, limit int) string {
	t.Helper()
	prev := cfg
	t.Cleanup(func() { cfg = prev })
	cfg = config.Defaults()
	cfg.DataDir = dir

	cmd := newHistoryCmd()
	cmd.SetContext(context.Background())
	var runErr error
	out := captureStdout(t, func() { runErr = runHistory(cmd, limit) })
	if runErr != nil {
		t.Fatalf("history: %v", runErr)
	}
	return out
}

// TestTB46_HistoryNamesLeavesAndCountsTheWholeFile: 25 processed units, the
// default page of 20 — the LEAF column shows the recorded name, and the footer
// says 25 exist, how many the head accepted, that acceptance is not credit, and how
// to see the rest.
func TestTB46_HistoryNamesLeavesAndCountsTheWholeFile(t *testing.T) {
	dir := t.TempDir()
	tb46WriteHistory(t, dir, 25, true)

	out := tb46Run(t, dir, 20)

	if !strings.Contains(out, "Extract2 Student Crowd F14 GPU") {
		t.Errorf("LEAF must show the leaf's recorded name (TB-46); got:\n%s", out)
	}
	if strings.Contains(out, "4ba0f9cb-...") {
		t.Errorf("LEAF still shows a truncated UUID for a leaf whose name is recorded; got:\n%s", out)
	}
	if !strings.Contains(out, "Showing 20 of 25 completed units; the head accepted 24 on submission.") {
		t.Errorf("footer must count the whole file, not the page (TB-46); got:\n%s", out)
	}
	if !strings.Contains(out, "Head acceptance is not credit") {
		t.Errorf("footer must say acceptance is not credit — the question this command is asked (TB-46); got:\n%s", out)
	}
	if !strings.Contains(out, "--limit") {
		t.Errorf("a truncated page must name the flag that shows the rest; got:\n%s", out)
	}
	if !strings.Contains(out, "HEAD ACCEPTED") {
		t.Errorf("the column must say whose verdict it is; got:\n%s", out)
	}
	if rows := strings.Count(out, "unit-"); rows != 20 {
		t.Errorf("rows shown = %d, want the 20-row page", rows)
	}

	all := tb46Run(t, dir, 0)
	if !strings.Contains(all, "Showing 25 of 25 completed units") {
		t.Errorf("--limit 0 must show everything; got:\n%s", all)
	}
	if strings.Contains(all, "--limit") {
		t.Errorf("nothing is hidden, so no hint about --limit; got:\n%s", all)
	}
}

// TestTB46_HistoryLooksUpLegacyLeafIdsThroughTheDaemon: entries written before
// names were recorded carry only the UUID. With the daemon running, the command
// resolves them through the management API's heads listing; with it stopped, the
// row shows the id and the footer says why.
func TestTB46_HistoryLooksUpLegacyLeafIdsThroughTheDaemon(t *testing.T) {
	dir := t.TempDir()
	tb46WriteHistory(t, dir, 3, false)

	stopped := tb46Run(t, dir, 20)
	if !strings.Contains(stopped, "4ba0f9cb-...") {
		t.Errorf("with no daemon the id is all there is; got:\n%s", stopped)
	}
	if !strings.Contains(stopped, "shown by leaf id") {
		t.Errorf("unresolved ids must be explained in the footer; got:\n%s", stopped)
	}

	// A stand-in for the running daemon's management API, on 127.0.0.1 like the real one.
	const token = "tb46-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/heads" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unexpected request", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"heads":[{"name":"SciOS Compute","leafs":[{"id":"%s","name":"Extract2 Student Crowd F14 GPU"}]}]}`, tb46LeafID)
	}))
	defer srv.Close()
	info, _ := json.Marshal(management.DaemonInfo{
		Port: srv.Listener.Addr().(*net.TCPAddr).Port, Token: token, PID: os.Getpid(),
	})
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), info, 0600); err != nil {
		t.Fatal(err)
	}

	running := tb46Run(t, dir, 20)
	if !strings.Contains(running, "Extract2 Student Crowd F14 GPU") {
		t.Errorf("with the daemon running, legacy ids must be resolved to names (TB-46); got:\n%s", running)
	}
	if strings.Contains(running, "4ba0f9cb-...") || strings.Contains(running, "shown by leaf id") {
		t.Errorf("nothing should remain unresolved; got:\n%s", running)
	}
}
