package management

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"gopkg.in/yaml.v3"
)

// The per-leaf CPU override, the CPU time limit and the task restart through
// the management API — what the app and the CLI use. Pre-fix the config
// update dropped the override keys and max_cpu_time_pct without a word, and
// there was no restart route.

// TestLeafOverride_IsSavedAndAppliedLive: a PUT carrying a head's cores and
// max_running maps is saved to config.yaml, applied to the running daemon at
// once, and reported on the leaf by GET /api/v1/heads with what it comes to:
// GREP (2–4 cores) set to 3 runs at 3, one at a time.
func TestLeafOverride_IsSavedAndAppliedLive(t *testing.T) {
	env := setupTestEnv(t)
	env.daemon.GetLeafCache().PopulateForTest("test-server", &daemon.CachedHeadInfo{Name: "test-server", Leafs: []daemon.CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
			ResourceRequirements: &daemon.CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
	}})

	resp := env.doRequest(t, "PUT", "/api/v1/config",
		`{"servers":[{"name":"test-server","leaf_preferences":{"cores":{"grep":3},"max_running":{"grep":1}}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/v1/config: %d %v", resp.StatusCode, decodeJSON(t, resp))
	}
	resp.Body.Close()

	servers, _ := savedYAML(t, env.cfgPath)["servers"].([]any)
	if len(servers) == 0 {
		t.Fatal("no servers in the saved config")
	}
	server, _ := servers[0].(map[string]any)
	lp, _ := server["leaf_preferences"].(map[string]any)
	cores, _ := lp["cores"].(map[string]any)
	maxRunning, _ := lp["max_running"].(map[string]any)
	if cores["grep"] != 3 || maxRunning["grep"] != 1 {
		t.Errorf("saved leaf_preferences cores=%v max_running=%v, want grep 3 and 1", cores, maxRunning)
	}

	heads := getHeads(t, env)
	cpu := leafCPU(t, heads, "grep")
	if cpu == nil {
		t.Fatal("GET /api/v1/heads carries no cpu arrangement for the leaf")
	}
	want := map[string]float64{"cores_override": 3, "max_running_override": 1, "task_cores_min": 3, "task_cores_max": 3, "runs_at_once": 1}
	for k, v := range want {
		if cpu[k] != v {
			t.Errorf("heads leaf cpu.%s = %v, want %v (all: %v)", k, cpu[k], v, cpu)
		}
	}

	// An empty map returns the leaf to its own figures, as for weights.
	resp = env.doRequest(t, "PUT", "/api/v1/config", `{"servers":[{"name":"test-server","leaf_preferences":{"cores":{},"max_running":{"grep":0}}}]}`)
	resp.Body.Close()
	cpu = leafCPU(t, getHeads(t, env), "grep")
	if cpu["cores_override"] != float64(0) || cpu["max_running_override"] != float64(0) || cpu["task_cores_max"] != float64(4) {
		t.Errorf("after clearing: %v, want no overrides and the leaf's own 2–4", cpu)
	}
}

// TestCPUTimeLimit_IsSavedAndReported: max_cpu_time_pct is saved, and while
// it is below 100 the status names the limit and is not "paused".
func TestCPUTimeLimit_IsSavedAndReported(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "PUT", "/api/v1/config", `{"resource_limits":{"max_cpu_time_pct":50}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/v1/config: %d %v", resp.StatusCode, decodeJSON(t, resp))
	}
	resp.Body.Close()
	limits, _ := savedYAML(t, env.cfgPath)["resource_limits"].(map[string]any)
	if limits["max_cpu_time_pct"] != 50 {
		t.Errorf("saved max_cpu_time_pct = %v, want 50", limits["max_cpu_time_pct"])
	}
	status := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/status", ""))
	limit, ok := status["cpu_time_limit"].(map[string]any)
	if !ok {
		t.Fatalf("status carries no cpu_time_limit: %v", status)
	}
	if limit["pct"] != float64(50) || limit["run_seconds"] != float64(5) || limit["period_seconds"] != float64(10) {
		t.Errorf("cpu_time_limit = %v, want 50 %%, 5 s of every 10 s", limit)
	}

	resp = env.doRequest(t, "PUT", "/api/v1/config", `{"resource_limits":{"max_cpu_time_pct":3}}`)
	if resp.StatusCode == http.StatusOK {
		t.Error("max_cpu_time_pct 3 was accepted; below 5 a task would run less time than pausing it takes")
	}
	resp.Body.Close()
}

// TestRestartTask_Endpoint: POST /api/v1/tasks/{id}/restart restarts a
// running task (200, "restarting") and answers 404 for one that is not
// running.
func TestRestartTask_Endpoint(t *testing.T) {
	env, wuID, blockCh := setupTestEnvWithActiveTask(t)
	defer func() {
		select {
		case <-blockCh:
		default:
			close(blockCh)
		}
	}()
	var resp *http.Response
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		resp = env.doRequest(t, "POST", "/api/v1/tasks/"+wuID+"/restart", "")
		if resp.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			break
		}
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("restart of the running task answered %d, want 200", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["status"] != "restarting" {
		t.Errorf("restart of the running task: %v, want status restarting", body)
	}

	resp = env.doRequest(t, "POST", "/api/v1/tasks/wu-does-not-exist/restart", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("restart of an unknown task: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// savedYAML reads the config file the daemon saved, as plain YAML.
func savedYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// getHeads reads GET /api/v1/heads.
func getHeads(t *testing.T, env *testEnv) []any {
	t.Helper()
	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/heads: %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	heads, _ := body["heads"].([]any)
	return heads
}

// leafCPU finds a leaf's cpu object in a heads response; nil when absent.
func leafCPU(t *testing.T, heads []any, slug string) map[string]any {
	t.Helper()
	for _, h := range heads {
		leafs, _ := h.(map[string]any)["leafs"].([]any)
		for _, l := range leafs {
			lm := l.(map[string]any)
			if lm["slug"] == slug {
				cpu, _ := lm["cpu"].(map[string]any)
				return cpu
			}
		}
	}
	t.Fatalf("no leaf %q in the heads response", slug)
	return nil
}
