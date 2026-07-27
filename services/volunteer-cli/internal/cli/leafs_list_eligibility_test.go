package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Regression tests for TB-4 — `leafs list` shows no runtime and no eligibility,
// so an untrusted native leaf looks fetchable.
//
// The tester's report: "leafs list does not show trust … simply show all
// active." On a machine trusted for CONTAINER only, a NATIVE leaf printed
// `active ✓` — identical to the container leaf that machine was actually
// computing — with nothing to say it would never be fetched. STATE is the
// HEAD's lifecycle state and ENABLED is the volunteer's own preference toggle;
// neither answers "will my machine run this?", which is what the table looked
// like it was answering.

// rowFor returns the table row whose SLUG cell matches slug, or "" if absent.
// The table is tab-padded, so cells are matched on whitespace-split fields.
func rowFor(out, slug string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == slug {
			return line
		}
	}
	return ""
}

// cell returns the n-th whitespace-separated cell of a row (0-based).
func cell(row string, n int) string {
	fields := strings.Fields(row)
	if n >= len(fields) {
		return ""
	}
	return fields[n]
}

// containerOnlyMachine reproduces the tester's setup: a container backend is
// available, the head is trusted for CONTAINER but NOT for NATIVE.
func containerOnlyMachine() (leafsAPIMachine, []config.ServerConfig) {
	machine := leafsAPIMachine{
		Runtimes:    []string{"container", "native", "wasm"},
		MaxMemoryMB: 16384,
	}
	servers := []config.ServerConfig{{
		Name:            "lbry.science",
		GRPCAddress:     "lbry.science:443",
		TrustedRuntimes: []string{"CONTAINER"},
	}}
	return machine, servers
}

// twoLeafHead offers one CONTAINER leaf and one NATIVE leaf, both ACTIVE and
// both enabled by the volunteer — the exact pair that displayed identically.
func twoLeafHead() leafsAPIHead {
	return leafsAPIHead{
		Name:        "lbry.science",
		GRPCAddress: "lbry.science:443",
		Leafs: []leafsAPILeaf{
			{
				Slug: "beyblade-arena", Name: "BB-A", State: "ACTIVE", Enabled: true,
				ExecutionSpec: &leafsAPIExecutionSpec{Image: "ghcr.io/lettuce/beyblade:1"},
			},
			{
				Slug: "beyblade-arena-native", Name: "BB-A-native", State: "ACTIVE", Enabled: true,
				ExecutionSpec: &leafsAPIExecutionSpec{Binaries: map[string]string{"linux_amd64": "https://example.invalid/bb"}},
			},
		},
	}
}

// TestLeafsListSeparatesAFetchableLeafFromAnUntrustedOne is the core TB-4
// regression: the two leafs the tester saw as identical must now differ, and the
// native one must say why it will never be fetched.
func TestLeafsListSeparatesAFetchableLeafFromAnUntrustedOne(t *testing.T) {
	machine, servers := containerOnlyMachine()
	resp := &leafsAPIResponse{Heads: []leafsAPIHead{twoLeafHead()}, Machine: machine}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()

	header := strings.Split(out, "\n")[0]
	for _, col := range []string{"RUNTIME", "WILL FETCH"} {
		if !strings.Contains(header, col) {
			t.Fatalf("header %q is missing the %s column — the table still cannot answer whether this machine runs a leaf", header, col)
		}
	}

	containerRow := rowFor(out, "beyblade-arena")
	nativeRow := rowFor(out, "beyblade-arena-native")
	if containerRow == "" || nativeRow == "" {
		t.Fatalf("expected a row for each leaf; got:\n%s", out)
	}
	if containerRow == nativeRow {
		t.Fatalf("the two leafs still render identically:\n%s", out)
	}

	// RUNTIME is column index 3 (SERVER SLUG NAME RUNTIME ...).
	if got := cell(containerRow, 3); got != "CONTAINER" {
		t.Errorf("container leaf RUNTIME = %q, want CONTAINER (row: %s)", got, containerRow)
	}
	if got := cell(nativeRow, 3); got != "NATIVE" {
		t.Errorf("native leaf RUNTIME = %q, want NATIVE (row: %s)", got, nativeRow)
	}

	if !strings.HasSuffix(strings.TrimSpace(containerRow), "yes") {
		t.Errorf("container leaf should be marked fetchable; row: %s", containerRow)
	}
	if !strings.HasSuffix(strings.TrimSpace(nativeRow), "no") {
		t.Errorf("native leaf should be marked NOT fetchable on a container-only-trusted machine; row: %s", nativeRow)
	}

	// The reason has to name the remedy, or the column just moves the confusion.
	if !strings.Contains(out, "heads trust lbry.science native") {
		t.Errorf("output does not tell the volunteer how to make the native leaf fetchable:\n%s", out)
	}
}

