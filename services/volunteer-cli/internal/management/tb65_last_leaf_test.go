package management

import (
	"net/http"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// Regression test for TB-65 at the management API, the surface the desktop
// app uses. Unchecking a head's last enabled leaf sends
// `{"mode":"SPECIFIC","enabled":[]}` for that head; the PUT used to fail
// validation with "SPECIFIC mode requires at least one enabled leaf" and the
// app rolled the checkbox back. The PUT must succeed, persist, and the heads
// listing must show every leaf of that head disabled — the state the app
// then renders after a reload.
func TestTB65_UpdateConfigSpecificWithNoLeavesIsAcceptedAndDisablesEveryLeaf(t *testing.T) {
	env := setupTestEnv(t)

	env.daemon.GetLeafCache().PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Test Head",
		Leafs: []daemon.CachedLeafInfo{
			{ID: "l1", Slug: "grep-f13", Name: "GREP f13", State: "ACTIVE"},
			{ID: "l2", Slug: "beyblade", Name: "Beyblade Arena", State: "ACTIVE"},
		},
		DefaultWeights: map[string]int{"grep-f13": 100, "beyblade": 100},
	})

	// The app's earlier write: one leaf left.
	resp, body := putServers(t, env, `{"servers":[{"name":"test-server","leaf_preferences":{"mode":"SPECIFIC","enabled":["beyblade"]}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("one enabled leaf: expected 200, got %d: %v", resp.StatusCode, body)
	}

	// Unchecking that last leaf.
	resp, body = putServers(t, env, `{"servers":[{"name":"test-server","leaf_preferences":{"mode":"SPECIFIC","enabled":[]}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("last leaf unchecked: expected 200, got %d: %v (the daemon must accept SPECIFIC with no enabled leaf)", resp.StatusCode, body)
	}
	if body["restart_required"] == true {
		t.Errorf("restart_required = true; leaf preferences are read live")
	}

	loaded, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	lp := loaded.Servers[0].LeafPreferences
	if lp.Mode != "SPECIFIC" || len(lp.Enabled) != 0 {
		t.Fatalf("on-disk leaf_preferences = %+v, want SPECIFIC with no enabled leaf", lp)
	}

	// What the app renders after a reload: every box unchecked.
	resp = env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET heads: expected 200, got %d", resp.StatusCode)
	}
	heads := decodeJSON(t, resp)["heads"].([]any)
	leafs := heads[0].(map[string]any)["leafs"].([]any)
	if len(leafs) != 2 {
		t.Fatalf("expected 2 leafs in the listing, got %d", len(leafs))
	}
	for _, raw := range leafs {
		leaf := raw.(map[string]any)
		if leaf["enabled"] != false {
			t.Errorf("leaf %v enabled = %v, want false", leaf["slug"], leaf["enabled"])
		}
	}

	// The daemon's own view: nothing to fetch from this head.
	if got := env.daemon.GetLeafCache(); got == nil {
		t.Fatal("leaf cache missing")
	}
	for _, h := range env.bridge.GetHeads() {
		for _, l := range h.Leafs {
			if l.Enabled {
				t.Errorf("bridge reports %s enabled under SPECIFIC with no leaf", l.Slug)
			}
		}
	}

	// And a later, unrelated save must not be refused because of it — the
	// trap after a CLI `leafs disable` of the last leaf, which wrote this
	// state without validating.
	resp, body = putServers(t, env, `{"work_buffer_hours": 3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("later save: expected 200, got %d: %v", resp.StatusCode, body)
	}
}
