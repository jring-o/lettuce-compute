package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression tests for TB-65: the desktop app could not unselect a head's
// last enabled leaf. Unchecking it writes `{mode: SPECIFIC, enabled: []}`,
// which Validate refused ("SPECIFIC mode requires at least one enabled leaf"),
// so the app's save failed and the box snapped back — while the CLI's `leafs
// disable` wrote the very same state for the last leaf (Save never validates)
// and the daemon ran it as "ask this head for nothing". The state is
// legitimate and the rule is gone.

// TestTB65_ValidateAcceptsSpecificWithNoEnabledLeaves is the exact body the
// app sends for the last unchecked box.
func TestTB65_ValidateAcceptsSpecificWithNoEnabledLeaves(t *testing.T) {
	for _, enabled := range [][]string{nil, {}} {
		cfg := Defaults()
		cfg.Servers = []ServerConfig{{
			GRPCAddress:     "infra.example.org:443",
			Name:            "infra",
			LeafPreferences: LeafPreferences{Mode: "SPECIFIC", Enabled: enabled},
		}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("enabled=%#v: Validate() = %v, want nil (SPECIFIC with no leaf selects nothing, a valid choice)", enabled, err)
		}
	}
}

// TestTB65_SpecificWithNoEnabledLeavesRoundTripsThroughSaveAndLoad pins that
// the state survives the file: what `leafs disable` writes for the last leaf
// is what the daemon (and the app's next PUT, which re-validates the whole
// config) reads back.
func TestTB65_SpecificWithNoEnabledLeavesRoundTripsThroughSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.DataDir = dir
	cfg.Servers = []ServerConfig{{
		GRPCAddress:     "infra.example.org:443",
		Name:            "infra",
		LeafPreferences: LeafPreferences{Mode: "SPECIFIC", Enabled: []string{}},
	}}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	lp := loaded.Servers[0].LeafPreferences
	if lp.Mode != "SPECIFIC" || len(lp.Enabled) != 0 {
		t.Fatalf("loaded leaf_preferences = %+v, want SPECIFIC with no enabled leaf", lp)
	}
	if err := loaded.Validate(); err != nil {
		t.Errorf("loaded config Validate() = %v, want nil", err)
	}
}
