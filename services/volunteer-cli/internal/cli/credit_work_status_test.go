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

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// stubCreditAPI serves body at /api/v1/credit and points the `credit` command at
// it through a daemon.json in a fresh data dir.
func stubCreditAPI(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/credit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
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

	prev := cfg
	t.Cleanup(func() { cfg = prev })
	cfg = config.Defaults()
	cfg.DataDir = dataDir
}

func runCreditForTest(t *testing.T) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runCredit(newCreditCmd(), nil) })
	if runErr != nil {
		t.Fatalf("credit: %v", runErr)
	}
	return out
}

// creditBody is a GET /api/v1/credit reply from a daemon attached to four heads:
// one that reports the account's results by state, one that reports none here,
// one too old to report them, and one unreachable.
func creditBody(t *testing.T) string {
	t.Helper()
	head := func(name string, available bool, ws any) map[string]any {
		return map[string]any{"head_name": name, "volunteer_id": "vol-1", "total_credit": 10, "available": available, "work_status": ws}
	}
	body := map[string]any{
		"total_credit": 10, "today": 0, "this_week": 0, "this_month": 0,
		"source": "head", "day_boundary": "utc",
		"by_leaf": []any{map[string]any{"leaf_id": "leaf-gpu", "leaf_name": "GPU leaf", "credit": 10}},
		"by_head": []any{
			head("scios", true, map[string]any{"by_leaf": []any{
				map[string]any{
					"leaf_id": "leaf-gpu", "leaf_name": "GPU leaf",
					"results_pending": 523, "results_agreed": 10, "results_disagreed": 1,
					"results_awaiting_content_verification": 0, "results_content_verification_failed": 0,
					"results_superseded": 4, "runs_stopped": 5, "copies_running": 2, "copies_waiting_to_start": 3,
				},
				map[string]any{
					"leaf_id": "leaf-idle", "leaf_name": "Idle leaf",
					"results_agreed": 7,
				},
			}}),
			head("new-head", true, map[string]any{"by_leaf": []any{}}),
			head("old-head", true, nil),
			head("down-head", false, nil),
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCreditPrintsResultsByStateAndCopiesInProgress: `credit` lists, per head and
// leaf, the account's results by state and its copies in progress in plain words,
// explains why results wait, and prints only the lines that have something in them.
func TestCreditPrintsResultsByStateAndCopiesInProgress(t *testing.T) {
	stubCreditAPI(t, creditBody(t))
	out := runCreditForTest(t)

	for _, want := range []string{
		"Your results by state",
		"scios", "GPU leaf",
		"waiting for validation", "523",
		"agreed (credited)",
		"did not agree",
		"not compared (the work unit was retired)",
		"stopped: enough results arrived while it ran",
		"2 running, 3 waiting to start",
		"Idle leaf",
		"a different account",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("credit output lacks %q:\n%s", want, out)
		}
	}
	// Zero counts stay off the page: the GPU leaf has no content-verification
	// results, and the idle leaf has nothing in progress.
	for _, unwanted := range []string{"checking the uploaded output", "could not be checked", "0 running"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("credit output prints a zero line %q:\n%s", unwanted, out)
		}
	}
}

// TestCreditSaysWhenAHeadDoesNotReportResultsByState: a head too old to send the
// figures is named as not reporting them, never shown as zeros; a head that
// reports nothing here says so; an unreachable head is left to the table above.
func TestCreditSaysWhenAHeadDoesNotReportResultsByState(t *testing.T) {
	stubCreditAPI(t, creditBody(t))
	out := runCreditForTest(t)

	start := strings.Index(out, "Your results by state")
	if start < 0 {
		t.Fatalf("credit prints no results-by-state section:\n%s", out)
	}
	section := out[start:]
	oldAt := strings.Index(section, "old-head")
	if oldAt < 0 {
		t.Fatalf("the results section does not name old-head:\n%s", out)
	}
	if !strings.Contains(section[oldAt:], "not reported by this head") {
		t.Errorf("old-head must say its figures are not reported:\n%s", out)
	}
	newAt := strings.Index(section, "new-head")
	if newAt < 0 || !strings.Contains(section[newAt:oldAt], "no results or copies on this head yet") {
		t.Errorf("new-head must say it has nothing here yet:\n%s", out)
	}
	if strings.Contains(section, "down-head") {
		t.Errorf("an unreachable head must not appear in the results section:\n%s", out)
	}
}

// TestCreditLocalEstimatePrintsNoResultsByState: with no head reachable there are
// no account-wide figures, so the section is left out rather than printed empty.
func TestCreditLocalEstimatePrintsNoResultsByState(t *testing.T) {
	stubCreditAPI(t, `{"total_credit":2,"source":"local","day_boundary":"local","by_head":[],"by_leaf":[]}`)
	out := runCreditForTest(t)
	if strings.Contains(out, "Your results by state") {
		t.Errorf("a local estimate printed a results-by-state section:\n%s", out)
	}
}
