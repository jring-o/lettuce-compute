package management

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TB-41 plumbing: GET /api/v1/heads must carry the daemon's live per-leaf
// disk-gate verdict, or `leafs list` has nothing to quote and falls back to
// the classifier arithmetic that contradicted the fetcher on the tester's
// host. Same reasoning as TB-4's machine block: the data exists only inside
// the running daemon.
func TestTB41_HeadsCarriesPerLeafDiskGate(t *testing.T) {
	env := setupTestEnv(t)

	env.daemon.GetLeafCache().PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Test Head",
		Leafs: []daemon.CachedLeafInfo{
			{ID: "leaf-1", Slug: "grep-cpu", Name: "GREP CPU", State: "ACTIVE"},
		},
		DefaultWeights: map[string]int{"grep-cpu": 100},
	})

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
	leaf := body["heads"].([]any)[0].(map[string]any)["leafs"].([]any)[0].(map[string]any)

	gate, ok := leaf["disk_gate"].(map[string]any)
	if !ok {
		t.Fatalf("the leaf carries no disk_gate verdict, so `leafs list` cannot quote the live gate (TB-41): %+v", leaf)
	}
	if _, ok := gate["blocked"].(bool); !ok {
		t.Errorf("disk_gate has no blocked verdict: %+v", gate)
	}
	// The test env's data dir is a fresh temp dir under the default 10 GB
	// allowance, so this leaf (2,048 MB fallback need) is not gated — and the
	// covering allowance for it is 2 GB.
	if gate["blocked"] == true {
		t.Errorf("a fresh test env must not be disk-gated: %+v", gate)
	}
	if got := gate["raise_to_gb"]; got != float64(2) {
		t.Errorf("raise_to_gb = %v, want 2 (the 2,048 MB fallback need, no usage)", got)
	}
}
