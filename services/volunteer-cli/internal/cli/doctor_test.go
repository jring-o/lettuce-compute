package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

func TestEvaluateLeafEligibility_RuntimeGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "native", ExecutionSpec: &lettucev1.ExecutionSpec{Binaries: map[string]string{"linux-amd64": "url"}}},
		{Id: "container", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/x/y:1"}},
		{Id: "nospec"}, // no execution spec → runs on native/wasm
	}

	// Without a usable container runtime, the image leaf is blocked (ample memory).
	res := evaluateLeafEligibility(leafs, volunteerCaps{maxMemoryMB: 16384, containerUsable: false})
	if res.total != 3 || res.eligible != 2 || res.containerBlocked != 1 {
		t.Errorf("no container: total=%d eligible=%d containerBlocked=%d, want 3/2/1", res.total, res.eligible, res.containerBlocked)
	}

	// With a usable container runtime, everything is runnable.
	res = evaluateLeafEligibility(leafs, volunteerCaps{maxMemoryMB: 16384, containerUsable: true})
	if res.total != 3 || res.eligible != 3 || res.containerBlocked != 0 {
		t.Errorf("with container: total=%d eligible=%d containerBlocked=%d, want 3/3/0", res.total, res.eligible, res.containerBlocked)
	}
}

// TestCountEligibleLeafs_MemoryGate reproduces the #30 memory-gate blind spot:
// doctor counted a leaf eligible purely on runtime (container vs native), ignoring
// the execution_config.max_memory_mb requirement — which is the gate that actually
// fires for a default-configured volunteer (2048 MB) against a standard leaf
// (4096 MB). A leaf whose per-unit memory exceeds the volunteer's limit must be
// reported ineligible, with memory named as the blocker.
func TestCountEligibleLeafs_MemoryGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "fits", Slug: "fits", ExecutionSpec: &lettucev1.ExecutionSpec{MaxMemoryMb: 2048}},
		{Id: "toobig", Slug: "toobig", ExecutionSpec: &lettucev1.ExecutionSpec{MaxMemoryMb: 4096}},
	}

	// Volunteer limit 2048 MB, has a usable container runtime, no GPU.
	caps := volunteerCaps{maxMemoryMB: 2048, containerUsable: true, hasGPU: false}
	res := evaluateLeafEligibility(leafs, caps)

	if res.total != 2 || res.eligible != 1 {
		t.Fatalf("total=%d eligible=%d, want 2/1 (the 4096 MB leaf gated by memory)", res.total, res.eligible)
	}
	// The blocked leaf must be attributed to memory so doctor can print the remedy.
	if res.memoryBlocked != 1 {
		t.Errorf("memoryBlocked=%d, want 1", res.memoryBlocked)
	}
}

func TestCheckIdentity_MissingKeypairFails(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}

	checkIdentity(rep, filepath.Join(dir, "id.key"), filepath.Join(dir, "id.pub"))

	if rep.fails != 1 {
		t.Errorf("fails = %d, want 1 for a missing keypair", rep.fails)
	}
	if !strings.Contains(buf.String(), "no keypair") {
		t.Errorf("expected a 'no keypair' message; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "lettuce-volunteer init") {
		t.Errorf("expected the init remedy; got: %s", buf.String())
	}
}

// TestCheckDisk_MatchesFetchGate pins doctor's disk verdict to the daemon's live
// shouldFetch gate (TODO #24): a host the gate would still fetch on must not be
// reported as a blocking failure, and a host the gate would actually block must
// fail with the gate's own numbers.
func TestCheckDisk_MatchesFetchGate(t *testing.T) {
	const dataDir = "/data"
	cases := []struct {
		name        string
		availableMB int64
		maxDiskGB   int
		wantFails   int
		wantWarns   int
	}{
		{
			// Ample: above the full allowance — gate always fetches.
			name: "ample", availableMB: 25 * 1024, maxDiskGB: 20, wantFails: 0, wantWarns: 0,
		},
		{
			// The reported false positive: max_disk_gb (20 GB) exceeds the 10 GB
			// cached-image headroom, and free space (15 GB) sits between them. The
			// gate still fetches work for any already-cached image, so doctor must
			// NOT report a blocking failure — at most a warning.
			name: "cached_only_region", availableMB: 15 * 1024, maxDiskGB: 20, wantFails: 0, wantWarns: 1,
		},
		{
			// Below even the cached-image headroom — the gate blocks all work, so
			// doctor correctly fails.
			name: "blocked", availableMB: 5 * 1024, maxDiskGB: 20, wantFails: 1, wantWarns: 0,
		},
		{
			// With max_disk_gb <= the headroom there is no cached-only band: the
			// gate is a single threshold, so a reading below it is a hard failure.
			name: "small_allowance_blocked", availableMB: 5 * 1024, maxDiskGB: 10, wantFails: 1, wantWarns: 0,
		},
		{
			name: "small_allowance_ample", availableMB: 12 * 1024, maxDiskGB: 10, wantFails: 0, wantWarns: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := &doctorReport{w: &buf}
			checkDisk(rep, dataDir, tc.availableMB, tc.maxDiskGB)
			if rep.fails != tc.wantFails || rep.warns != tc.wantWarns {
				t.Errorf("checkDisk(avail=%d, maxGB=%d): fails=%d warns=%d, want fails=%d warns=%d\nreport:\n%s",
					tc.availableMB, tc.maxDiskGB, rep.fails, rep.warns, tc.wantFails, tc.wantWarns, buf.String())
			}
		})
	}
}

func TestDoctorReport_CountsLevels(t *testing.T) {
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	rep.add(docOK, "a", "fine", "")
	rep.add(docInfo, "b", "noted", "")
	rep.add(docWarn, "c", "hmm", "do x")
	rep.add(docFail, "d", "broken", "fix y")

	if rep.fails != 1 || rep.warns != 1 {
		t.Errorf("fails=%d warns=%d, want 1/1", rep.fails, rep.warns)
	}
	if !strings.Contains(buf.String(), "-> do x") || !strings.Contains(buf.String(), "-> fix y") {
		t.Errorf("remedies should be printed; got: %s", buf.String())
	}
}
