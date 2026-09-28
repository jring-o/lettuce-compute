package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"gopkg.in/yaml.v3"
)

// The CLI side of the per-leaf CPU override, the CPU time limit and the task
// restart: `leafs cores`, `leafs max-running`, `tasks restart`, and what
// `status`, `leafs list` and `doctor` say about them.

// overrideConfig writes a config with one head, "lettuce.science", and
// returns its path and data dir (no daemon.json: no daemon runs).
func overrideConfig(t *testing.T) (cfgFile, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	c := config.Defaults()
	c.DataDir = dataDir
	c.Servers = []config.ServerConfig{{Name: "lettuce.science", GRPCAddress: "lettuce.science:443"}}
	cfgFile = filepath.Join(dataDir, "config.yaml")
	if err := c.Save(cfgFile); err != nil {
		t.Fatal(err)
	}
	return cfgFile, dataDir
}

// TestLeafsCoresAndMaxRunningAreSaved: with no daemon running, the two
// commands save the head's override maps to config.yaml, and "default" /
// "none" remove the entries again.
func TestLeafsCoresAndMaxRunningAreSaved(t *testing.T) {
	cfgFile, dataDir := overrideConfig(t)
	var err error
	out := captureStdout(t, func() {
		err = runCLI(t, "leafs", "cores", "grep", "3", "--config", cfgFile, "--data-dir", dataDir)
	})
	if err != nil {
		t.Fatalf("leafs cores: %v", err)
	}
	if !strings.Contains(out, "takes effect when the daemon starts") {
		t.Errorf("with no daemon the command should say the change waits for the next start:\n%s", out)
	}
	if err := runCLI(t, "leafs", "max-running", "grep", "1", "--config", cfgFile, "--data-dir", dataDir); err != nil {
		t.Fatalf("leafs max-running: %v", err)
	}
	cores, maxRunning := savedOverrides(t, cfgFile)
	if cores["grep"] != 3 || maxRunning["grep"] != 1 {
		t.Fatalf("saved cores=%v max_running=%v, want grep 3 and 1", cores, maxRunning)
	}
	raw, _ := os.ReadFile(cfgFile)
	if !strings.Contains(string(raw), "cores each of the leaf's tasks is given") {
		t.Errorf("the saved file does not explain the cores key:\n%s", raw)
	}

	if err := runCLI(t, "leafs", "cores", "grep", "default", "--config", cfgFile, "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(t, "leafs", "max-running", "grep", "none", "--config", cfgFile, "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	if cores, maxRunning := savedOverrides(t, cfgFile); len(cores) != 0 || len(maxRunning) != 0 {
		t.Errorf("after default/none: cores=%v max_running=%v, want both empty", cores, maxRunning)
	}
	if err := runCLI(t, "leafs", "cores", "grep", "two", "--config", cfgFile, "--data-dir", dataDir); err == nil {
		t.Error("`leafs cores grep two` was accepted")
	}
}

// savedOverrides reads the first head's cores and max_running maps from a
// saved config file, as plain YAML.
func savedOverrides(t *testing.T, path string) (cores, maxRunning map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers []struct {
			LeafPreferences map[string]any `yaml:"leaf_preferences"`
		} `yaml:"servers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) == 0 {
		t.Fatal("no servers in the saved config")
	}
	cores, _ = doc.Servers[0].LeafPreferences["cores"].(map[string]any)
	maxRunning, _ = doc.Servers[0].LeafPreferences["max_running"].(map[string]any)
	for _, m := range []map[string]any{cores, maxRunning} {
		for k, v := range m {
			m[k] = toInt(v)
		}
	}
	return cores, maxRunning
}

// toInt reads a YAML number.
func toInt(v any) any {
	if n, ok := v.(int); ok {
		return n
	}
	return v
}

// stubManagement serves a fake daemon's management API from handlers keyed
// by "METHOD /path" and returns the data dir whose daemon.json points at it.
func stubManagement(t *testing.T, handlers map[string]http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	dataDir := t.TempDir()
	info := fmt.Sprintf(`{"port":%d,"token":"test-token","pid":1,"started_at":""}`, port)
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// TestLeafsCoresAppliesLiveThroughTheDaemon: with a daemon running, `leafs
// cores` sends the head's cores map to the daemon, which saves and applies
// it, and reports what it comes to there (the leaf's range keeps 6 at 4).
func TestLeafsCoresAppliesLiveThroughTheDaemon(t *testing.T) {
	var mu sync.Mutex
	var put map[string]any
	dataDir := stubManagement(t, map[string]http.HandlerFunc{
		"PUT /api/v1/config": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewDecoder(r.Body).Decode(&put)
			w.Write([]byte(`{}`))
		},
		"GET /api/v1/heads": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"heads":[{"name":"lettuce.science","grpc_address":"lettuce.science:443","leafs":[{"slug":"grep","cpu":{"cores_override":6,"task_cores_min":4,"task_cores_max":4,"runs_at_once":1}}]}]}`))
		},
	})
	c := config.Defaults()
	c.DataDir = dataDir
	c.Servers = []config.ServerConfig{{Name: "lettuce.science", GRPCAddress: "lettuce.science:443"}}
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := c.Save(cfgFile); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfgFile)

	var err error
	out := captureStdout(t, func() {
		err = runCLI(t, "leafs", "cores", "grep", "6", "--config", cfgFile, "--data-dir", dataDir)
	})
	if err != nil {
		t.Fatalf("leafs cores: %v", err)
	}
	mu.Lock()
	body, _ := json.Marshal(put)
	mu.Unlock()
	if !strings.Contains(string(body), `"cores":{"grep":6}`) || !strings.Contains(string(body), `"name":"lettuce.science"`) {
		t.Errorf("PUT body %s does not carry the head's cores map", body)
	}
	if after, _ := os.ReadFile(cfgFile); !bytes.Equal(before, after) {
		t.Error("the CLI rewrote config.yaml itself while a daemon runs; the daemon saves the change")
	}
	if !strings.Contains(out, "each grep task is given 4 cores") {
		t.Errorf("output does not say what the change comes to on this machine:\n%s", out)
	}
}

// TestTasksRestartFindsTheTaskByItsID: `tasks restart` takes the ID `status`
// shows (a unique prefix) and asks the daemon to restart that task.
func TestTasksRestartFindsTheTaskByItsID(t *testing.T) {
	var mu sync.Mutex
	var restarted []string
	dataDir := stubManagement(t, map[string]http.HandlerFunc{
		"GET /api/v1/status": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"active_tasks":[{"work_unit_id":"3f2a91c0-aaaa-4000-8000-000000000001","leaf_name":"GREP"},{"work_unit_id":"3f7b0000-bbbb-4000-8000-000000000002","leaf_name":"Beyblade"}]}`))
		},
		"POST /api/v1/tasks/3f2a91c0-aaaa-4000-8000-000000000001/restart": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			restarted = append(restarted, r.URL.Path)
			mu.Unlock()
			w.Write([]byte(`{"status":"restarting"}`))
		},
	})
	cfgFile := writeDefaultConfig(t, dataDir)

	if err := runCLI(t, "tasks", "restart", "3f", "--config", cfgFile, "--data-dir", dataDir); err == nil {
		t.Error("an id prefix matching two tasks was accepted")
	}
	var err error
	out := captureStdout(t, func() {
		err = runCLI(t, "tasks", "restart", "3f2a91c0", "--config", cfgFile, "--data-dir", dataDir)
	})
	if err != nil {
		t.Fatalf("tasks restart: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(restarted) != 1 {
		t.Fatalf("restart requests = %v, want one for the GREP task", restarted)
	}
	if !strings.Contains(out, "Restarting GREP task 3f2a91c0") {
		t.Errorf("output: %q", out)
	}
}

