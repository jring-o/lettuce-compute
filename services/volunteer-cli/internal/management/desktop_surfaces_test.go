package management

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// Management-API surfaces the desktop app needs: per-head runtime trust
// settable over PUT /api/v1/config and POST /api/v1/leafs/attach, the
// build-version and update-required signals, and the volunteer-facing notice
// ring at GET /api/v1/notices.

const testServerAddr = "localhost:50051"

// setTrustOnDisk edits the test env's config file the way `heads trust` does
// (load, set, save) so a test can seed any starting trust.
func setTrustOnDisk(t *testing.T, cfgPath string, trusted []string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Servers[0].TrustedRuntimes = trusted
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// trustOnDisk reads the first server's recorded trust back from the file.
func trustOnDisk(t *testing.T, cfgPath, addr string) []string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.Servers {
		if s.GRPCAddress == addr {
			return s.TrustedRuntimes
		}
	}
	t.Fatalf("server %s missing from config", addr)
	return nil
}

// errorCode extracts the code from the API's {"error":{"code":...}} envelope.
func errorCode(body map[string]any) any {
	if e, ok := body["error"].(map[string]any); ok {
		return e["code"]
	}
	return nil
}

func putServers(t *testing.T, env *testEnv, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp := env.doRequest(t, "PUT", "/api/v1/config", body)
	return resp, decodeJSON(t, resp)
}

// --- PUT /api/v1/config: trusted_runtimes ---

