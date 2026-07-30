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
// pre-per-head-trust config and re-seeded it from the then-live global
// available_runtimes key — which init populated with CONTAINER on any
// podman/docker host — silently upgrading a deliberate no-trust choice to
// CONTAINER trust. (The global key is retired now, TB-25; the explicit-empty
// round-trip property stands on its own.)

func TestExplicitTrustNone_SurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.DataDir = dir
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

// TestLegacyTrustUnset_PinnedOnSave: a config whose server entry has NO
// trusted_runtimes key at all (written before per-head trust existed) is pinned
// to WASM-only on load (TB-25 — the retired global keys never grant trust), and
// the pinned decision must be persisted explicitly on the next save so a later
// load takes the explicit-choice path, not the migration.
func TestLegacyTrustUnset_PinnedOnSave(t *testing.T) {
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
	if srv.TrustsRuntime("CONTAINER") {
		t.Errorf("legacy entry (no trusted_runtimes key) must NOT gain trust from the retired available_runtimes key; trusted = %v",
			srv.EffectiveTrustedRuntimes())
	}
	if !srv.TrustsRuntime("WASM") {
		t.Errorf("WASM must stay implicitly trusted; trusted = %v", srv.EffectiveTrustedRuntimes())
	}

	if err := loaded.Save(path); err != nil {
		t.Fatalf("Save after migration: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(data), "trusted_runtimes") {
		t.Error("pinned trust was not persisted explicitly on save")
	}
}
