package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for TB-10's visibility half, at the command a volunteer
// actually runs. The tester's own account of the incident was "my host with
// C 200 - N 100 has not fetched a single Native task yet (completed 12 WU)" —
// `status` showed the 12 completed container units and said nothing at all
// about the seventeen native units that had been fetched and failed.
//
// These drive the real `status` rendering path (printActiveTasks) against a
// stub management API, so they cover the call site and not just the helper.

// stubStatusAPI serves body at /api/v1/status and writes the daemon.json that
// points `status` at it, returning the data dir to pass in.
func stubStatusAPI(t *testing.T, body map[string]any) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	dataDir := t.TempDir()
	info := fmt.Sprintf(`{"port":%d,"token":"test-token","pid":1,"started_at":""}`, port)
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte(info), 0o600); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
	return dataDir
}

func TestStatusReportsLeafsWhoseWorkIsFailingHere(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []any{},
		"queued_tasks": []any{},
		"failing_leafs": []any{map[string]any{
			"leaf_name":            "BB-A-native",
			"consecutive_failures": 3,
			"total_failures":       17,
			"last_reason":          "non-zero exit code 2",
			"paused":               true,
		}},
	})

	out := captureStdout(t, func() { printActiveTasks(dataDir) })

	for _, want := range []string{"BB-A-native", "17", "non-zero exit code 2", "paused"} {
		if !strings.Contains(out, want) {
			t.Errorf("`status` does not mention %q — the volunteer still cannot see that work is arriving and failing:\n%s", want, out)
		}
	}
	// The headline has to correct the volunteer's actual misreading — that no
	// work of that kind is arriving — not merely list a count.
	if !strings.Contains(out, "arriving") {
		t.Errorf("`status` does not say the work IS arriving, which is the whole misunderstanding:\n%s", out)
	}
}

// TestStatusSaysNothingWhenNothingIsFailing: a healthy volunteer's status must
// be what it always was. A diagnostic that prints on every run is one testers
// learn to skip.
func TestStatusSaysNothingWhenNothingIsFailing(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks":  []any{},
		"queued_tasks":  []any{},
		"failing_leafs": []any{},
	})

	out := captureStdout(t, func() { printActiveTasks(dataDir) })
	if strings.Contains(out, "Failing leafs") {
		t.Errorf("status printed a failing-leafs section with no failures:\n%s", out)
	}
	if !strings.Contains(out, "Active tasks: none") {
		t.Errorf("status lost its normal output:\n%s", out)
	}
}

// TestStatusDistinguishesPausedFromStillRetrying: a leaf the breaker has stopped
// requesting and one that has failed but is still being tried call for different
// reactions, so they must not render the same.
func TestStatusDistinguishesPausedFromStillRetrying(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []any{},
		"queued_tasks": []any{},
		"failing_leafs": []any{
			map[string]any{"leaf_name": "paused-leaf", "consecutive_failures": 3, "total_failures": 3, "paused": true},
			map[string]any{"leaf_name": "flaky-leaf", "consecutive_failures": 1, "total_failures": 4, "paused": false},
		},
	})

	out := captureStdout(t, func() { printActiveTasks(dataDir) })

	var pausedRow, flakyRow string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "paused-leaf"):
			pausedRow = line
		case strings.Contains(line, "flaky-leaf"):
			flakyRow = line
		}
	}
	if !strings.Contains(pausedRow, "paused") {
		t.Errorf("paused leaf row does not say paused: %q\n%s", pausedRow, out)
	}
	if !strings.Contains(flakyRow, "retrying") {
		t.Errorf("a leaf still being retried should say so: %q\n%s", flakyRow, out)
	}
}

// TestStatusKeepsAFailureReasonToOneCell: the reason can carry a tail of the
// failing program's output. That belongs in the log; a table cell that wraps
// destroys the table.
func TestStatusKeepsAFailureReasonToOneCell(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []any{},
		"queued_tasks": []any{},
		"failing_leafs": []any{map[string]any{
			"leaf_name":      "l",
			"total_failures": 1,
			"last_reason":    "non-zero exit code 2; output: " + strings.Repeat("noise ", 200),
		}},
	})

	out := captureStdout(t, func() { printActiveTasks(dataDir) })
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 200 {
			t.Errorf("status emitted a %d-character line; the reason was not truncated:\n%s", len(line), line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Errorf("a truncated reason should be marked as truncated:\n%s", out)
	}
}
