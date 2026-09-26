package management

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// The app's "Use Defaults" puts a head's leaves back on its defaults the way
// `lettuce-volunteer leafs reset` does. It sends an explicit empty weights map,
// and the merge must clear the saved weights, which a body without the key
// leaves in place (that is how a checkbox keeps them). The head's own weight
// is not the button's to change.
func TestApplyServers_EmptyWeightsClearsLeafWeights(t *testing.T) {
	cfg := config.Defaults()
	cfg.Servers = []config.ServerConfig{{
		Name: "lettuce.science", GRPCAddress: "lettuce.science:443", Weight: 200,
		LeafPreferences: config.LeafPreferences{Mode: "SPECIFIC", Enabled: []string{"a"}, Weights: map[string]int{"a": 200, "b": 50}},
	}}

	keep := []any{map[string]any{"name": "lettuce.science", "leaf_preferences": map[string]any{"mode": "SPECIFIC", "enabled": []any{"a", "b"}}}}
	if _, err := applyServers(cfg, keep); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Servers[0].LeafPreferences.Weights; len(got) != 2 {
		t.Fatalf("a body without weights changed them to %v; a checkbox must keep them", got)
	}

	reset := []any{map[string]any{"name": "lettuce.science", "leaf_preferences": map[string]any{"mode": "ALL", "weights": map[string]any{}}}}
	if _, err := applyServers(cfg, reset); err != nil {
		t.Fatal(err)
	}
	lp := cfg.Servers[0].LeafPreferences
	if lp.Mode != "ALL" || len(lp.Enabled) != 0 || len(lp.Weights) != 0 {
		t.Errorf("after Use Defaults: mode=%q enabled=%v weights=%v, want ALL with no enabled list and no weights", lp.Mode, lp.Enabled, lp.Weights)
	}
	if cfg.Servers[0].Weight != 200 {
		t.Errorf("head weight = %d, want 200 (Use Defaults leaves the head's weight alone)", cfg.Servers[0].Weight)
	}
}
