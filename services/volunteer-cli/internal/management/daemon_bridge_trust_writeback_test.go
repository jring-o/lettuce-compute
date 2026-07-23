package management

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// PB-28 regression coverage (bridge write-back path): the management API's
// config writes (POST /api/v1/config and the attach/detach endpoints, used by
// the desktop app) must persist changes on top of the CURRENT on-disk config,
// never on top of the daemon's boot-time in-memory snapshot. The two diverge
// whenever the CLI edits config.yaml while the daemon runs — most critically
// `heads trust <head> none`, which revokes runtime trust on disk and tells the
// user to restart. Before the fix, any bridge write saved the stale snapshot
// whole, silently reverting the revocation to the old wider trust.

// newTrustRevocationFixture boots a daemon whose in-memory config trusts head
// h1 for CONTAINER, then revokes that trust on disk exactly the way
// `heads trust h1 none` does (load, set explicit-empty, save) WITHOUT
// restarting the daemon. Returns the bridge and the config path; extraServers
// are appended to the seed config before the daemon boots.
func newTrustRevocationFixture(t *testing.T, extraServers ...config.ServerConfig) (*DaemonBridge, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	seed := config.Defaults()
	seed.DataDir = dir
	// What init writes on a container-capable host.
	seed.AvailableRuntimes = []string{"WASM", "CONTAINER"}
	seed.ContainerBackend = "podman"
	seed.Servers = append([]config.ServerConfig{{
		GRPCAddress:     "h1.example.org:443",
		Name:            "h1",
		TrustedRuntimes: []string{"CONTAINER"},
	}}, extraServers...)
	if err := seed.Save(cfgPath); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	bootCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("boot Load: %v", err)
	}
	d := daemon.NewDaemon(daemon.DaemonConfig{Config: bootCfg, Logger: logger})
	bridge := NewDaemonBridge(d, cfgPath)

	// While the daemon runs: `heads trust h1 none` (disk-only; the CLI prints
	// "Restart the daemon for the change to take effect").
	cliCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("CLI Load: %v", err)
	}
	cliCfg.Servers[0].TrustedRuntimes = []string{}
	if err := cliCfg.Save(cfgPath); err != nil {
		t.Fatalf("CLI Save: %v", err)
	}

	// Sanity: the revocation landed on disk before the bridge writes.
	onDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("sanity Load: %v", err)
	}
	if onDisk.Servers[0].TrustsRuntime("CONTAINER") {
		t.Fatal("fixture broken: disk should be WASM-only before the bridge write")
	}
	return bridge, cfgPath
}

// requireH1TrustStillRevoked reloads the config file and fails the test if head
// h1's explicit trust-none was widened back (the PB-28 consent harm).
func requireH1TrustStillRevoked(t *testing.T, cfgPath, via string) *config.Config {
	t.Helper()
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	for _, srv := range after.Servers {
		if srv.GRPCAddress != "h1.example.org:443" {
			continue
		}
		if srv.TrustsRuntime("CONTAINER") {
			t.Fatalf("PB-28: `heads trust none` was silently reverted by %s — head %q trusts %v again "+
				"(the bridge persisted the daemon's stale boot-time trust over the on-disk revocation)",
				via, srv.DisplayName(), srv.EffectiveTrustedRuntimes())
		}
		if srv.TrustedRuntimes == nil {
			t.Fatalf("head %q trust became nil after %s; the explicit empty list must stay explicit "+
				"or the next load re-seeds it from available_runtimes", srv.DisplayName(), via)
		}
		return after
	}
	t.Fatalf("head h1 missing from config after %s", via)
	return nil
}

// The mandated PB-28 bridge-revert reproduction: revoke trust on disk, then
// flip an UNRELATED setting through POST /api/v1/config. The revocation must
// survive and the unrelated change must still apply.
func TestUpdateConfig_PreservesOnDiskTrustRevocation(t *testing.T) {
	bridge, cfgPath := newTrustRevocationFixture(t)

	resp, err := bridge.UpdateConfig(map[string]any{"log_level": "debug"})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.LogLevel != "debug" {
		t.Errorf("response log_level = %q, want %q", resp.LogLevel, "debug")
	}

	after := requireH1TrustStillRevoked(t, cfgPath, "POST /api/v1/config")
	if after.LogLevel != "debug" {
		t.Errorf("on-disk log_level = %q, want %q (the unrelated update must still persist)",
			after.LogLevel, "debug")
	}
}

// Attaching a NEW head through the bridge must not re-persist stale trust for
// an existing head, and the new head must come up WASM-only (there was no
// consent step to grant it more).
func TestAttachLeaf_PreservesOnDiskTrustRevocation(t *testing.T) {
	bridge, cfgPath := newTrustRevocationFixture(t)

	if err := bridge.AttachLeaf(AttachRequest{ServerAddress: "h2.example.org:443", LeafID: "leaf-1"}); err != nil {
		t.Fatalf("AttachLeaf: %v", err)
	}

	after := requireH1TrustStillRevoked(t, cfgPath, "POST /api/v1/leafs/attach")
	var attached *config.ServerConfig
	for i := range after.Servers {
		if after.Servers[i].GRPCAddress == "h2.example.org:443" {
			attached = &after.Servers[i]
		}
	}
	if attached == nil {
		t.Fatal("attached head missing from saved config")
	}
	if attached.TrustsRuntime("CONTAINER") || attached.TrustsRuntime("NATIVE") {
		t.Errorf("bridge-attached head was granted %v with no consent step",
			attached.EffectiveTrustedRuntimes())
	}
}

// Detaching one head through the bridge must not re-persist stale trust for
// the heads that remain.
func TestDetachLeaf_PreservesOnDiskTrustRevocation(t *testing.T) {
	bridge, cfgPath := newTrustRevocationFixture(t, config.ServerConfig{
		GRPCAddress:     "h2.example.org:443",
		Name:            "h2",
		TrustedRuntimes: []string{},
	})

	if err := bridge.DetachLeaf(DetachRequest{ServerAddress: "h2.example.org:443"}); err != nil {
		t.Fatalf("DetachLeaf: %v", err)
	}

	after := requireH1TrustStillRevoked(t, cfgPath, "POST /api/v1/leafs/detach")
	for _, srv := range after.Servers {
		if srv.GRPCAddress == "h2.example.org:443" {
			t.Errorf("detached head %q still present", srv.DisplayName())
		}
	}
}

// With no config file on disk there is no disk-side decision to preserve; the
// bridge falls back to the daemon's in-memory config as the write base instead
// of erroring (or silently writing bare defaults that would drop the server
// list).
func TestUpdateConfig_MissingFileFallsBackToLiveConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bootCfg := config.Defaults()
	bootCfg.DataDir = dir
	bootCfg.Servers = []config.ServerConfig{{
		GRPCAddress:     "h1.example.org:443",
		Name:            "h1",
		TrustedRuntimes: []string{},
	}}
	d := daemon.NewDaemon(daemon.DaemonConfig{Config: bootCfg, Logger: logger})
	bridge := NewDaemonBridge(d, cfgPath)

	if _, err := bridge.UpdateConfig(map[string]any{"log_level": "warn"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(after.Servers) != 1 || after.Servers[0].GRPCAddress != "h1.example.org:443" {
		t.Fatalf("server list not carried from the live config; servers = %+v", after.Servers)
	}
	if after.LogLevel != "warn" {
		t.Errorf("log_level = %q, want %q", after.LogLevel, "warn")
	}
}
