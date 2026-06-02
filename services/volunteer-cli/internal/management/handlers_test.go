package management

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon/procmetrics"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// testEnv sets up a running management server and returns a cleanup function.
type testEnv struct {
	server  *Server
	bridge  *DaemonBridge
	daemon  *daemon.Daemon
	dataDir string
	cfgPath string
	baseURL string
	token   string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	return &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", srv.Port()),
		token:   srv.Token(),
	}
}

func (e *testEnv) doRequest(t *testing.T, method, path string, body string) *http.Response {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}

	req, err := http.NewRequest(method, e.baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return result
}

// --- Handler Tests ---

func TestHandleGetStatus(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	state, ok := body["state"].(string)
	if !ok {
		t.Fatal("missing state field")
	}
	// Daemon hasn't been Run(), so state should be "stopped".
	if state != "stopped" {
		t.Errorf("expected state 'stopped', got %q", state)
	}
}

func TestHandlePauseResume(t *testing.T) {
	env := setupTestEnv(t)

	// Pause.
	resp := env.doRequest(t, "POST", "/api/v1/daemon/pause", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for pause, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["state"] != "paused" {
		t.Errorf("expected state 'paused', got %v", body["state"])
	}

	// Pause again should return 409.
	resp = env.doRequest(t, "POST", "/api/v1/daemon/pause", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for double pause, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Resume.
	resp = env.doRequest(t, "POST", "/api/v1/daemon/resume", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for resume, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if body["state"] != "active" {
		t.Errorf("expected state 'active', got %v", body["state"])
	}

	// Resume again should return 409.
	resp = env.doRequest(t, "POST", "/api/v1/daemon/resume", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for double resume, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHandleGetMetrics(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/metrics", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	// Verify expected fields exist.
	for _, field := range []string{"cpu_usage_pct", "gpu_usage_pct", "memory_used_mb", "memory_total_mb", "disk_used_gb", "disk_total_gb", "cpu_temp_c", "gpu_temp_c"} {
		if _, ok := body[field]; !ok {
			t.Errorf("missing field %s", field)
		}
	}
}

func TestHandleGetLeafs(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/leafs", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	leafs, ok := body["leafs"].([]any)
	if !ok {
		t.Fatal("missing leafs array")
	}
	if len(leafs) != 1 {
		t.Fatalf("expected 1 leaf, got %d", len(leafs))
	}

	proj := leafs[0].(map[string]any)
	if proj["server_name"] != "test-server" {
		t.Errorf("expected server_name 'test-server', got %v", proj["server_name"])
	}
}

func TestHandleAttachDetach(t *testing.T) {
	env := setupTestEnv(t)

	// Attach a new server.
	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach",
		`{"server_address": "localhost:50052", "name": "new-server"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for attach, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify it appears in leafs list.
	resp = env.doRequest(t, "GET", "/api/v1/leafs", "")
	body := decodeJSON(t, resp)
	leafs := body["leafs"].([]any)
	if len(leafs) != 2 {
		t.Fatalf("expected 2 leafs after attach, got %d", len(leafs))
	}

	// Attach duplicate should fail.
	resp = env.doRequest(t, "POST", "/api/v1/leafs/attach",
		`{"server_address": "localhost:50052"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for duplicate attach, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Detach by name.
	resp = env.doRequest(t, "POST", "/api/v1/leafs/detach",
		`{"server_name": "new-server"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for detach, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify removed.
	resp = env.doRequest(t, "GET", "/api/v1/leafs", "")
	body = decodeJSON(t, resp)
	leafs = body["leafs"].([]any)
	if len(leafs) != 1 {
		t.Fatalf("expected 1 leaf after detach, got %d", len(leafs))
	}

	// Detach unknown should 404.
	resp = env.doRequest(t, "POST", "/api/v1/leafs/detach",
		`{"server_name": "nonexistent"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown detach, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHandleGetAvailableLeafs_Legacy(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/leafs/browse", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if _, ok := body["leafs"]; !ok {
		t.Error("missing leafs field")
	}
}

func TestHandleGetHistory(t *testing.T) {
	env := setupTestEnv(t)

	// Write some test history entries.
	for i := 0; i < 5; i++ {
		daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
			WorkUnitID:       fmt.Sprintf("wu-%d", i),
			LeafID:        "proj-1",
			CompletedAt:      time.Now().UTC().Add(-time.Duration(i) * time.Hour),
			WallClockSeconds: 100,
			ResultAccepted:   true,
		})
	}

	// Get all history.
	resp := env.doRequest(t, "GET", "/api/v1/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	entries := body["entries"].([]any)
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}

	// Test pagination with limit=2.
	resp = env.doRequest(t, "GET", "/api/v1/history?limit=2", "")
	body = decodeJSON(t, resp)
	entries = body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries with limit=2, got %d", len(entries))
	}
	pagination := body["pagination"].(map[string]any)
	if pagination["has_more"] != true {
		t.Error("expected has_more=true")
	}
	nextCursor := pagination["next_cursor"].(string)

	// Follow cursor.
	resp = env.doRequest(t, "GET", "/api/v1/history?limit=2&cursor="+nextCursor, "")
	body = decodeJSON(t, resp)
	entries = body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries on page 2, got %d", len(entries))
	}

	// Filter by leaf.
	resp = env.doRequest(t, "GET", "/api/v1/history?leaf_id=nonexistent", "")
	body = decodeJSON(t, resp)
	entries = body["entries"].([]any)
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for unknown leaf, got %d", len(entries))
	}
}

func TestHandleGetConfig(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	// Verify key fields exist and sensitive paths are redacted.
	if _, ok := body["resource_limits"]; !ok {
		t.Error("missing resource_limits")
	}
	if _, ok := body["scheduling"]; !ok {
		t.Error("missing scheduling")
	}
	// key_file and pubkey_file should NOT be present (redacted).
	if _, ok := body["key_file"]; ok {
		t.Error("key_file should be redacted")
	}
	if _, ok := body["pubkey_file"]; ok {
		t.Error("pubkey_file should be redacted")
	}
}

func TestHandleUpdateConfig(t *testing.T) {
	env := setupTestEnv(t)

	// Update max_cpu_cores.
	resp := env.doRequest(t, "PUT", "/api/v1/config",
		`{"resource_limits": {"max_cpu_cores": 4}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	rl := body["resource_limits"].(map[string]any)
	if int(rl["max_cpu_cores"].(float64)) != 4 {
		t.Errorf("expected max_cpu_cores=4, got %v", rl["max_cpu_cores"])
	}

	// Verify persisted to disk.
	loaded, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if loaded.ResourceLimits.MaxCPUCores != 4 {
		t.Errorf("config not persisted: max_cpu_cores=%d", loaded.ResourceLimits.MaxCPUCores)
	}
}

func TestHandleUpdateConfig_ValidationError(t *testing.T) {
	env := setupTestEnv(t)

	// Set invalid max_cpu_cores (0 is invalid).
	resp := env.doRequest(t, "PUT", "/api/v1/config",
		`{"resource_limits": {"max_cpu_cores": 0}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid config, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHandleGetCredit(t *testing.T) {
	env := setupTestEnv(t)

	// Write history entries.
	daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
		WorkUnitID:       "wu-1",
		LeafID:        "proj-a",
		CompletedAt:      time.Now().UTC(),
		WallClockSeconds: 60,
		ResultAccepted:   true,
	})
	daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
		WorkUnitID:       "wu-2",
		LeafID:        "proj-a",
		CompletedAt:      time.Now().UTC(),
		WallClockSeconds: 120,
		ResultAccepted:   true,
	})
	daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
		WorkUnitID:       "wu-3",
		LeafID:        "proj-b",
		CompletedAt:      time.Now().UTC(),
		WallClockSeconds: 30,
		ResultAccepted:   false, // rejected — should not count
	})

	resp := env.doRequest(t, "GET", "/api/v1/credit", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	if int(body["total_credit"].(float64)) != 2 {
		t.Errorf("expected total_credit=2, got %v", body["total_credit"])
	}
	if int(body["today"].(float64)) != 2 {
		t.Errorf("expected today=2, got %v", body["today"])
	}
	byLeaf := body["by_leaf"].([]any)
	if len(byLeaf) != 1 {
		t.Fatalf("expected 1 leaf in credit, got %d", len(byLeaf))
	}
}

func TestAuthRequired(t *testing.T) {
	env := setupTestEnv(t)

	// Request without auth token.
	req, _ := http.NewRequest("GET", env.baseURL+"/api/v1/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", resp.StatusCode)
	}
}

