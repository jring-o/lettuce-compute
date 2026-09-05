package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tb71History writes a history file whose newest 50 lines all belong to one
// leaf and whose older 10 lines belong to another — the shape on which the
// desktop app's leaf filter, built from the pages loaded so far, could not
// offer the older leaf until the reader had scrolled a row of it into view.
// The file is append-only (oldest first) and served newest first.
func tb71History(t *testing.T, dir string) {
	t.Helper()
	var sb strings.Builder
	base := time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)
	for i := 9; i >= 0; i-- {
		fmt.Fprintf(&sb, `{"work_unit_id":"grep-%02d","leaf_id":"leaf-grep","leaf_name":"GREP v1","server_name":"SciOS Compute",`+
			`"completed_at":%q,"wall_clock_seconds":3000,"cpu_seconds":2900,"result_accepted":true}`+"\n",
			i, base.Add(-time.Duration(100+i)*time.Minute).Format(time.RFC3339))
	}
	for i := 49; i >= 0; i-- {
		fmt.Fprintf(&sb, `{"work_unit_id":"bb-%02d","leaf_id":"leaf-bb","leaf_name":"Beyblade Arena","server_name":"lbry",`+
			`"completed_at":%q,"wall_clock_seconds":600,"cpu_seconds":590,"result_accepted":true}`+"\n",
			i, base.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339))
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestTB71_HistoryPageListsEveryLeafNameInTheFile: the first page of a
// 60-line history holds one leaf's rows only, yet the response names both
// leaves, so a filter built from it can select the older leaf at once. The
// list is a property of the whole file, not of the page, cursor, leaf or date
// window asked for.
func TestTB71_HistoryPageListsEveryLeafNameInTheFile(t *testing.T) {
	d, dir := tb46Daemon(t)
	tb71History(t, dir)
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	want := []string{"Beyblade Arena", "GREP v1"}

	first := bridge.GetHistory("", 50, "", "", "")
	if len(first.Entries) != 50 || !first.Pagination.HasMore {
		t.Fatalf("first page: %d entries, has_more=%v; want 50 with more", len(first.Entries), first.Pagination.HasMore)
	}
	for _, e := range first.Entries {
		if e.LeafName != "Beyblade Arena" {
			t.Fatalf("first page holds %q; the fixture puts only Beyblade Arena on it", e.LeafName)
		}
	}
	if !reflect.DeepEqual(first.LeafNames, want) {
		t.Errorf("leaf_names on the first page = %v, want %v (every leaf in the file, TB-71)", first.LeafNames, want)
	}

	// Neither a leaf filter nor a date window narrows the list: the filter is
	// what the list feeds, and a leaf outside the window is still a leaf.
	narrowed := bridge.GetHistory("", 50, "leaf-bb", "2026-09-05T07:00:00Z", "")
	if !reflect.DeepEqual(narrowed.LeafNames, want) {
		t.Errorf("leaf_names under a leaf+date filter = %v, want %v", narrowed.LeafNames, want)
	}

	// A cursor past the end still names the leaves.
	past := bridge.GetHistory("999", 50, "", "", "")
	if len(past.Entries) != 0 || !reflect.DeepEqual(past.LeafNames, want) {
		t.Errorf("past the end: %d entries, leaf_names = %v; want 0 and %v", len(past.Entries), past.LeafNames, want)
	}
}

// TestTB71_HistoryRouteCarriesLeafNames: the JSON the app reads has the list
// beside `entries` and `pagination`, and an empty history serves an empty
// array rather than null.
func TestTB71_HistoryRouteCarriesLeafNames(t *testing.T) {
	env := setupTestEnv(t)
	tb71History(t, env.dataDir)

	resp := env.doRequest(t, "GET", "/api/v1/history?limit=50", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Entries   []json.RawMessage `json:"entries"`
		LeafNames []string          `json:"leaf_names"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 50 {
		t.Fatalf("entries = %d, want 50", len(body.Entries))
	}
	if want := []string{"Beyblade Arena", "GREP v1"}; !reflect.DeepEqual(body.LeafNames, want) {
		t.Errorf("leaf_names = %v, want %v", body.LeafNames, want)
	}

	if err := os.Remove(filepath.Join(env.dataDir, "history.jsonl")); err != nil {
		t.Fatal(err)
	}
	resp = env.doRequest(t, "GET", "/api/v1/history", "")
	raw := decodeJSON(t, resp)
	names, ok := raw["leaf_names"].([]any)
	if !ok || len(names) != 0 {
		t.Errorf("leaf_names with no history = %v (%T), want an empty array", raw["leaf_names"], raw["leaf_names"])
	}
}
