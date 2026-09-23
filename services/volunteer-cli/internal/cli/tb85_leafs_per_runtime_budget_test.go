package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TB-85 regression test, diagnostics half.
//
// `leafs list` (and `doctor`, through the same classifier) judged every leaf
// against the daemon's one memory figure — on a Mac, the container engine
// VM's budget — so a native leaf the machine could run in its own RAM was
// reported "WILL FETCH no" with a remedy to enlarge a VM it never runs in.
// The machine record now carries the host budgets beside the container ones,
// and each leaf is judged against the budget of its runtime.

// TestTB85_LeafsTableJudgesEachLeafByItsRuntimeBudget: on the Mac mini
// (container budget 768 MB, limit 1,024), a 900 MB native leaf will be
// fetched and a 900 MB container leaf will not, with the VM named as the
// reason. The machine record is decoded from the management API's JSON, as
// the command reads it.
func TestTB85_LeafsTableJudgesEachLeafByItsRuntimeBudget(t *testing.T) {
	raw := `{
	  "machine": {"runtimes": ["container", "native", "wasm"],
	    "max_memory_mb": 768, "host_max_memory_mb": 1024,
	    "container_vm_memory_mb": 1280, "memory_limited_by_vm": true,
	    "max_disk_mb": 102400, "max_cpu_cores": 2, "host_max_cpu_cores": 2},
	  "heads": [{"name": "lbry.science", "grpc_address": "lbry.science:443", "leafs": [
	    {"slug": "bb-native", "name": "Beyblade native", "state": "ACTIVE", "enabled": true,
	     "execution_spec": {"binaries": {"darwin_arm64": "https://example.invalid/bb"}, "max_memory_mb": 900}},
	    {"slug": "bb-container", "name": "Beyblade container", "state": "ACTIVE", "enabled": true,
	     "execution_spec": {"image": "ghcr.io/example/beyblade:1", "max_memory_mb": 900}}
	  ]}]
	}`
	var resp leafsAPIResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	servers := []config.ServerConfig{{Name: "lbry.science", GRPCAddress: "lbry.science:443", TrustedRuntimes: []string{"CONTAINER", "NATIVE"}}}

	var buf bytes.Buffer
	printLeafsTable(&buf, &resp, servers)
	out := buf.String()

	if row := rowFor(out, "bb-native"); !strings.HasSuffix(strings.TrimSpace(row), "yes") {
		t.Errorf("a 900 MB native leaf is not fetchable on a 1024 MB limit; row: %s\n%s", row, out)
	}
	if row := rowFor(out, "bb-container"); !strings.HasSuffix(strings.TrimSpace(row), "no") {
		t.Errorf("a 900 MB container leaf is fetchable on a 768 MB container budget; row: %s\n%s", row, out)
	}
	for _, want := range []string{"900 MB", "768 MB", "1280 MB", "podman machine set --memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("leafs list lacks %q:\n%s", want, out)
		}
	}
}
