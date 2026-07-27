package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/spf13/cobra"
)

// TB-6 / TB-8 / TB-9 regression coverage for what the CLI tells the volunteer.
//
// TB-6: a typo'd subcommand was reported as a bad FLAG — `schedule ad --from
// 03:00` failed with "unknown flag: --from" — because cobra parses flags before
// it validates positional arguments and only rejects unknown subcommands on the
// root command. The volunteer was steered into "fixing" flags that were correct.
//
// TB-8: the `heads` parent enumerated its subcommands as "(list, weight)" in
// top-level help, an enumeration that went stale when `trust` was added and so
// hid the security-critical command from anyone scanning for it.
//
// TB-9: `leafs weight` printed the raw map value for the previous weight, so a
// first-time set claimed "0 → 200" when the leaf had in fact been running at the
// effective default of 100.

// captureStdout collects everything fn writes to os.Stdout. The CLI's
// user-facing output goes to fmt.Print* rather than through cobra's out writer,
// so the stream itself has to be swapped.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	collected := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		collected <- buf.String()
	}()

	fn()

	os.Stdout = orig
	w.Close()
	out := <-collected
	r.Close()
	return out
}

// --- TB-6 ---

// TestTypodSubcommandWithFlagsNamesTheCommand is the filed repro: the error must
// blame `ad`, not the perfectly good `--from`.
func TestTypodSubcommandWithFlagsNamesTheCommand(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	err := runCLI(t, "schedule", "ad", "--from", "03:00", "--to", "01:00", "--days", "fri-sat",
		"--config", cfgFile, "--data-dir", dir)
	if err == nil {
		t.Fatal("`schedule ad --from …` succeeded; expected an unknown-command error")
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown flag") {
		t.Errorf("error blames a flag rather than the mistyped subcommand: %v", err)
	}
	if !strings.Contains(msg, `unknown command "ad"`) {
		t.Errorf("error does not name the unknown command: %v", err)
	}
	if !strings.Contains(msg, "add") {
		t.Errorf("error does not suggest `add`: %v", err)
	}
}

// TestTypodSubcommandWithoutFlagsIsRejected covers the same mistake with no
// flags to be blamed instead: it used to be swallowed as a positional argument
// and the parent's own output printed as if nothing were wrong.
func TestTypodSubcommandWithoutFlagsIsRejected(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	cases := []struct{ parent, typo string }{
		{"schedule", "ad"},
		{"config", "se"},
		{"heads", "trus"},
		{"leafs", "lis"},
	}
	for _, tc := range cases {
		err := runCLI(t, tc.parent, tc.typo, "--config", cfgFile, "--data-dir", dir)
		if err == nil {
			t.Errorf("`%s %s` succeeded; expected an unknown-command error", tc.parent, tc.typo)
			continue
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("`%s %s`: %v, want an unknown-command error", tc.parent, tc.typo, err)
		}
	}
}

// TestParentCommandsStillRunBare guards the fix: the runnable parents must keep
// working with no arguments, which is how `schedule` and `config` show state.
func TestParentCommandsStillRunBare(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeDefaultConfig(t, dir)

	for _, parent := range []string{"schedule", "config", "heads", "leafs"} {
		if err := runCLI(t, parent, "--config", cfgFile, "--data-dir", dir); err != nil {
			t.Errorf("bare `%s` failed: %v", parent, err)
		}
	}
	// And the real subcommands still resolve.
	if err := runCLI(t, "schedule", "show", "--config", cfgFile, "--data-dir", dir); err != nil {
		t.Errorf("`schedule show` failed: %v", err)
	}
}

// --- TB-8 ---

// TestHeadsShortHelpMentionsTrust: `trust` is where a volunteer grants a head
// the right to run unsandboxed code on their machine. Top-level help must not
// describe `heads` in a way that implies it is not there.
func TestHeadsShortHelpMentionsTrust(t *testing.T) {
	root := newRootCmd()
	var heads *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "heads" {
			heads = c
			break
		}
	}
	if heads == nil {
		t.Fatal("no `heads` command on the root")
	}
	if !strings.Contains(strings.ToLower(heads.Short), "trust") {
		t.Errorf("heads Short = %q; top-level help must mention trust", heads.Short)
	}
	// The stale enumeration is what went out of date in the first place.
	if strings.Contains(heads.Short, "(list, weight)") {
		t.Errorf("heads Short still carries the stale subcommand enumeration: %q", heads.Short)
	}
}

// --- TB-9 ---

// TestLeafsWeightShowsEffectiveDefaultAsOldValue: setting a weight for the first
// time must report the weight the leaf actually had (the default 100), not the
// zero value of a missing map key.
func TestLeafsWeightShowsEffectiveDefaultAsOldValue(t *testing.T) {
	dir := t.TempDir()
	c := config.Defaults()
	c.DataDir = dir
	c.Servers = []config.ServerConfig{{
		GRPCAddress: "head.example:9091",
		Name:        "head.example",
	}}
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := c.Save(cfgFile); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runCLI(t, "leafs", "weight", "beyblade-arena", "200",
			"--server", "head.example", "--config", cfgFile, "--data-dir", dir)
	})
	if runErr != nil {
		t.Fatalf("leafs weight: %v", runErr)
	}

	if strings.Contains(out, ": 0 → 200") {
		t.Errorf("first-time weight set reported the leaf as previously weighted 0:\n%s", out)
	}
	if !strings.Contains(out, "100 → 200") {
		t.Errorf("output does not report the effective default as the old weight:\n%s", out)
	}

	// The stored weight is the point of the command and must be unaffected.
	saved, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Servers[0].LeafPreferences.Weights["beyblade-arena"]; got != 200 {
		t.Errorf("stored weight = %d, want 200", got)
	}
}
