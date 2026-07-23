package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PB-28 regression coverage: an explicitly-empty per-head trust list (the
// volunteer chose `--trust none` / `heads trust <head> none`) must survive a
// config save/load cycle. Before the fix the empty list was dropped from the
// file (omitempty), so the next Load could not tell it from a legacy
// pre-per-head-trust config and re-seeded it from available_runtimes — which
// init populates with CONTAINER on any podman/docker host — silently upgrading
// a deliberate no-trust choice to CONTAINER trust.

func TestExplicitTrustNone_SurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.DataDir = dir
	// What init writes on a container-capable host — the seed source that
	// overrode the explicit choice pre-fix.
	cfg.AvailableRuntimes = []string{"WASM", "CONTAINER"}
	cfg.Servers = []ServerConfig{{
		GRPCAddress: "head1.example.com:443",
		Name:        "head1.example.com",
		// attach --trust none: an explicit, deliberate "WASM only".
		TrustedRuntimes: []string{},
	}}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(loaded.Servers))
	}
	srv := loaded.Servers[0]
	if srv.TrustsRuntime("CONTAINER") {
		t.Errorf("explicit --trust none was upgraded to CONTAINER trust across a reload (PB-28); trusted = %v",
			srv.EffectiveTrustedRuntimes())
	}
	if !srv.TrustsRuntime("WASM") {
		t.Errorf("WASM must stay implicitly trusted; trusted = %v", srv.EffectiveTrustedRuntimes())
	}
}

// TestLegacyTrustUnset_StillMigrates is the control: a config whose server
// entry has NO trusted_runtimes key at all (written before per-head trust
// existed) must still be seeded from the legacy global knobs, exactly as
// before, so an upgraded volunteer keeps its posture.
func TestLegacyTrustUnset_StillMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Join([]string{
		"available_runtimes: [WASM, CONTAINER]",
		"servers:",
		"  - grpc_address: head1.example.com:443",
		"    name: head1.example.com",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	srv := loaded.Servers[0]
	if !srv.TrustsRuntime("CONTAINER") {
		t.Errorf("legacy entry (no trusted_runtimes key) must migrate from available_runtimes; trusted = %v",
			srv.EffectiveTrustedRuntimes())
	}

	// The migration result must be pinned explicitly on the next save so a later
	// load takes the explicit-choice path, not the migration.
	if err := loaded.Save(path); err != nil {
		t.Fatalf("Save after migration: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(data), "trusted_runtimes") {
		t.Error("migrated trust was not persisted explicitly on save")
	}
}