// TestStatusShowsTaskIDsAndTheCPUTimeLimit: `status` lists each running
// task's ID (what `tasks restart` takes), says a running task keeps the
// settings it started with, and names the CPU time limit when one is set.
func TestStatusShowsTaskIDsAndTheCPUTimeLimit(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []any{map[string]any{"work_unit_id": "3f2a91c0-aaaa-4000-8000-000000000001", "leaf_name": "GREP", "runtime_type": "container", "cpu_cores": 3, "task_status": "running"}},
		"queued_tasks": []any{},
		"cpu_time_limit": map[string]any{"pct": 50, "run_seconds": 5, "period_seconds": 10,
			"description": "Runs 50 % of the time (5 s of every 10 s): CPU time limit"},
	})
	out := captureStdout(t, func() { printActiveTasks(dataDir) })
	for _, want := range []string{"ID", "3f2a91c0", "CPU time: Runs 50 % of the time", "tasks restart <ID>"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

// TestLeafsListShowsCoresAndHowManyRunAtOnce: the leaf table has CORES and
// AT ONCE columns from the daemon, with the volunteer's own settings marked.
func TestLeafsListShowsCoresAndHowManyRunAtOnce(t *testing.T) {
	machine, servers := containerOnlyMachine()
	head := twoLeafHead()
	// The daemon's answer, as it arrives on the wire.
	cpu := `[{"cpu":{"cores_override":3,"task_cores_min":3,"task_cores_max":3,"runs_at_once":1,"max_running_override":1}},{"cpu":{"task_cores_min":2,"task_cores_max":4,"runs_at_once":2}}]`
	if err := json.Unmarshal([]byte(cpu), &head.Leafs); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printLeafsTable(&buf, &leafsAPIResponse{Heads: []leafsAPIHead{head}, Machine: machine}, servers)
	out := buf.String()
	header := strings.Split(out, "\n")[0]
	if !strings.Contains(header, "CORES") || !strings.Contains(header, "AT ONCE") {
		t.Fatalf("header %q lacks CORES / AT ONCE", header)
	}
	first := rowFor(out, head.Leafs[0].Slug)
	second := rowFor(out, head.Leafs[1].Slug)
	if !strings.Contains(first, " 3* ") || !strings.Contains(first, " 1* ") {
		t.Errorf("row for the overridden leaf should show 3* and 1*: %q", first)
	}
	if !strings.Contains(second, " 2-4 ") {
		t.Errorf("row for the other leaf should show its range 2-4: %q", second)
	}
	if !strings.Contains(out, "* set by you") {
		t.Errorf("no footnote for the starred figures:\n%s", out)
	}
}
