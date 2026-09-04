package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestTB65_LeafsDisableLastEnabledLeafLeavesAValidConfig: `leafs disable` on
// the last enabled leaf of a SPECIFIC-mode head writes SPECIFIC with no
// enabled leaf. `Save` never validated, so the command always succeeded — but
// the state it left failed `Validate`, which every later config write from
// the desktop app runs, so each of those was refused with "SPECIFIC mode
// requires at least one enabled leaf" (TB-65). The state is now valid: the
// head stays attached and is asked for nothing, and the command says so.
func TestTB65_LeafsDisableLastEnabledLeafLeavesAValidConfig(t *testing.T) {
	dir := t.TempDir()
	c := config.Defaults()
	c.DataDir = dir
	c.Servers = []config.ServerConfig{{
		GRPCAddress: "head.example:9091",
		Name:        "head.example",
		LeafPreferences: config.LeafPreferences{
			Mode:    "SPECIFIC",
			Enabled: []string{"grep-f13"},
		},
	}}
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := c.Save(cfgFile); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runCLI(t, "leafs", "disable", "grep-f13",
			"--server", "head.example", "--config", cfgFile, "--data-dir", dir)
	})
	if runErr != nil {
		t.Fatalf("leafs disable: %v", runErr)
	}
	if !strings.Contains(out, "asked for no work") {
		t.Errorf("output does not say the head now gets nothing:\n%s", out)
	}

	saved, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	lp := saved.Servers[0].LeafPreferences
	if lp.Mode != "SPECIFIC" || len(lp.Enabled) != 0 {
		t.Fatalf("saved leaf_preferences = %+v, want SPECIFIC with no enabled leaf", lp)
	}
	if err := saved.Validate(); err != nil {
		t.Errorf("the config `leafs disable` left behind fails Validate: %v (every later app save would be refused)", err)
	}
}
