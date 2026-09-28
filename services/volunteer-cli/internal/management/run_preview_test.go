package management

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TestRunPreview_Endpoint: GET /api/v1/run-preview answers what would run
// together under the current settings — on 4 cores with GREP (2–4) and
// Beyblade (1): each alone, and together GREP at 2 cores beside a Beyblade,
// the next GREP waiting for its 2 cores.
func TestRunPreview_Endpoint(t *testing.T) {
	env := setupTestEnv(t)
	// The daemon previews the heads it is connected to, as its fetcher asks them.
	env.daemon.SetMultiClientForTest(daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{Name: "test-server", VolunteerID: "vol-1", Available: true},
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	env.daemon.GetLeafCache().PopulateForTest("test-server", &daemon.CachedHeadInfo{Name: "test-server", Leafs: []daemon.CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
			ResourceRequirements: &daemon.CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
		{ID: "leaf-bb", Slug: "beyblade", Name: "Beyblade", State: "ACTIVE",
			ResourceRequirements: &daemon.CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}},
	}})
	resp := env.doRequest(t, "PUT", "/api/v1/config", `{"resource_limits":{"max_cpu_cores":4,"max_memory_mb":8192}}`)
	resp.Body.Close()

	resp = env.doRequest(t, "GET", "/api/v1/run-preview", "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /api/v1/run-preview answered %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["cpu_limit"] != float64(4) || body["together_cores"] != float64(3) {
		t.Errorf("cpu_limit / together_cores = %v / %v, want 4 / 3", body["cpu_limit"], body["together_cores"])
	}
	together, _ := body["together"].([]any)
	if len(together) != 2 {
		t.Fatalf("together = %v, want GREP at 2 cores and a Beyblade at 1", together)
	}
	first := together[0].(map[string]any)
	if first["leaf_name"] != "GREP" || first["cores"] != float64(2) {
		t.Errorf("first task = %v, want GREP at 2 cores", first)
	}
	if w, _ := body["waiting_for_cores"].(map[string]any); w == nil || w["leaf_name"] != "GREP" {
		t.Errorf("waiting_for_cores = %v, want the next GREP task", body["waiting_for_cores"])
	}
	alone, _ := body["alone"].([]any)
	if len(alone) != 2 {
		t.Errorf("alone = %v, want both leafs", alone)
	}

	// Each leaf in the heads response carries the deadline verdict.
	heads := getHeads(t, env)
	leafs, _ := heads[0].(map[string]any)["leafs"].([]any)
	for _, l := range leafs {
		if dl, _ := l.(map[string]any)["deadline"].(map[string]any); dl == nil || dl["blocked"] != false {
			t.Errorf("leaf %v: deadline verdict = %v, want present and not blocked (no runs here yet)", l.(map[string]any)["slug"], dl)
		}
	}
}
