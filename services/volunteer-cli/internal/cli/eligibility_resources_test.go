package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestEvaluateLeafEligibility_DiskGate is the TB-15 regression test for the disk
// half. The head refuses to dispatch a leaf whose
// resource_requirements.min_disk_mb exceeds the volunteer's advertised
// max_disk_gb*1024 (FindNextAssignable's `min_disk_mb <= $5` predicate, and its
// in-memory twin leafMatchesCapabilities). doctor checked memory, container,
// trust and GPU only, so it reported such a leaf ELIGIBLE and the volunteer sat
// idle with every local diagnostic passing.
func TestEvaluateLeafEligibility_DiskGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "fits", Slug: "fits",
			ExecutionSpec:        &lettucev1.ExecutionSpec{MaxMemoryMb: 4096},
			ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 1024}},
		{Id: "toobig", Slug: "toobig",
			ExecutionSpec:        &lettucev1.ExecutionSpec{MaxMemoryMb: 4096},
			ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15360}},
	}
	// The reported shape: memory raised to 6000, disk left at the 10 GB default.
	caps := volunteerCaps{maxMemoryMB: 6000, containerUsable: true, maxDiskMB: 10 * 1024, maxCPUCores: 8}

	res := evaluateLeafEligibility(leafs, caps, trustingHead, nil)
	if res.total != 2 || res.eligible != 1 {
		t.Fatalf("total=%d eligible=%d, want 2/1 (the 15 GB leaf gated by disk)", res.total, res.eligible)
	}
	if res.diskBlocked != 1 {
		t.Errorf("diskBlocked=%d, want 1", res.diskBlocked)
	}
	// The reason must name both numbers and the setting that changes them: a
	// message naming only the shortfall is the documented way this confuses people.
	var blocked string
	for _, le := range res.leaves {
		if !le.eligible {
			blocked = le.reason
		}
	}
	for _, want := range []string{"15360", "10240", "max_disk_gb 15"} {
		if !strings.Contains(blocked, want) {
			t.Errorf("reason %q missing %q", blocked, want)
		}
	}
}

// TestEvaluateLeafEligibility_CoresGate is the TB-15 regression test for the CPU
// half: `min_cpu_cores <= $3` in the same dispatch predicate. The client default
// is NumCPU/2, so an ordinary machine advertises fewer cores than it has.
func TestEvaluateLeafEligibility_CoresGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "toomany", Slug: "toomany",
			ExecutionSpec:        &lettucev1.ExecutionSpec{MaxMemoryMb: 1024},
			ResourceRequirements: &lettucev1.LeafResourceRequirements{MinCpuCores: 4}},
	}
	caps := volunteerCaps{maxMemoryMB: 16384, containerUsable: true, maxDiskMB: 100 * 1024, maxCPUCores: 2}

	res := evaluateLeafEligibility(leafs, caps, trustingHead, nil)
	if res.eligible != 0 || res.coresBlocked != 1 {
		t.Fatalf("eligible=%d coresBlocked=%d, want 0/1", res.eligible, res.coresBlocked)
	}
	if !strings.Contains(res.leaves[0].reason, "max_cpu_cores 4") {
		t.Errorf("reason %q should name the setting and the value that covers it", res.leaves[0].reason)
	}
}

// TestEvaluateLeafEligibility_UnknownBudgetsDoNotBlock pins the degradation rule
// in both directions. A head too old to send resource_requirements, and a daemon
// too old to report the machine's disk/core budgets, must both read as "unknown"
// and leave the verdict where it was — never as a zero that blocks everything.
// The second case is the live version-skew risk: `leafs list` reads the machine
// budgets from the RUNNING daemon, which may predate an upgraded binary.
func TestEvaluateLeafEligibility_UnknownBudgetsDoNotBlock(t *testing.T) {
	// Old head: no resource_requirements at all on the leaf.
	oldHeadLeafs := []*lettucev1.LeafInfo{
		{Id: "l", Slug: "l", ExecutionSpec: &lettucev1.ExecutionSpec{MaxMemoryMb: 1024}},
	}
	caps := volunteerCaps{maxMemoryMB: 4096, containerUsable: true, maxDiskMB: 10 * 1024, maxCPUCores: 2}
	if res := evaluateLeafEligibility(oldHeadLeafs, caps, trustingHead, nil); res.eligible != 1 {
		t.Errorf("old head: eligible=%d, want 1 (absent requirements are unknown, not blocking)", res.eligible)
	}

	// Old daemon: the leaf declares requirements but the machine budgets are 0.
	hungry := []*lettucev1.LeafInfo{
		{Id: "l", Slug: "l",
			ExecutionSpec:        &lettucev1.ExecutionSpec{MaxMemoryMb: 1024},
			ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15360, MinCpuCores: 8}},
	}
	unknownCaps := volunteerCaps{maxMemoryMB: 4096, containerUsable: true}
	if res := evaluateLeafEligibility(hungry, unknownCaps, trustingHead, nil); res.eligible != 1 {
		t.Errorf("old daemon: eligible=%d, want 1 (an unreported budget must not fabricate a block)", res.eligible)
	}
}