func TestAttachMissingAddress(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"name": "foo"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing address, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDetachMissingIdentifier(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/detach", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing identifier, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStatusReflectsPauseState(t *testing.T) {
	env := setupTestEnv(t)

	// Pause, then check status shows paused with reason.
	env.doRequest(t, "POST", "/api/v1/daemon/pause", "").Body.Close()

	resp := env.doRequest(t, "GET", "/api/v1/status", "")
	body := decodeJSON(t, resp)

	// Daemon is not Run()-ing, so state is "stopped" even though userPaused is true.
	// But IsPaused() returns true and PauseReason() returns "user".
	reason := body["paused_reason"]
	if reason == nil {
		t.Error("expected paused_reason to be set after pause")
	} else if reason.(string) != "user" {
		t.Errorf("expected paused_reason 'user', got %q", reason)
	}
}

func TestDetachByAddress(t *testing.T) {
	env := setupTestEnv(t)

	// Attach a server.
	env.doRequest(t, "POST", "/api/v1/leafs/attach",
		`{"server_address": "localhost:60000", "name": "addr-test"}`).Body.Close()

	// Detach by address (not name).
	resp := env.doRequest(t, "POST", "/api/v1/leafs/detach",
		`{"server_address": "localhost:60000"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for detach by address, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify removed.
	resp = env.doRequest(t, "GET", "/api/v1/leafs", "")
	body := decodeJSON(t, resp)
	leafs := body["leafs"].([]any)
	if len(leafs) != 1 {
		t.Fatalf("expected 1 leaf after detach by address, got %d", len(leafs))
	}
}

func TestUpdateConfigScheduling(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "PUT", "/api/v1/config",
		`{"scheduling": {"mode": "when_idle", "idle_threshold_mins": 10}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	sched := body["scheduling"].(map[string]any)
	if sched["mode"] != "WHEN_IDLE" {
		t.Errorf("expected mode 'WHEN_IDLE', got %v", sched["mode"])
	}
	if int(sched["idle_threshold_mins"].(float64)) != 10 {
		t.Errorf("expected idle_threshold_mins=10, got %v", sched["idle_threshold_mins"])
	}
}

func TestUpdateConfigLogLevel(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "PUT", "/api/v1/config", `{"log_level": "debug"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	if body["log_level"] != "debug" {
		t.Errorf("expected log_level 'debug', got %v", body["log_level"])
	}
}

func TestHistoryWithTimeFilter(t *testing.T) {
	env := setupTestEnv(t)

	now := time.Now().UTC()
	// Write entries at different times.
	daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
		WorkUnitID:     "wu-old",
		LeafID:      "proj-1",
		CompletedAt:    now.Add(-48 * time.Hour),
		ResultAccepted: true,
	})
	daemon.AppendHistory(env.dataDir, daemon.HistoryEntry{
		WorkUnitID:     "wu-recent",
		LeafID:      "proj-1",
		CompletedAt:    now.Add(-1 * time.Hour),
		ResultAccepted: true,
	})

	// Filter to only recent entries (from 24h ago).
	fromTime := now.Add(-24 * time.Hour).Format(time.RFC3339)
	resp := env.doRequest(t, "GET", "/api/v1/history?from="+fromTime, "")
	body := decodeJSON(t, resp)
	entries := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with from filter, got %d", len(entries))
	}
	entry := entries[0].(map[string]any)
	if entry["work_unit_id"] != "wu-recent" {
		t.Errorf("expected wu-recent, got %v", entry["work_unit_id"])
	}
}

func TestHistoryEmptyResult(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	entries := body["entries"].([]any)
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for empty history, got %d", len(entries))
	}
}

func TestHandleUpdateConfig_Notifications(t *testing.T) {
	env := setupTestEnv(t)

	// Update notification settings.
	resp := env.doRequest(t, "PUT", "/api/v1/config",
		`{"notifications": {"credit_milestones": false, "work_unit_completed": true, "credit_milestone_threshold": 250, "errors": false, "updates": false}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	notif, ok := body["notifications"].(map[string]any)
	if !ok {
		t.Fatal("missing notifications field in response")
	}
	if notif["credit_milestones"] != false {
		t.Errorf("expected credit_milestones=false, got %v", notif["credit_milestones"])
	}
	if notif["work_unit_completed"] != true {
		t.Errorf("expected work_unit_completed=true, got %v", notif["work_unit_completed"])
	}
	if int(notif["credit_milestone_threshold"].(float64)) != 250 {
		t.Errorf("expected credit_milestone_threshold=250, got %v", notif["credit_milestone_threshold"])
	}
	if notif["errors"] != false {
		t.Errorf("expected errors=false, got %v", notif["errors"])
	}
	if notif["updates"] != false {
		t.Errorf("expected updates=false, got %v", notif["updates"])
	}

	// Verify persisted to disk.
	loaded, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if loaded.Notifications.CreditMilestones {
		t.Error("persisted CreditMilestones should be false")
	}
	if !loaded.Notifications.WorkUnitCompleted {
		t.Error("persisted WorkUnitCompleted should be true")
	}
	if loaded.Notifications.CreditMilestoneThreshold != 250 {
		t.Errorf("persisted CreditMilestoneThreshold = %d, want 250", loaded.Notifications.CreditMilestoneThreshold)
	}
}

func TestHandleGetConfig_IncludesNotifications(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	notif, ok := body["notifications"].(map[string]any)
	if !ok {
		t.Fatal("missing notifications field in GET /api/v1/config response")
	}

	// Check that all expected notification fields are present with defaults.
	if notif["credit_milestones"] != true {
		t.Errorf("expected default credit_milestones=true, got %v", notif["credit_milestones"])
	}
	if int(notif["credit_milestone_threshold"].(float64)) != 100 {
		t.Errorf("expected default credit_milestone_threshold=100, got %v", notif["credit_milestone_threshold"])
	}
	if notif["work_unit_completed"] != false {
		t.Errorf("expected default work_unit_completed=false, got %v", notif["work_unit_completed"])
	}
	if notif["errors"] != true {
		t.Errorf("expected default errors=true, got %v", notif["errors"])
	}
	if notif["updates"] != true {
		t.Errorf("expected default updates=true, got %v", notif["updates"])
	}
}

func TestHandleRegenerateKeypair(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	// Get initial public key.
	resp := env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for config, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	initialPubKey := body["public_key"].(string)
	if initialPubKey == "" {
		t.Fatal("expected non-empty initial public_key")
	}

	// Regenerate keypair.
	resp = env.doRequest(t, "POST", "/api/v1/identity/regenerate", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for regenerate, got %d", resp.StatusCode)
	}

	body = decodeJSON(t, resp)
	newPubKey, ok := body["public_key"].(string)
	if !ok || newPubKey == "" {
		t.Fatal("expected non-empty public_key in regenerate response")
	}

	// The new key should be different from the initial key.
	if newPubKey == initialPubKey {
		t.Error("regenerated key should differ from initial key")
	}

	// Verify the new key is valid by reading it from the config endpoint.
	resp = env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for config after regenerate, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	configPubKey := body["public_key"].(string)
	if configPubKey != newPubKey {
		t.Errorf("config public_key after regenerate = %q, want %q", configPubKey, newPubKey)
	}
}

// setupTestEnvWithKeys creates a test env with an Ed25519 keypair on disk,
// so SignChallenge can load it.
func setupTestEnvWithKeys(t *testing.T) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Generate and save a keypair.
	pub, priv, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	keyFile := filepath.Join(tmpDir, "identity.key")
	pubFile := filepath.Join(tmpDir, "identity.pub")
	if err := identity.SaveKeyPair(keyFile, pubFile, priv, pub); err != nil {
		t.Fatalf("saving keypair: %v", err)
	}

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.KeyFile = keyFile
	cfg.PubKeyFile = pubFile
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	return &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", srv.Port()),
		token:   srv.Token(),
	}
}

func TestHandleSignChallenge_Success(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	// Use a known hex challenge.
	challengeHex := "deadbeef01020304050607080910111213141516171819202122232425262728"
	body := fmt.Sprintf(`{"challenge_hex": "%s"}`, challengeHex)

	resp := env.doRequest(t, "POST", "/api/v1/identity/sign", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)

	pubKey, ok := result["public_key"].(string)
	if !ok || pubKey == "" {
		t.Error("expected non-empty public_key in response")
	}

	sig, ok := result["signature"].(string)
	if !ok || sig == "" {
		t.Error("expected non-empty signature in response")
	}

	// Verify the signature is valid by decoding and checking with ed25519.
	if pubKey != "" && sig != "" {
		pubBytes, err := base64.RawURLEncoding.DecodeString(pubKey)
		if err != nil {
			t.Fatalf("decoding public key: %v", err)
		}
		sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
		if err != nil {
			t.Fatalf("decoding signature: %v", err)
		}
		challengeBytes, err := hex.DecodeString(challengeHex)
		if err != nil {
			t.Fatalf("decoding challenge: %v", err)
		}

		if !ed25519.Verify(ed25519.PublicKey(pubBytes), challengeBytes, sigBytes) {
			t.Error("signature verification failed — the returned signature does not match the challenge")
		}
	}
}

func TestHandleSignChallenge_EmptyChallenge(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	resp := env.doRequest(t, "POST", "/api/v1/identity/sign", `{"challenge_hex": ""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty challenge_hex, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHandleSignChallenge_InvalidHex(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	resp := env.doRequest(t, "POST", "/api/v1/identity/sign", `{"challenge_hex": "not-valid-hex!!!"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 for invalid hex, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHandleSignChallenge_MissingBody(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	resp := env.doRequest(t, "POST", "/api/v1/identity/sign", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing challenge_hex, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestFlow_IdentityRegeneration exercises the end-to-end journey for
// identity regeneration — the F24 flow.
//
// Steps:
//  1. Start with fresh config (keys present)
//  2. GET /api/v1/config — capture the initial public key
//  3. POST /api/v1/identity/regenerate — verify new public key
//  4. GET /api/v1/config — verify the config reflects the new public key
func TestFlow_IdentityRegeneration(t *testing.T) {
	env := setupTestEnvWithKeys(t)

	// Step 1+2: GET /api/v1/config — capture the initial public key.
	resp := env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2: expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)

	initialPubKey, ok := body["public_key"].(string)
	if !ok || initialPubKey == "" {
		t.Fatal("step 2: expected non-empty public_key in fresh config")
	}

	// Step 3: POST /api/v1/identity/regenerate — verify new public key.
	resp = env.doRequest(t, "POST", "/api/v1/identity/regenerate", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3: expected 200, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	newPubKey, ok := body["public_key"].(string)
	if !ok || newPubKey == "" {
		t.Fatal("step 3: expected non-empty public_key in regenerate response")
	}
	if newPubKey == initialPubKey {
		t.Fatal("step 3: regenerated key should differ from initial key")
	}

	// Step 4: GET /api/v1/config — verify the config reflects the new public key.
	resp = env.doRequest(t, "GET", "/api/v1/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4: expected 200, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if body["public_key"] != newPubKey {
		t.Fatalf("step 4: config public_key=%v, want %q", body["public_key"], newPubKey)
	}
}

func TestHandleGetHeads_EmptyCache(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	heads, ok := body["heads"].([]any)
	if !ok {
		t.Fatal("missing heads array")
	}
	// setupTestEnv creates one server ("test-server") but no leaf cache.
	if len(heads) != 1 {
		t.Fatalf("expected 1 head entry, got %d", len(heads))
	}
	head := heads[0].(map[string]any)
	if head["name"] != "test-server" {
		t.Errorf("expected head name 'test-server', got %v", head["name"])
	}
	if head["status"] != "disconnected" {
		t.Errorf("expected status 'disconnected', got %v", head["status"])
	}
	// Leafs should be an empty array (not null).
	leafs, ok := head["leafs"].([]any)
	if !ok {
		t.Fatal("missing leafs array in head")
	}
	if len(leafs) != 0 {
		t.Errorf("expected 0 leafs with empty cache, got %d", len(leafs))
	}
}

func TestHandleGetHeads_WithCachedLeafs(t *testing.T) {
	env := setupTestEnv(t)

	// Populate the leaf cache directly on the daemon.
	lc := env.daemon.GetLeafCache()
	lc.PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name:        "Test Head",
		Description: "A research head",
		URL:         "https://test.example.com",
		Leafs: []daemon.CachedLeafInfo{
			{
				ID:               "leaf-1",
				Slug:             "prime-gaps",
				Name:             "Prime Gap Search",
				Description:      "Finding prime gaps",
				ResearchArea:     []string{"mathematics"},
				TaskPattern:      "PARAMETER_SWEEP",
				State:            "ACTIVE",
				QueuedWorkUnits:  42,
				ActiveVolunteers: 7,
			},
			{
				ID:   "leaf-2",
				Slug: "protein-fold",
				Name: "Protein Folding",
				State: "ACTIVE",
			},
		},
		DefaultWeights: map[string]int{
			"prime-gaps":   200,
			"protein-fold": 100,
		},
	})

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	heads := body["heads"].([]any)
	if len(heads) != 1 {
		t.Fatalf("expected 1 head, got %d", len(heads))
	}

	head := heads[0].(map[string]any)
	if head["name"] != "Test Head" {
		t.Errorf("expected head name 'Test Head', got %v", head["name"])
	}
	if head["description"] != "A research head" {
		t.Errorf("expected description 'A research head', got %v", head["description"])
	}
	if int(head["weight"].(float64)) != 100 {
		t.Errorf("expected default weight 100, got %v", head["weight"])
	}

	leafs := head["leafs"].([]any)
	if len(leafs) != 2 {
		t.Fatalf("expected 2 leafs, got %d", len(leafs))
	}

	leaf0 := leafs[0].(map[string]any)
	if leaf0["slug"] != "prime-gaps" {
		t.Errorf("expected slug 'prime-gaps', got %v", leaf0["slug"])
	}
	if leaf0["enabled"] != true {
		t.Errorf("expected enabled=true (ALL mode), got %v", leaf0["enabled"])
	}
	if int(leaf0["effective_weight"].(float64)) != 200 {
		t.Errorf("expected effective_weight=200 (from researcher default), got %v", leaf0["effective_weight"])
	}
	if int(leaf0["queued_work_units"].(float64)) != 42 {
		t.Errorf("expected queued_work_units=42, got %v", leaf0["queued_work_units"])
	}
}

func TestHandleGetHeads_ExecutionSpecPropagation(t *testing.T) {
	env := setupTestEnv(t)

	lc := env.daemon.GetLeafCache()
	lc.PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Spec Head",
		Leafs: []daemon.CachedLeafInfo{
			{
				ID:   "leaf-wasm",
				Slug: "wasm-compute",
				Name: "WASM Compute",
				ExecutionSpec: &daemon.CachedExecutionSpec{
					Binaries:    map[string]string{"wasm": "https://example.com/module.wasm"},
					GPURequired: true,
					GPUType:     "WEBGPU",
					MaxMemoryMB: 2048,
				},
			},
			{
				ID:   "leaf-container",
				Slug: "container-compute",
				Name: "Container Compute",
				ExecutionSpec: &daemon.CachedExecutionSpec{
					Image:         "alpine:latest",
					NetworkAccess: true,
				},
			},
			{
				ID:   "leaf-no-spec",
				Slug: "no-spec",
				Name: "No Spec",
				// ExecutionSpec is nil
			},
		},
	})

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	heads := body["heads"].([]any)
	head := heads[0].(map[string]any)
	leafs := head["leafs"].([]any)

	if len(leafs) != 3 {
		t.Fatalf("expected 3 leafs, got %d", len(leafs))
	}

	// WASM leaf should have execution_spec with binaries.wasm.
	wasmLeaf := leafs[0].(map[string]any)
	wasmSpec, ok := wasmLeaf["execution_spec"].(map[string]any)
	if !ok || wasmSpec == nil {
		t.Fatal("WASM leaf should have execution_spec")
	}
	bins := wasmSpec["binaries"].(map[string]any)
	if bins["wasm"] != "https://example.com/module.wasm" {
		t.Errorf("binaries.wasm = %v", bins["wasm"])
	}
	if wasmSpec["gpu_required"] != true {
		t.Errorf("gpu_required = %v, want true", wasmSpec["gpu_required"])
	}
	if wasmSpec["gpu_type"] != "WEBGPU" {
		t.Errorf("gpu_type = %v, want WEBGPU", wasmSpec["gpu_type"])
	}

	// Container leaf should have execution_spec with image.
	containerLeaf := leafs[1].(map[string]any)
	containerSpec, ok := containerLeaf["execution_spec"].(map[string]any)
	if !ok || containerSpec == nil {
		t.Fatal("Container leaf should have execution_spec")
	}
	if containerSpec["image"] != "alpine:latest" {
		t.Errorf("image = %v, want alpine:latest", containerSpec["image"])
	}

	// No-spec leaf should NOT have execution_spec key (omitempty).
	noSpecLeaf := leafs[2].(map[string]any)
	if _, exists := noSpecLeaf["execution_spec"]; exists {
		t.Error("no-spec leaf should not have execution_spec (omitempty)")
	}
}

func TestHandleGetHeads_LeafPreferencesFiltering(t *testing.T) {
	// Set up with BLOCKLIST mode that disables one leaf.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.Servers = []config.ServerConfig{
		{
			GRPCAddress: "localhost:50051",
			Name:        "test-server",
			LeafPreferences: config.LeafPreferences{
				Mode:     "BLOCKLIST",
				Disabled: []string{"blocked-leaf"},
				Weights:  map[string]int{"good-leaf": 500},
			},
		},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	// Populate leaf cache.
	lc := d.GetLeafCache()
	lc.PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Test Head",
		Leafs: []daemon.CachedLeafInfo{
			{ID: "l1", Slug: "good-leaf", Name: "Good Leaf", State: "ACTIVE"},
			{ID: "l2", Slug: "blocked-leaf", Name: "Blocked Leaf", State: "ACTIVE"},
		},
		DefaultWeights: map[string]int{"good-leaf": 100, "blocked-leaf": 100},
	})

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	env := &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", srv.Port()),
		token:   srv.Token(),
	}

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	heads := body["heads"].([]any)
	head := heads[0].(map[string]any)
	leafs := head["leafs"].([]any)

	if len(leafs) != 2 {
		t.Fatalf("expected 2 leafs in response, got %d", len(leafs))
	}

	// Find each leaf and check enabled/weight.
	for _, raw := range leafs {
		leaf := raw.(map[string]any)
		switch leaf["slug"] {
		case "good-leaf":
			if leaf["enabled"] != true {
				t.Error("good-leaf should be enabled")
			}
			if int(leaf["effective_weight"].(float64)) != 500 {
				t.Errorf("good-leaf effective_weight = %v, want 500 (custom override)", leaf["effective_weight"])
			}
		case "blocked-leaf":
			if leaf["enabled"] != false {
				t.Error("blocked-leaf should be disabled in BLOCKLIST mode")
			}
		default:
			t.Errorf("unexpected leaf slug: %v", leaf["slug"])
		}
	}
}

