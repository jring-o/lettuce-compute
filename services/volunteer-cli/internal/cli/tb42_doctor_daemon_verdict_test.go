package cli

import (
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// Regression tests for TB-42: doctor's per-leaf disk budget hard-coded
// imageCached=false, demanding the full min_disk_mb where the daemon charges
// min(need, 10240) for a cached image — so doctor kept failing a host the
// daemon was happily fetching on, ~5–10 GB high, with no wording admitting the
// assumption. The fix quotes the running daemon's own verdict when it can be
// had, and labels the fresh-download assumption when it cannot.

// The tester's Arch/podman host, 2026-08-05: allowance 27,648 MB, usage
// 16,527 MB, GREP leaf declaring 15,000 MB with its image cached. The daemon
// charges 10,240 incremental (16,527 + 10,240 ≤ 27,648: fetches); doctor
// recomputed with the full 15,000 and reported the leaf disk-blocked forever.
var tb42Req = leafRequirements{name: "extract2-student-crowd-v1", needsContainer: true, diskMB: 15000}
var tb42Caps = volunteerCaps{
	maxMemoryMB:   16384,
	maxDiskMB:     27648,
	freeDataDirMB: 200 * 1024,
	lettuceUsedMB: 16527,
	usedMBKnown:   true,
}

// TestTB42_FallbackBudgetNoteAdmitsTheFreshDownloadAssumption: when the daemon
// cannot be asked, the local recomputation stands — but its budget refusal
// must say it assumes a fresh download, as the free-space branch always did.
// The unlabelled message is what made the tester ask which surface was lying.
func TestTB42_FallbackBudgetNoteAdmitsTheFreshDownloadAssumption(t *testing.T) {
	note := localDiskFetchNote(tb42Req, tb42Caps)
	if note == "" {
		t.Fatal("the conservative fresh-download reading blocks here (16,527 + 15,000 > 27,648) — expected a note")
	}
	if !strings.Contains(note, "assuming a fresh image download") {
		t.Errorf("the fallback budget refusal must admit its fresh-download assumption (TB-42); got: %s", note)
	}
	if !strings.Contains(note, "start it and re-run doctor") {
		t.Errorf("the fallback must point at the enforced verdict (the running daemon); got: %s", note)
	}
}

// TestTB42_DoctorQuotesTheDaemonsPerLeafVerdict: with the running daemon's
// verdict in hand, doctor must print IT — no note when the daemon fetches,
// the daemon's own reason when it gates — never its own recomputation with a
// guessed cachedness.
func TestTB42_DoctorQuotesTheDaemonsPerLeafVerdict(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{{
		Id: "l1", Slug: "extract2-student-crowd-v1", State: "ACTIVE",
		ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "ghcr.io/example/grep:1", MaxMemoryMb: 6000},
		ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15000, MinCpuCores: 1},
	}}
	caps := tb42Caps
	caps.containerUsable = true

	// The daemon fetches this leaf (cached image): the note must vanish, even
	// though the local fresh-download reading would block.
	gates := map[string]*leafsAPIDiskGate{
		"extract2-student-crowd-v1": {Blocked: false, RaiseToGB: 27},
	}
	res := evaluateLeafEligibility(leafs, caps, trustingHead, gates)
	if res.eligible != 1 {
		t.Fatalf("eligible = %d, want 1", res.eligible)
	}
	if note := res.leaves[0].fetchNote; note != "" {
		t.Errorf("the daemon fetches this leaf; doctor must not contradict it with a recomputed verdict (TB-42); got: %s", note)
	}

	// The daemon gates it: doctor prints the daemon's reason, attributed.
	gates["extract2-student-crowd-v1"] = &leafsAPIDiskGate{
		Blocked: true,
		Reason:  "disk budget: Lettuce already uses 16527 MB (work folders + cached images) and this leaf needs 10240 MB more, exceeding the 20480 MB max_disk_gb allowance — free space (superseded images are reclaimed automatically), disable an unused leaf, or raise resource_limits.max_disk_gb to 27",
	}
	res = evaluateLeafEligibility(leafs, caps, trustingHead, gates)
	note := res.leaves[0].fetchNote
	if !strings.Contains(note, "running daemon") || !strings.Contains(note, "16527") {
		t.Errorf("a gated leaf's note must quote the daemon's own verdict; got: %s", note)
	}

	// No verdict for this leaf (daemon down, or a head the daemon has not
	// cached): the labelled local fallback applies.
	res = evaluateLeafEligibility(leafs, caps, trustingHead, nil)
	if note := res.leaves[0].fetchNote; !strings.Contains(note, "assuming a fresh image download") {
		t.Errorf("without a daemon verdict the labelled fallback applies; got: %s", note)
	}
}