func TestUpdateConfig_TrustWiden(t *testing.T) {
	env := setupTestEnv(t)
	setTrustOnDisk(t, env.cfgPath, []string{})

	resp, body := putServers(t, env, `{"servers":[{"name":"test-server","trusted_runtimes":["container"]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != true {
		t.Errorf("restart_required = %v, want true (trust is applied when the daemon starts)", body["restart_required"])
	}
	if got := trustOnDisk(t, env.cfgPath, testServerAddr); !reflect.DeepEqual(got, []string{"CONTAINER"}) {
		t.Errorf("on-disk trust = %v, want [CONTAINER] (input token upper-cased)", got)
	}
	srv := body["servers"].([]any)[0].(map[string]any)
	if got := srv["trusted_runtimes"]; !reflect.DeepEqual(got, []any{"CONTAINER"}) {
		t.Errorf("response trusted_runtimes = %v, want [CONTAINER]", got)
	}
}

func TestUpdateConfig_TrustNarrow(t *testing.T) {
	env := setupTestEnv(t)
	setTrustOnDisk(t, env.cfgPath, []string{"CONTAINER", "NATIVE"})

	resp, body := putServers(t, env, `{"servers":[{"name":"test-server","trusted_runtimes":["Native"]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != true {
		t.Errorf("restart_required = %v, want true", body["restart_required"])
	}
	if got := trustOnDisk(t, env.cfgPath, testServerAddr); !reflect.DeepEqual(got, []string{"NATIVE"}) {
		t.Errorf("on-disk trust = %v, want [NATIVE] (the list replaces, it does not merge)", got)
	}
}

// An explicit empty list is the recorded "WASM only" decision: it must be
// saved as an empty list, never as an absent key the load-time migration
// would treat as a legacy blank.
func TestUpdateConfig_TrustNoneIsExplicitEmpty(t *testing.T) {
	env := setupTestEnv(t)
	setTrustOnDisk(t, env.cfgPath, []string{"CONTAINER"})

	resp, body := putServers(t, env, `{"servers":[{"name":"test-server","trusted_runtimes":[]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != true {
		t.Errorf("restart_required = %v, want true", body["restart_required"])
	}
	got := trustOnDisk(t, env.cfgPath, testServerAddr)
	if got == nil || len(got) != 0 {
		t.Errorf("on-disk trust = %#v, want an explicit empty list", got)
	}
}

// A PUT that does not mention trust leaves it exactly as it was — the PB-28
// guarantee — and needs no restart. Re-sending the current trust is not a
// change either.
func TestUpdateConfig_TrustAbsentIsUntouched(t *testing.T) {
	env := setupTestEnv(t)
	setTrustOnDisk(t, env.cfgPath, []string{"CONTAINER"})

	resp, body := putServers(t, env, `{"servers":[{"name":"test-server","weight":50}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != false {
		t.Errorf("restart_required = %v, want false when trust was not sent", body["restart_required"])
	}
	if got := trustOnDisk(t, env.cfgPath, testServerAddr); !reflect.DeepEqual(got, []string{"CONTAINER"}) {
		t.Errorf("on-disk trust = %v, want [CONTAINER] untouched", got)
	}
	loaded, _ := config.Load(env.cfgPath)
	if loaded.Servers[0].Weight != 50 {
		t.Errorf("weight = %d, want 50 (the unrelated field must still apply)", loaded.Servers[0].Weight)
	}

	resp, body = putServers(t, env, `{"servers":[{"name":"test-server","trusted_runtimes":["container"]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != false {
		t.Errorf("restart_required = %v, want false when the trust sent equals the trust on disk", body["restart_required"])
	}
}

func TestUpdateConfig_TrustInvalidTokenRejected(t *testing.T) {
	env := setupTestEnv(t)
	setTrustOnDisk(t, env.cfgPath, []string{"CONTAINER"})

	for _, body := range []string{
		`{"servers":[{"name":"test-server","trusted_runtimes":["bogus"]}]}`,
		`{"servers":[{"name":"test-server","trusted_runtimes":["wasm"]}]}`,
		`{"servers":[{"name":"test-server","trusted_runtimes":[1]}]}`,
		`{"servers":[{"name":"test-server","trusted_runtimes":"container"}]}`,
	} {
		resp, got := putServers(t, env, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %v", body, resp.StatusCode, got)
			continue
		}
		if errorCode(got) != "VALIDATION_ERROR" {
			t.Errorf("%s: error code = %v, want VALIDATION_ERROR", body, errorCode(got))
		}
	}
	if got := trustOnDisk(t, env.cfgPath, testServerAddr); !reflect.DeepEqual(got, []string{"CONTAINER"}) {
		t.Errorf("on-disk trust = %v after rejected updates, want [CONTAINER] untouched", got)
	}
}

// The PB-28 fixture: trust revoked on disk while the daemon still holds the
// wider boot-time trust. A PUT that carries trust re-widens it — that is the
// volunteer's explicit new decision — while the sibling test in
// daemon_bridge_trust_writeback_test.go guards that a PUT without trust
// does not.
func TestUpdateConfig_ExplicitTrustOverridesOnDiskRevocation(t *testing.T) {
	bridge, cfgPath := newTrustRevocationFixture(t)

	resp, err := bridge.UpdateConfig(map[string]any{
		"servers": []any{map[string]any{"name": "h1", "trusted_runtimes": []any{"container"}}},
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.RestartRequired {
		t.Error("RestartRequired = false, want true")
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !after.Servers[0].TrustsRuntime("CONTAINER") {
		t.Errorf("explicit trust in the request was not saved: %v", after.Servers[0].TrustedRuntimes)
	}
}

// --- POST /api/v1/leafs/attach: trusted_runtimes ---

func TestAttachLeaf_WithTrust(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach",
		`{"server_address":"h2.example.org:443","name":"h2","trusted_runtimes":["native","Container","container"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := trustOnDisk(t, env.cfgPath, "h2.example.org:443"); !reflect.DeepEqual(got, []string{"CONTAINER", "NATIVE"}) {
		t.Errorf("attached head trust = %v, want [CONTAINER NATIVE] (upper-cased, de-duplicated, sorted)", got)
	}
}

func TestAttachLeaf_WithoutTrustIsWASMOnly(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"server_address":"h2.example.org:443","name":"h2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	got := trustOnDisk(t, env.cfgPath, "h2.example.org:443")
	if got == nil || len(got) != 0 {
		t.Errorf("attached head trust = %#v, want an explicit empty list (WASM only)", got)
	}
}

func TestAttachLeaf_InvalidTrustRejectedAndNothingAttached(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach",
		`{"server_address":"h2.example.org:443","trusted_runtimes":["podman"]}`)
	body := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || errorCode(body) != "VALIDATION_ERROR" {
		t.Fatalf("expected 400 VALIDATION_ERROR, got %d: %v", resp.StatusCode, body)
	}
	cfg, _ := config.Load(env.cfgPath)
	for _, s := range cfg.Servers {
		if s.GRPCAddress == "h2.example.org:443" {
			t.Error("a head with an invalid trust list must not be attached")
		}
	}
}

// --- Version and update-required signals ---

func TestStatusCarriesClientVersion(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: testServerAddr, Name: "test-server", TrustedRuntimes: []string{}}}
	d := daemon.NewDaemon(daemon.DaemonConfig{Config: cfg, Logger: logger, ClientVersion: "1.2.3"})
	bridge := NewDaemonBridge(d, cfgPath)

	if got := bridge.GetStatus().ClientVersion; got != "1.2.3" {
		t.Errorf("client_version = %q, want 1.2.3", got)
	}

	// Over HTTP the field is always present, empty when the build carries no
	// version (tests, and any daemon started without one).
	env := setupTestEnv(t)
	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/status", ""))
	if v, ok := body["client_version"]; !ok || v != "" {
		t.Errorf("client_version = %v (present=%v), want an empty string present", v, ok)
	}
}

func TestHeadsCarriesHeadVersionAndUpdateRequired(t *testing.T) {
	env := setupTestEnv(t)

	head := func() map[string]any {
		body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
		return body["heads"].([]any)[0].(map[string]any)
	}

	h := head()
	if v, ok := h["head_version"]; !ok || v != "" {
		t.Errorf("fresh head_version = %v (present=%v), want an empty string present", v, ok)
	}
	if h["update_required"] != false {
		t.Errorf("fresh update_required = %v, want false", h["update_required"])
	}

	env.daemon.HeadStatus().SetVersion(testServerAddr, "v0.9.1")
	env.daemon.HeadStatus().MarkUpdateRequired(testServerAddr)
	h = head()
	if h["head_version"] != "v0.9.1" {
		t.Errorf("head_version = %v, want v0.9.1", h["head_version"])
	}
	if h["update_required"] != true {
		t.Errorf("update_required = %v, want true after a too-old rejection", h["update_required"])
	}

	env.daemon.HeadStatus().MarkContactOK(testServerAddr)
	h = head()
	if h["update_required"] != false {
		t.Errorf("update_required = %v, want false after a successful request to the head", h["update_required"])
	}
	if h["head_version"] != "v0.9.1" {
		t.Errorf("head_version = %v, want v0.9.1 kept across the clear", h["head_version"])
	}
}

// --- GET /api/v1/notices ---

func TestNoticesEndpoint(t *testing.T) {
	env := setupTestEnv(t)

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/notices", ""))
	if n, ok := body["notices"].([]any); !ok || len(n) != 0 {
		t.Errorf("fresh notices = %v, want an empty list", body["notices"])
	}
	if body["latest_id"] != float64(0) {
		t.Errorf("fresh latest_id = %v, want 0", body["latest_id"])
	}

	env.daemon.Notices().Notify(daemon.NoticeWarn, "update_required", "too old", "test-server", "")
	env.daemon.Notices().Notify(daemon.NoticeWarn, "leaf_failing", "keeps failing", "", "leaf-1")
	env.daemon.Notices().Notify(daemon.NoticeWarn, "leaf_failing", "keeps failing (again)", "", "leaf-1") // refresh, same id
	env.daemon.Notices().Notify(daemon.NoticeInfo, "thermal_throttle", "released", "", "")

	body = decodeJSON(t, env.doRequest(t, "GET", "/api/v1/notices", ""))
	notices := body["notices"].([]any)
	if len(notices) != 3 {
		t.Fatalf("got %d notices, want 3 (the repeat refreshed its notice): %v", len(notices), notices)
	}
	if body["latest_id"] != float64(3) {
		t.Errorf("latest_id = %v, want 3", body["latest_id"])
	}
	newest := notices[0].(map[string]any)
	if newest["code"] != "thermal_throttle" || newest["level"] != "info" {
		t.Errorf("newest = %v, want the thermal_throttle info notice first", newest)
	}
	for _, raw := range notices {
		n := raw.(map[string]any)
		for _, key := range []string{"id", "level", "code", "message", "count", "first_at", "at"} {
			if _, ok := n[key]; !ok {
				t.Errorf("notice %v lacks %q", n, key)
			}
		}
		switch n["code"] {
		case "update_required":
			if n["head"] != "test-server" {
				t.Errorf("update_required head = %v, want test-server", n["head"])
			}
			if _, ok := n["leaf"]; ok {
				t.Errorf("update_required must omit an empty leaf: %v", n)
			}
		case "leaf_failing":
			if n["leaf"] != "leaf-1" || n["count"] != float64(2) || n["message"] != "keeps failing (again)" {
				t.Errorf("leaf_failing = %v, want leaf-1 with count 2 and the refreshed message", n)
			}
		}
	}

	body = decodeJSON(t, env.doRequest(t, "GET", "/api/v1/notices?since=2", ""))
	notices = body["notices"].([]any)
	if len(notices) != 1 || notices[0].(map[string]any)["id"] != float64(3) {
		t.Errorf("since=2 returned %v, want only id 3", notices)
	}
	if body["latest_id"] != float64(3) {
		t.Errorf("since=2 latest_id = %v, want 3", body["latest_id"])
	}

	resp := env.doRequest(t, "GET", "/api/v1/notices?since=abc", "")
	body = decodeJSON(t, resp)
	if resp.StatusCode != http.StatusBadRequest || errorCode(body) != "VALIDATION_ERROR" {
		t.Errorf("since=abc: expected 400 VALIDATION_ERROR, got %d: %v", resp.StatusCode, body)
	}
}

// The daemon-side emission sites reachable without a running work loop: the
// leaf failure breaker (c) must land in the ring with the leaf named.
func TestNotices_LeafBreakerTripEmits(t *testing.T) {
	env := setupTestEnv(t)

	for i := 0; i < 3; i++ {
		env.daemon.RecordLeafFailureForTest("leaf-broken", "non-zero exit code 2")
	}
	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/notices", ""))
	notices := body["notices"].([]any)
	if len(notices) != 1 {
		t.Fatalf("got %d notices, want exactly 1 leaf_failing when the breaker trips: %v", len(notices), notices)
	}
	n := notices[0].(map[string]any)
	if n["code"] != "leaf_failing" || n["leaf"] != "leaf-broken" || n["level"] != "warn" {
		t.Errorf("notice = %v, want leaf_failing/warn for leaf-broken", n)
	}
	if msg, _ := n["message"].(string); !strings.Contains(msg, "non-zero exit code 2") {
		t.Errorf("message should carry the last failure reason: %q", msg)
	}
}