// TestLeafsListRuntimeColumnFollowsDispatchPrecedence pins the RUNTIME label to
// the same precedence the fetcher uses to select a runtime (container if an
// image is set; else wasm if a wasm build exists; else native). A label derived
// any other way would be a second, drifting answer to a question dispatch
// already decides.
func TestLeafsListRuntimeColumnFollowsDispatchPrecedence(t *testing.T) {
	cases := []struct {
		name string
		spec *leafsAPIExecutionSpec
		want string
	}{
		{"image wins over binaries", &leafsAPIExecutionSpec{
			Image:    "ghcr.io/x/y:1",
			Binaries: map[string]string{"linux_amd64": "u", "wasm": "u"},
		}, "CONTAINER"},
		{"wasm build", &leafsAPIExecutionSpec{Binaries: map[string]string{"wasm": "u"}}, "WASM"},
		{"wasm preferred over native", &leafsAPIExecutionSpec{
			Binaries: map[string]string{"wasm": "u", "linux_amd64": "u"},
		}, "WASM"},
		{"native binaries", &leafsAPIExecutionSpec{Binaries: map[string]string{"linux_amd64": "u"}}, "NATIVE"},
		{"no spec at all", nil, "NATIVE"},
	}

	machine := leafsAPIMachine{Runtimes: []string{"container", "native", "wasm"}, MaxMemoryMB: 16384}
	servers := []config.ServerConfig{{
		Name: "h", GRPCAddress: "h:443", TrustedRuntimes: []string{"CONTAINER", "NATIVE"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &leafsAPIResponse{
				Machine: machine,
				Heads: []leafsAPIHead{{
					Name: "h", GRPCAddress: "h:443",
					Leafs: []leafsAPILeaf{{Slug: "l", Name: "L", State: "ACTIVE", Enabled: true, ExecutionSpec: c.spec}},
				}},
			}
			var buf bytes.Buffer
			printLeafsTable(&buf, resp, servers)
			if got := cell(rowFor(buf.String(), "l"), 3); got != c.want {
				t.Errorf("RUNTIME = %q, want %q\n%s", got, c.want, buf.String())
			}
		})
	}
}

// TestLeafsListMarksALeafPausedByRepeatedFailures joins TB-4 to TB-10: a leaf
// this machine CAN run, and is being sent work for, but whose work fails here
// every time, is neither "yes" nor a capability problem. It has to read as its
// own state, because the action it calls for is different.
func TestLeafsListMarksALeafPausedByRepeatedFailures(t *testing.T) {
	machine := leafsAPIMachine{Runtimes: []string{"native"}, MaxMemoryMB: 16384}
	servers := []config.ServerConfig{{
		Name: "h", GRPCAddress: "h:443", TrustedRuntimes: []string{"NATIVE"},
	}}
	resp := &leafsAPIResponse{
		Machine: machine,
		Heads: []leafsAPIHead{{
			Name: "h", GRPCAddress: "h:443",
			Leafs: []leafsAPILeaf{{
				Slug: "broken", Name: "Broken", State: "ACTIVE", Enabled: true,
				ExecutionSpec: &leafsAPIExecutionSpec{Binaries: map[string]string{"linux_amd64": "u"}},
				Failures:      &leafsAPILeafFailures{ConsecutiveFailures: 3, TotalFailures: 17, Paused: true},
			}},
		}},
	}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()

	row := rowFor(out, "broken")
	if !strings.HasSuffix(strings.TrimSpace(row), "paused") {
		t.Errorf("a leaf paused by repeated local failures should say so; row: %s", row)
	}
	if !strings.Contains(out, "failed 3 times in a row") {
		t.Errorf("output does not explain the pause:\n%s", out)
	}
}

// TestLeafsListReportsTheVolunteersOwnChoicesAsSuch keeps the column honest in
// the other direction: a leaf the volunteer disabled, or one the head has
// paused, is not an eligibility failure and must not be reported as one.
func TestLeafsListReportsTheVolunteersOwnChoicesAsSuch(t *testing.T) {
	machine := leafsAPIMachine{Runtimes: []string{"container", "native"}, MaxMemoryMB: 16384}
	servers := []config.ServerConfig{{
		Name: "h", GRPCAddress: "h:443", TrustedRuntimes: []string{"CONTAINER", "NATIVE"},
	}}
	spec := &leafsAPIExecutionSpec{Binaries: map[string]string{"linux_amd64": "u"}}
	resp := &leafsAPIResponse{
		Machine: machine,
		Heads: []leafsAPIHead{{
			Name: "h", GRPCAddress: "h:443",
			Leafs: []leafsAPILeaf{
				{Slug: "off", Name: "Off", State: "ACTIVE", Enabled: false, ExecutionSpec: spec},
				{Slug: "paused-head", Name: "HeadPaused", State: "PAUSED", Enabled: true, ExecutionSpec: spec},
			},
		}},
	}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()

	if !strings.HasSuffix(strings.TrimSpace(rowFor(out, "off")), "no") {
		t.Errorf("a disabled leaf will not be fetched; row: %s", rowFor(out, "off"))
	}
	if !strings.Contains(out, "you disabled this leaf") {
		t.Errorf("a leaf the volunteer disabled should say so, not blame capability:\n%s", out)
	}
	if !strings.Contains(out, "state PAUSED") {
		t.Errorf("a leaf the head has paused should say so:\n%s", out)
	}
}

// TestLeafsListFallsBackHonestlyWithoutADaemon: without the daemon there is no
// live head state, so the config-only table must say what it cannot show rather
// than letting its columns be read as the whole answer.
func TestLeafsListFallsBackHonestlyWithoutADaemon(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })
	cfg = config.Defaults()
	cfg.Servers = []config.ServerConfig{{Name: "h", GRPCAddress: "h:443"}}

	out := captureStdout(t, func() {
		if err := printLeafsFromConfig(); err != nil {
			t.Fatalf("printLeafsFromConfig: %v", err)
		}
	})

	for _, want := range []string{"runtime", "actually fetch"} {
		if !strings.Contains(out, want) {
			t.Errorf("config-only fallback does not disclaim %q:\n%s", want, out)
		}
	}
}