func TestHandleGetHeads_VolunteerIDInJSON(t *testing.T) {
	env := setupTestEnv(t)

	// Inject a MultiServerClient with a VolunteerID on the server connection.
	mc := daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{
			Name:        "test-server",
			VolunteerID: "vol-handler-test-456",
			Available:   true,
		},
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	env.daemon.SetMultiClientForTest(mc)

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	heads := body["heads"].([]any)
	if len(heads) != 1 {
		t.Fatalf("expected 1 head, got %d", len(heads))
	}

	head := heads[0].(map[string]any)
	volID, ok := head["volunteer_id"]
	if !ok {
		t.Fatal("volunteer_id field missing from head JSON response")
	}
	if volID != "vol-handler-test-456" {
		t.Errorf("volunteer_id = %v, want %q", volID, "vol-handler-test-456")
	}
}

func TestHandleGetAvailableLeafs(t *testing.T) {
	env := setupTestEnv(t)

	// Populate cache.
	lc := env.daemon.GetLeafCache()
	lc.PopulateForTest("test-server", &daemon.CachedHeadInfo{
		Name: "Test Head",
		Leafs: []daemon.CachedLeafInfo{
			{ID: "leaf-1", Slug: "a", Name: "Leaf A", State: "ACTIVE"},
			{ID: "leaf-2", Slug: "b", Name: "Leaf B", State: "PAUSED"},
		},
		DefaultWeights: map[string]int{"a": 100, "b": 100},
	})

	resp := env.doRequest(t, "GET", "/api/v1/leafs/available", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	leafs, ok := body["leafs"].([]any)
	if !ok {
		t.Fatal("missing leafs array")
	}
	if len(leafs) != 2 {
		t.Fatalf("expected 2 leafs, got %d", len(leafs))
	}
}

func TestHandleSignChallenge_NoKeys(t *testing.T) {
	// Create an env where key files point to non-existent paths.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.KeyFile = filepath.Join(tmpDir, "nonexistent.key")
	cfg.PubKeyFile = filepath.Join(tmpDir, "nonexistent.pub")
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	env := &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", srv.Port()),
		token:   srv.Token(),
	}

	resp := env.doRequest(t, "POST", "/api/v1/identity/sign",
		`{"challenge_hex": "deadbeef"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no keys exist, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestActiveTaskInfo_JSONIncludesWorkDir(t *testing.T) {
	// Verify that ActiveTaskInfo serializes work_dir to JSON.
	info := ActiveTaskInfo{
		WorkUnitID:            "wu-123",
		LeafName:              "prime-gaps",
		ProgressPct:           42,
		ElapsedSeconds:        300,
		WorkDir:               "/tmp/lettuce/wu-123",
		CheckpointSequence:    3,
		ResumedFromCheckpoint: true,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify work_dir is present and correct.
	workDir, ok := m["work_dir"]
	if !ok {
		t.Fatal("work_dir field missing from JSON output")
	}
	if workDir != "/tmp/lettuce/wu-123" {
		t.Errorf("work_dir = %v, want %q", workDir, "/tmp/lettuce/wu-123")
	}

	// Verify all expected fields are present.
	for _, field := range []string{"work_unit_id", "leaf_name", "progress_pct", "elapsed_seconds", "work_dir", "checkpoint_sequence", "resumed_from_checkpoint"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing field %q in JSON output", field)
		}
	}
}

func TestActiveTaskInfo_JSONWorkDirEmpty(t *testing.T) {
	// Verify that WorkDir serializes as empty string (not omitted) when not set.
	info := ActiveTaskInfo{
		WorkUnitID:     "wu-456",
		LeafName:       "test-leaf",
		ElapsedSeconds: 60,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	workDir, ok := m["work_dir"]
	if !ok {
		t.Fatal("work_dir field missing from JSON output even when empty")
	}
	if workDir != "" {
		t.Errorf("work_dir = %v, want empty string", workDir)
	}
}

func TestHandleSuspendAndQuit_ResponseBeforeExit(t *testing.T) {
	// Override osExitFunc so SuspendAndQuit doesn't kill the test process.
	restore := daemon.SetOsExitFunc(func(code int) {
		// no-op: prevent actual exit
	})
	defer restore()

	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/daemon/suspend-and-quit", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	state, ok := body["state"].(string)
	if !ok {
		t.Fatal("missing state field in response")
	}
	if state != "shutting_down" {
		t.Errorf("expected state 'shutting_down', got %q", state)
	}
}

// --- Per-Task Action Handler Tests ---

func TestHandleSuspendTask_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "POST", "/api/v1/tasks/wu-nonexistent/suspend", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errMap["code"] != "NOT_FOUND" {
		t.Errorf("error code = %v, want NOT_FOUND", errMap["code"])
	}
}

func TestHandleResumeTask_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "POST", "/api/v1/tasks/wu-nonexistent/resume", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errMap["code"] != "NOT_FOUND" {
		t.Errorf("error code = %v, want NOT_FOUND", errMap["code"])
	}
}

func TestHandleAbortTask_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "POST", "/api/v1/tasks/wu-nonexistent/abort", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errMap["code"] != "NOT_FOUND" {
		t.Errorf("error code = %v, want NOT_FOUND", errMap["code"])
	}
}

func TestHandleGetTaskDetails_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.doRequest(t, "GET", "/api/v1/tasks/wu-nonexistent/details", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errMap["code"] != "NOT_FOUND" {
		t.Errorf("error code = %v, want NOT_FOUND", errMap["code"])
	}
}

// --- Mock types for task visibility E2E test ---

// e2eMockWorkClient satisfies daemon.WorkClient for the management E2E test.
type e2eMockWorkClient struct{}

func (m *e2eMockWorkClient) Close() error { return nil }
func (m *e2eMockWorkClient) RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	return nil, fmt.Errorf("no work")
}
func (m *e2eMockWorkClient) SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	return &lettucev1.SubmitResultResponse{ResultId: "r-1", Accepted: true}, nil
}
func (m *e2eMockWorkClient) Heartbeat(ctx context.Context, req *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
	return &lettucev1.HeartbeatResponse{ContinueExecution: true}, nil
}
func (m *e2eMockWorkClient) SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
}
func (m *e2eMockWorkClient) GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	return &lettucev1.GetCheckpointResponse{HasCheckpoint: false}, nil
}
func (m *e2eMockWorkClient) GetHeadInfo(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	return &lettucev1.GetHeadInfoResponse{}, nil
}
func (m *e2eMockWorkClient) AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	return &lettucev1.AbandonWorkUnitResponse{Requeued: true}, nil
}

// e2eMockRuntime satisfies runtime.Runtime for the management E2E test.
type e2eMockRuntime struct {
	executeFn func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error)
}

func (m *e2eMockRuntime) Prepare(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
	return &runtime.PrepareResult{WorkDir: "/tmp/test"}, nil
}
func (m *e2eMockRuntime) Execute(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
	if m.executeFn != nil {
		return m.executeFn(ctx, wu, prep)
	}
	return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
}
func (m *e2eMockRuntime) Cleanup(prep *runtime.PrepareResult) error { return nil }
func (m *e2eMockRuntime) CanHandle(spec *runtime.ExecutionSpec) bool { return true }
func (m *e2eMockRuntime) Name() string                              { return "native" }

// e2eMockProcessHandle satisfies daemon.ProcessHandle for per-task suspend/resume.
type e2eMockProcessHandle struct {
	pid       int
	suspended bool
}

func (m *e2eMockProcessHandle) Suspend() error { m.suspended = true; return nil }
func (m *e2eMockProcessHandle) Resume() error  { m.suspended = false; return nil }
func (m *e2eMockProcessHandle) PID() int       { return m.pid }

// setupTestEnvWithActiveTask creates a test environment with a daemon that has
// an active task in a SlotManager, suitable for testing per-task endpoints.
// The blockCh channel is returned so the caller can unblock the task when needed
// (closing blockCh causes the mock execution to complete).
func setupTestEnvWithActiveTask(t *testing.T) (env *testEnv, workUnitID string, blockCh chan struct{}) {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.Thermal.Enabled = false
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	// Create a SlotManager and inject it into the daemon for testing.
	sm := daemon.NewSlotManager(1, logger)
	d.SetSlotManagerForTest(sm)

	// Override readProcessMetrics to avoid real OS calls.
	origReader := readProcessMetrics
	readProcessMetrics = func(pid int) (*procmetrics.ProcessMetrics, error) {
		rss := 128.5
		vmem := 512.0
		cpuPct := 42.0
		diskR := 10.0
		diskW := 5.0
		return &procmetrics.ProcessMetrics{
			MemoryRSSMB:     &rss,
			VirtualMemoryMB: &vmem,
			CPUUsagePct:     &cpuPct,
			DiskReadMB:      &diskR,
			DiskWrittenMB:   &diskW,
		}, nil
	}

	// Create the blocking mock execution.
	blockCh = make(chan struct{})
	workUnitID = "wu-e2e-task-1"
	mockRT := &e2eMockRuntime{
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			select {
			case <-blockCh:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
		},
	}

	// Build the PreFetchItem and start the slot.
	workDir := filepath.Join(tmpDir, "work", workUnitID)
	os.MkdirAll(workDir, 0755)

	item := &daemon.PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              workUnitID,
			LeafID:          "leaf-prime-gaps",
			Runtime:         "native",
			DeadlineSeconds: 3600,
		},
		WUResp: &lettucev1.WorkUnitAssignment{
			HeartbeatIntervalSeconds: 300,
		},
		Prep: &runtime.PrepareResult{
			WorkDir: workDir,
		},
		Runtime: mockRT,
		Conn: &daemon.ServerConnection{
			Name:        "test-server",
			VolunteerID: "vol-e2e",
			Client:      &e2eMockWorkClient{},
			Config:      config.ServerConfig{GRPCAddress: "localhost:50051", Name: "test-server"},
		},
		FetchedAt: time.Now(),
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	slotID := sm.AvailableSlotID()
	sm.StartSlot(ctx, slotID, item, d)

	// Give the slot goroutine time to start.
	time.Sleep(50 * time.Millisecond)

	// Attach a mock process handle so per-task suspend/resume works.
	sm.SetProcessHandle(0, &e2eMockProcessHandle{pid: 12345})

	// Set up the management bridge and server.
	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		// Restore the original reader.
		readProcessMetrics = origReader

		// Unblock the task if still blocked.
		select {
		case <-blockCh:
		default:
			close(blockCh)
		}
		ctxCancel()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	})

	env = &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", srv.Port()),
		token:   srv.Token(),
	}
	return env, workUnitID, blockCh
}

// TestFlow_TaskVisibility_E2E is a flow-complete integration test for the
// daemon task visibility feature (F32). It exercises the full user journey:
//
//  1. A daemon with an active task exposes per-task status via the management API
//  2. Per-task suspend/resume/abort endpoints work correctly
//  3. The task details endpoint returns full task properties
//
// This test operates entirely through the HTTP management API, creating a
// daemon with a mock SlotManager that has a running task, then exercising
// every per-task endpoint in sequence.
func TestFlow_TaskVisibility_E2E(t *testing.T) {
	env, wuID, blockCh := setupTestEnvWithActiveTask(t)
	taskPath := "/api/v1/tasks/" + wuID

	// =====================================================================
	// Step 1: GET /api/v1/status — verify the active task appears.
	// =====================================================================
	resp := env.doRequest(t, "GET", "/api/v1/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 1: GET /status expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	activeTasks, ok := body["active_tasks"].([]any)
	if !ok {
		t.Fatal("step 1: missing active_tasks array")
	}
	if len(activeTasks) != 1 {
		t.Fatalf("step 1: expected 1 active task, got %d", len(activeTasks))
	}
	task0 := activeTasks[0].(map[string]any)
	if task0["work_unit_id"] != wuID {
		t.Errorf("step 1: work_unit_id = %v, want %s", task0["work_unit_id"], wuID)
	}
	if task0["task_status"] != "running" {
		t.Errorf("step 1: task_status = %v, want 'running'", task0["task_status"])
	}

	// =====================================================================
	// Step 2: GET /api/v1/tasks/{id}/details — verify full task-detail fields.
	// =====================================================================
	resp = env.doRequest(t, "GET", taskPath+"/details", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2: GET details expected 200, got %d", resp.StatusCode)
	}
	detail := decodeJSON(t, resp)

	// Verify core identity fields.
	if detail["work_unit_id"] != wuID {
		t.Errorf("step 2: work_unit_id = %v, want %s", detail["work_unit_id"], wuID)
	}
	if detail["task_status"] != "running" {
		t.Errorf("step 2: task_status = %v, want 'running'", detail["task_status"])
	}
	if detail["head_name"] != "test-server" {
		t.Errorf("step 2: head_name = %v, want 'test-server'", detail["head_name"])
	}
	if detail["runtime_type"] != "native" {
		t.Errorf("step 2: runtime_type = %v, want 'native'", detail["runtime_type"])
	}

	// Verify time-based fields are populated.
	elapsed, ok := detail["elapsed_seconds"].(float64)
	if !ok || elapsed < 0 {
		t.Errorf("step 2: elapsed_seconds = %v, want >= 0", detail["elapsed_seconds"])
	}
	cpuSec, ok := detail["cpu_seconds"].(float64)
	if !ok || cpuSec < 0 {
		t.Errorf("step 2: cpu_seconds = %v, want >= 0", detail["cpu_seconds"])
	}

	// Verify deadline is roughly 3600 - elapsed.
	deadlineSec, ok := detail["deadline_seconds"].(float64)
	if !ok {
		t.Errorf("step 2: missing deadline_seconds")
	} else if deadlineSec < 3590 || deadlineSec > 3601 {
		t.Errorf("step 2: deadline_seconds = %v, want ~3600", deadlineSec)
	}

	// Verify process ID is present.
	pid, ok := detail["process_id"].(float64)
	if !ok || int(pid) != 12345 {
		t.Errorf("step 2: process_id = %v, want 12345", detail["process_id"])
	}

	// Verify per-process metrics from the mocked readProcessMetrics.
	if rss, ok := detail["memory_rss_mb"].(float64); !ok || rss != 128.5 {
		t.Errorf("step 2: memory_rss_mb = %v, want 128.5", detail["memory_rss_mb"])
	}
	if vmem, ok := detail["virtual_memory_mb"].(float64); !ok || vmem != 512.0 {
		t.Errorf("step 2: virtual_memory_mb = %v, want 512.0", detail["virtual_memory_mb"])
	}
	if cpuPct, ok := detail["cpu_usage_pct"].(float64); !ok || cpuPct != 42.0 {
		t.Errorf("step 2: cpu_usage_pct = %v, want 42.0", detail["cpu_usage_pct"])
	}

	// Verify fraction_done is present (0 since no progress file).
	if fd, ok := detail["fraction_done"].(float64); !ok || fd != 0 {
		t.Errorf("step 2: fraction_done = %v, want 0", detail["fraction_done"])
	}

	// =====================================================================
	// Step 3: POST /api/v1/tasks/{id}/suspend — suspend the task.
	// =====================================================================
	resp = env.doRequest(t, "POST", taskPath+"/suspend", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3: POST suspend expected 200, got %d", resp.StatusCode)
	}
	suspendBody := decodeJSON(t, resp)
	if suspendBody["status"] != "suspended" {
		t.Errorf("step 3: suspend response status = %v, want 'suspended'", suspendBody["status"])
	}

	// =====================================================================
	// Step 4: GET /api/v1/tasks/{id}/details — verify status is now suspended.
	// =====================================================================
	resp = env.doRequest(t, "GET", taskPath+"/details", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4: GET details expected 200, got %d", resp.StatusCode)
	}
	detail = decodeJSON(t, resp)
	if detail["task_status"] != "suspended_user" {
		t.Errorf("step 4: task_status = %v, want 'suspended_user'", detail["task_status"])
	}
	if detail["status_reason"] == nil {
		t.Error("step 4: status_reason should not be nil when suspended")
	}

	// =====================================================================
	// Step 5: POST /api/v1/tasks/{id}/suspend again — should be 409 (already suspended).
	// =====================================================================
	resp = env.doRequest(t, "POST", taskPath+"/suspend", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("step 5: POST suspend again expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// =====================================================================
	// Step 6: POST /api/v1/tasks/{id}/resume — resume the task.
	// =====================================================================
	resp = env.doRequest(t, "POST", taskPath+"/resume", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 6: POST resume expected 200, got %d", resp.StatusCode)
	}
	resumeBody := decodeJSON(t, resp)
	if resumeBody["status"] != "resumed" {
		t.Errorf("step 6: resume response status = %v, want 'resumed'", resumeBody["status"])
	}

	// =====================================================================
	// Step 7: GET /api/v1/tasks/{id}/details — verify status is back to running.
	// =====================================================================
	resp = env.doRequest(t, "GET", taskPath+"/details", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 7: GET details expected 200, got %d", resp.StatusCode)
	}
	detail = decodeJSON(t, resp)
	if detail["task_status"] != "running" {
		t.Errorf("step 7: task_status = %v, want 'running'", detail["task_status"])
	}

	// =====================================================================
	// Step 8: POST /api/v1/tasks/{id}/resume again — should be 409 (not suspended).
	// =====================================================================
	resp = env.doRequest(t, "POST", taskPath+"/resume", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("step 8: POST resume again expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// =====================================================================
	// Step 9: POST /api/v1/tasks/{id}/abort — abort the task.
	// =====================================================================
	resp = env.doRequest(t, "POST", taskPath+"/abort", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 9: POST abort expected 200, got %d", resp.StatusCode)
	}
	abortBody := decodeJSON(t, resp)
	if abortBody["status"] != "aborted" {
		t.Errorf("step 9: abort response status = %v, want 'aborted'", abortBody["status"])
	}

	// Give the slot goroutine time to clean up after abort.
	time.Sleep(100 * time.Millisecond)

	// =====================================================================
	// Step 10: GET /api/v1/tasks/{id}/details — task should be gone (404).
	// =====================================================================
	resp = env.doRequest(t, "GET", taskPath+"/details", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("step 10: GET details after abort expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// =====================================================================
	// Step 11: GET /api/v1/status — verify active_tasks is now empty.
	// =====================================================================
	resp = env.doRequest(t, "GET", "/api/v1/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 11: GET /status expected 200, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	activeTasks, ok = body["active_tasks"].([]any)
	if !ok {
		t.Fatal("step 11: missing active_tasks array")
	}
	if len(activeTasks) != 0 {
		t.Errorf("step 11: expected 0 active tasks after abort, got %d", len(activeTasks))
	}

	// =====================================================================
	// Step 12: POST /api/v1/tasks/{id}/abort on non-existent task — 404.
	// =====================================================================
	resp = env.doRequest(t, "POST", "/api/v1/tasks/wu-does-not-exist/abort", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("step 12: POST abort non-existent expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The blockCh is already unblocked by the abort (context cancellation).
	// Cleanup will drain the slot via t.Cleanup.
	_ = blockCh
}

func TestHandleListResults_Empty(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/results", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatal("expected results array in response")
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d", len(results))
	}
}

func TestHandleListResults_WithEntries(t *testing.T) {
	env := setupTestEnv(t)

	// Save some results directly using the daemon package. IDs must be canonical
	// UUIDs (H2 path-traversal fix in SaveResult/GetResultData).
	for _, id := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
		err := daemon.SaveResult(env.dataDir, id, "Test Leaf", "test-leaf", "test-head",
			[]byte(`{"data": "ok"}`), "/viz/bundle.html", 0)
		if err != nil {
			t.Fatalf("SaveResult(%s) error: %v", id, err)
		}
	}

	resp := env.doRequest(t, "GET", "/api/v1/results", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := decodeJSON(t, resp)
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatal("expected results array in response")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Verify the entries have the expected fields.
	first := results[0].(map[string]any)
	if first["leaf_name"] != "Test Leaf" {
		t.Errorf("leaf_name = %v, want Test Leaf", first["leaf_name"])
	}
	if first["head_name"] != "test-head" {
		t.Errorf("head_name = %v, want test-head", first["head_name"])
	}
	if first["viz_bundle_path"] != "/viz/bundle.html" {
		t.Errorf("viz_bundle_path = %v, want /viz/bundle.html", first["viz_bundle_path"])
	}
}

func TestHandleGetResult_Found(t *testing.T) {
	env := setupTestEnv(t)

	outputData := []byte(`{"answer": 42, "details": "computed"}`)
	const wuID = "33333333-3333-4333-8333-333333333333"
	err := daemon.SaveResult(env.dataDir, wuID, "Leaf", "leaf", "head", outputData, "", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	resp := env.doRequest(t, "GET", "/api/v1/results/"+wuID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result["answer"].(float64) != 42 {
		t.Errorf("answer = %v, want 42", result["answer"])
	}
}

func TestHandleGetResult_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/results/wu-nonexistent", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
