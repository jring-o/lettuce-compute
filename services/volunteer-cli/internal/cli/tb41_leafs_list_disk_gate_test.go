package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Regression tests for TB-41: `leafs list` said WILL FETCH: yes for a leaf the
// same binary's daemon was disk-gating (its verdict consulted only the
// classifier — requirement vs allowance, usage nowhere), and when it did say
// no, its remedy named an allowance computed from the leaf requirement alone,
// which a machine with usage can never satisfy — the tester's observed
// 20 → 27 → 43 → 53 GB chase.
//
// Both tests feed the management-API response as JSON, exactly as the running
// daemon serves it, so this file compiles against the pre-fix code (which
// simply ignored the disk_gate field) and demonstrates the red half.

// tb41Response decodes a leafs-list API response from JSON.
func tb41Response(t *testing.T, body string) *leafsAPIResponse {
	t.Helper()
	var resp leafsAPIResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &resp
}

// TestTB41_WillFetchQuotesTheDaemonsDiskGate is the tester's 20 GB state,
// 2026-08-05: allowance 20,480 MB, GREP leaf declaring 15,000 MB — the
// classifier calls that eligible (15,000 ≤ 20,480), while the daemon's live
// gate refuses it (16,527 MB used + 10,240 MB incremental > 20,480). The
// column must answer with the gate's verdict, not contradict it.
func TestTB41_WillFetchQuotesTheDaemonsDiskGate(t *testing.T) {
	resp := tb41Response(t, `{
		"machine": {"runtimes": ["container"], "max_memory_mb": 16384, "max_disk_mb": 20480, "max_cpu_cores": 8},
		"heads": [{
			"name": "test-head", "grpc_address": "test-head:9090",
			"leafs": [{
				"slug": "grep-cpu", "name": "GREP CPU", "state": "ACTIVE", "enabled": true,
				"execution_spec": {"image": "ghcr.io/example/grep:1", "max_memory_mb": 6000},
				"resource_requirements": {"min_disk_mb": 15000, "min_cpu_cores": 1},
				"disk_gate": {
					"blocked": true,
					"reason": "disk budget: Lettuce already uses 16527 MB (work folders + cached images) and this leaf needs 10240 MB more, exceeding the 20480 MB max_disk_gb allowance — free space (superseded images are reclaimed automatically), disable an unused leaf, or raise resource_limits.max_disk_gb to 27",
					"raise_to_gb": 27
				}
			}]
		}]
	}`)

	var out strings.Builder
	printLeafsTable(&out, resp, []config.ServerConfig{trustingHead})
	got := out.String()

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "grep-cpu") && strings.HasSuffix(strings.TrimSpace(line), "yes") {
			t.Errorf("WILL FETCH says yes for a leaf the daemon's own fetcher is skipping (TB-41):\n%s", line)
		}
	}
	if !strings.Contains(got, "16527") {
		t.Errorf("the note must quote the daemon's own gate reason (its numbers included); got:\n%s", got)
	}
}

// TestTB41_DiskRemedyUsesTheDaemonsCoveringAllowance: when the classifier does
// block on disk, the paste-me number must come from the daemon's arithmetic —
// max(need, usage + incremental need) — not from the leaf requirement alone.
// The tester's GPU-less docker host shape reduced to one leaf: requirement
// 20,000 MB, allowance 15,360 MB, and the daemon (usage 16,527 MB, image
// cached) says 27 GB covers it. The requirement-only number, 20, is the first
// step of the chase.
func TestTB41_DiskRemedyUsesTheDaemonsCoveringAllowance(t *testing.T) {
	resp := tb41Response(t, `{
		"machine": {"runtimes": ["container"], "max_memory_mb": 16384, "max_disk_mb": 15360, "max_cpu_cores": 8},
		"heads": [{
			"name": "test-head", "grpc_address": "test-head:9090",
			"leafs": [{
				"slug": "big-disk-leaf", "name": "Big Disk", "state": "ACTIVE", "enabled": true,
				"execution_spec": {"image": "ghcr.io/example/big:1", "max_memory_mb": 6000},
				"resource_requirements": {"min_disk_mb": 20000, "min_cpu_cores": 1},
				"disk_gate": {
					"blocked": true,
					"reason": "disk budget: Lettuce already uses 16527 MB (work folders + cached images) and this leaf needs 10240 MB more, exceeding the 15360 MB max_disk_gb allowance — free space (superseded images are reclaimed automatically), disable an unused leaf, or raise resource_limits.max_disk_gb to 27",
					"raise_to_gb": 27
				}
			}]
		}]
	}`)

	var out strings.Builder
	printLeafsTable(&out, resp, []config.ServerConfig{trustingHead})
	got := out.String()

	if !strings.Contains(got, "max_disk_gb 27") {
		t.Errorf("the disk remedy must name the daemon's covering allowance (27), not the requirement-only 20 the tester chased (TB-41); got:\n%s", got)
	}
}

// TestTB41_NoDaemonGateKeepsTheOldBehavior: a daemon predating the disk_gate
// field sends none, and the verdict must degrade to exactly the pre-fix
// answer rather than inventing a gate.
func TestTB41_NoDaemonGateKeepsTheOldBehavior(t *testing.T) {
	resp := tb41Response(t, `{
		"machine": {"runtimes": ["container"], "max_memory_mb": 16384, "max_disk_mb": 20480, "max_cpu_cores": 8},
		"heads": [{
			"name": "test-head", "grpc_address": "test-head:9090",
			"leafs": [{
				"slug": "grep-cpu", "name": "GREP CPU", "state": "ACTIVE", "enabled": true,
				"execution_spec": {"image": "ghcr.io/example/grep:1", "max_memory_mb": 6000},
				"resource_requirements": {"min_disk_mb": 15000, "min_cpu_cores": 1}
			}]
		}]
	}`)

	var out strings.Builder
	printLeafsTable(&out, resp, []config.ServerConfig{trustingHead})
	got := out.String()

	found := false
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "grep-cpu") && strings.HasSuffix(strings.TrimSpace(line), "yes") {
			found = true
		}
	}
	if !found {
		t.Errorf("without a daemon verdict the classifier's answer stands (eligible → yes); got:\n%s", got)
	}
}
