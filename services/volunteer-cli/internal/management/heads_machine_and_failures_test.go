package management

import (
	"net/http"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// Plumbing regressions for TB-4 and TB-10.
//
// The data both bugs needed already existed inside the daemon — each leaf's
// runtime is in its execution spec, and the daemon necessarily knows which
// runtimes it registered — but the management API never carried it, so
// `leafs list` had nothing to render and printed a table that looked like it
// answered "will my machine run this?" when it did not.

// TestHeadsCarriesTheMachinesOwnCapabilities: `leafs list` must be able to ask
// the RUNNING daemon what it can do, rather than re-deriving it from a config
// file that may not be the one the daemon loaded (and re-running hardware
// detection to do it).
func TestHeadsCarriesTheMachinesOwnCapabilities(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)

	machine, ok := body["machine"].(map[string]any)
	if !ok {
		t.Fatal("GET /heads carries no machine block, so a client cannot tell which leafs this machine could ever run")
	}
	if _, ok := machine["runtimes"].([]any); !ok {
		t.Errorf("machine block has no runtimes list: %+v", machine)
	}
	if _, ok := machine["max_memory_mb"]; !ok {
		t.Errorf("machine block has no memory ceiling: %+v", machine)
	}
	if _, ok := machine["has_gpu"]; !ok {
		t.Errorf("machine block does not report GPU presence: %+v", machine)
	}
}

// TestHeadsCarriesTheGRPCAddress is a GUARD, not a regression test — it passes
// against the pre-fix code too, because HeadInfo has always carried the address.
// It is here because `leafs list` now DEPENDS on that field to match a head to
// its per-head runtime trust: a head's display NAME comes from the head itself
// and need not equal the local config's name for it, so dropping the address
// would silently make every leaf report "trust could not be checked".
func TestHeadsCarriesTheGRPCAddress(t *testing.T) {
	env := setupTestEnv(t)

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
	head := body["heads"].([]any)[0].(map[string]any)
	if head["grpc_address"] != "localhost:50051" {
		t.Errorf("head grpc_address = %v, want localhost:50051 — without it a renamed head cannot be matched to its trust settings", head["grpc_address"])
	}
}

// TestStatusReportsFailingLeafs: the per-leaf failure record has to reach the
// API, or `status` stays as silent as it was during the incident.
func TestStatusReportsFailingLeafs(t *testing.T) {
	env := setupTestEnv(t)

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/status", ""))
	failing, ok := body["failing_leafs"].([]any)
	if !ok {
		t.Fatal("GET /status has no failing_leafs field")
	}
	if len(failing) != 0 {
		t.Errorf("a fresh daemon reports %d failing leafs, want 0", len(failing))
	}
}

// TestHeadsAttachesFailureRecordsToTheirLeafs: the record has to arrive on the
// leaf row itself, so `leafs list` can mark the failing leaf rather than making
// the volunteer cross-reference two commands.
func TestHeadsAttachesFailureRecordsToTheirLeafs(t *testing.T) {
	env := setupTestEnv(t)

	env.daemon.GetLeafCache().PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Test Head",
		Leafs: []daemon.CachedLeafInfo{
			{ID: "leaf-broken", Slug: "broken", Name: "Broken", State: "ACTIVE"},
			{ID: "leaf-fine", Slug: "fine", Name: "Fine", State: "ACTIVE"},
		},
		DefaultWeights: map[string]int{"broken": 100, "fine": 100},
	})

	// Record failures through the daemon's own path, so this test breaks if the
	// recording path stops reaching the API.
	env.daemon.RecordLeafFailureForTest("leaf-broken", "non-zero exit code 2")
	env.daemon.RecordLeafFailureForTest("leaf-broken", "non-zero exit code 2")

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
	leafs := body["heads"].([]any)[0].(map[string]any)["leafs"].([]any)

	byslug := map[string]map[string]any{}
	for _, l := range leafs {
		m := l.(map[string]any)
		byslug[m["slug"].(string)] = m
	}

	broken, ok := byslug["broken"]["failures"].(map[string]any)
	if !ok {
		t.Fatalf("the failing leaf carries no failure record: %+v", byslug["broken"])
	}
	if int(broken["total_failures"].(float64)) != 2 {
		t.Errorf("total_failures = %v, want 2", broken["total_failures"])
	}
	if _, present := byslug["fine"]["failures"]; present {
		t.Errorf("a leaf that has never failed must carry no failure record: %+v", byslug["fine"])
	}
}