// TestDiskGBToCover checks the pasteable remedy value rounds UP: truncating would
// hand the volunteer a max_disk_gb still short of the requirement by up to 1023 MB,
// which they would set, restart, and still receive nothing.
func TestDiskGBToCover(t *testing.T) {
	for _, tc := range []struct {
		mb   int64
		want int
	}{{1024, 1}, {1025, 2}, {15360, 15}, {15361, 16}, {20480, 20}} {
		if got := diskGBToCover(tc.mb); got != tc.want {
			t.Errorf("diskGBToCover(%d) = %d, want %d", tc.mb, got, tc.want)
		}
	}
}

// TestPrintLeafsTable_DiskBlockedLeafSaysNo is the TB-15 regression test for the
// second surface. `leafs list` grew its WILL FETCH column (TB-4) precisely so a
// volunteer could trust one place; it ran the same blind classifier, so it
// promised a fetch the head would refuse.
func TestPrintLeafsTable_DiskBlockedLeafSaysNo(t *testing.T) {
	resp := &leafsAPIResponse{
		Machine: leafsAPIMachine{Runtimes: []string{"container", "wasm"}, MaxMemoryMB: 6000, MaxDiskMB: 10 * 1024, MaxCPUCores: 8},
		Heads: []leafsAPIHead{{
			Name:             "test-head",
			GRPCAddress:      "head:9090",
			LeafsRefreshedAt: time.Now(),
			Leafs: []leafsAPILeaf{{
				Slug: "disk-hungry", Name: "Disk Hungry", State: "ACTIVE", Enabled: true,
				ExecutionSpec:        &leafsAPIExecutionSpec{Image: "example/x:1", MaxMemoryMB: 4096},
				ResourceRequirements: &leafsAPIResourceRequirements{MinDiskMB: 15360},
			}},
		}},
	}
	servers := []config.ServerConfig{{Name: "test-head", GRPCAddress: "head:9090", TrustedRuntimes: []string{"CONTAINER"}}}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()

	if !strings.Contains(out, "15360") || !strings.Contains(out, "max_disk_gb 15") {
		t.Errorf("table should print the disk shortfall and its remedy beneath the rows, got:\n%s", out)
	}
	// The row's verdict must be "no". The header contains "WILL FETCH", so look at
	// the leaf's own line rather than the whole buffer.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "disk-hungry") && strings.HasSuffix(strings.TrimSpace(line), "yes") {
			t.Errorf("WILL FETCH says yes for a leaf the head refuses on disk: %q", line)
		}
	}
}

// TestDescribeSnapshotAge is the TB-14 regression test. The three
// head-derived columns come from a cache refreshed ONLY inside the fetch path,
// so their age is unbounded — a tester spent over an hour comparing three
// machines because nothing on screen said the numbers were of different
// vintages, and reasoned his way to two wrong explanations first.
func TestDescribeSnapshotAge(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 50, 0, 0, time.UTC)

	fresh := describeSnapshotAge("lbry.science", now.Add(-90*time.Second), now)
	if !strings.Contains(fresh, "14:48:30") || !strings.Contains(fresh, "1m ago") {
		t.Errorf("fresh snapshot should carry a clock time and an age, got %q", fresh)
	}
	if strings.Contains(fresh, "frozen") {
		t.Errorf("a 90-second-old snapshot is not stale, got %q", fresh)
	}

	// Past the threshold the line must also say WHY it is frozen: that cause is
	// the answer to the question the reader is usually about to ask.
	stale := describeSnapshotAge("lbry.science", now.Add(-70*time.Minute), now)
	if !strings.Contains(stale, "1h10m ago") {
		t.Errorf("stale age should render as hours and minutes, got %q", stale)
	}
	if !strings.Contains(stale, "not asked that head for work") {
		t.Errorf("stale snapshot should explain the cause, got %q", stale)
	}

	// Never cached: say so rather than implying the figures are current.
	never := describeSnapshotAge("lbry.science", time.Time{}, now)
	if !strings.Contains(never, "never refreshed") {
		t.Errorf("zero timestamp should read as never refreshed, got %q", never)
	}
}

// TestPrintLeafsTable_ShowsSnapshotAge is TB-14 at the surface the tester
// actually read.
func TestPrintLeafsTable_ShowsSnapshotAge(t *testing.T) {
	resp := &leafsAPIResponse{
		Machine: leafsAPIMachine{Runtimes: []string{"native"}, MaxMemoryMB: 4096, MaxDiskMB: 50 * 1024, MaxCPUCores: 8},
		Heads: []leafsAPIHead{{
			Name:             "lbry.science",
			GRPCAddress:      "lbry.science:9090",
			LeafsRefreshedAt: time.Now().Add(-42 * time.Minute),
			Leafs: []leafsAPILeaf{{
				Slug: "beyblade-arena-native", Name: "Beyblade", State: "ACTIVE", Enabled: true,
				QueuedWorkUnits: 294, ActiveVolunteers: 4, ActiveHosts: 4,
				ExecutionSpec: &leafsAPIExecutionSpec{Binaries: map[string]string{"linux_amd64": "u"}, MaxMemoryMB: 128},
			}},
		}},
	}
	servers := []config.ServerConfig{{Name: "lbry.science", GRPCAddress: "lbry.science:9090", TrustedRuntimes: []string{"NATIVE"}}}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()

	if !strings.Contains(out, "come from the head, as of") {
		t.Errorf("table must date its head-derived figures, got:\n%s", out)
	}
	if !strings.Contains(out, "42m ago") {
		t.Errorf("expected the snapshot age in the output, got:\n%s", out)
	}
}
