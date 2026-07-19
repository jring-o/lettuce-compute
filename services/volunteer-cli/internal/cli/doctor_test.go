package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
)

// trustingHead is a head config trusted for every runtime, so tests that gate
// on other dimensions (runtime presence, memory, GPU) aren't confounded by the
// per-head trust gate.
var trustingHead = config.ServerConfig{Name: "test-head", TrustedRuntimes: []string{"CONTAINER", "NATIVE"}}

func TestEvaluateLeafEligibility_RuntimeGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "native", ExecutionSpec: &lettucev1.ExecutionSpec{Binaries: map[string]string{"linux-amd64": "url"}}},
		{Id: "container", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/x/y:1"}},
		{Id: "nospec"}, // no execution spec → runs on native/wasm
	}

	// Without a usable container runtime, the image leaf is blocked (ample memory).
	res := evaluateLeafEligibility(leafs, volunteerCaps{maxMemoryMB: 16384, containerUsable: false}, trustingHead)
	if res.total != 3 || res.eligible != 2 || res.containerBlocked != 1 {
		t.Errorf("no container: total=%d eligible=%d containerBlocked=%d, want 3/2/1", res.total, res.eligible, res.containerBlocked)
	}

	// With a usable container runtime, everything is runnable.
	res = evaluateLeafEligibility(leafs, volunteerCaps{maxMemoryMB: 16384, containerUsable: true}, trustingHead)
	if res.total != 3 || res.eligible != 3 || res.containerBlocked != 0 {
		t.Errorf("with container: total=%d eligible=%d containerBlocked=%d, want 3/3/0", res.total, res.eligible, res.containerBlocked)
	}
}

// TestEvaluateLeafEligibility_TrustGate is the PB-5 regression test: a leaf the
// per-head runtime trust would refuse must not be counted eligible. A volunteer
// whose trust in a head is WASM-only (trust "none" at attach) can never receive
// that head's NATIVE or CONTAINER work — doctor said "eligible for 1 of 1
// leafs" anyway, and the volunteer idled with no hint.
func TestEvaluateLeafEligibility_TrustGate(t *testing.T) {
	nativeOnly := []*lettucev1.LeafInfo{
		{Id: "native", Slug: "native-leaf", ExecutionSpec: &lettucev1.ExecutionSpec{Binaries: map[string]string{"linux_amd64": "url"}}},
	}
	containerLeaf := []*lettucev1.LeafInfo{
		{Id: "container", Slug: "container-leaf", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/x/y:1"}},
	}
	wasmLeaf := []*lettucev1.LeafInfo{
		{Id: "wasm", Slug: "wasm-leaf", ExecutionSpec: &lettucev1.ExecutionSpec{Binaries: map[string]string{"wasm": "url"}}},
	}
	caps := volunteerCaps{maxMemoryMB: 16384, containerUsable: true}
	untrusting := config.ServerConfig{Name: "wasm-only-head"} // no TrustedRuntimes → WASM only

	if res := evaluateLeafEligibility(nativeOnly, caps, untrusting); res.eligible != 0 || res.trustBlocked != 1 {
		t.Errorf("native leaf, untrusted head: eligible=%d trustBlocked=%d, want 0/1", res.eligible, res.trustBlocked)
	}
	if res := evaluateLeafEligibility(containerLeaf, caps, untrusting); res.eligible != 0 || res.trustBlocked != 1 {
		t.Errorf("container leaf, untrusted head: eligible=%d trustBlocked=%d, want 0/1", res.eligible, res.trustBlocked)
	}
	// WASM is always trusted — stays eligible on an untrusting head.
	if res := evaluateLeafEligibility(wasmLeaf, caps, untrusting); res.eligible != 1 || res.trustBlocked != 0 {
		t.Errorf("wasm leaf, untrusted head: eligible=%d trustBlocked=%d, want 1/0", res.eligible, res.trustBlocked)
	}
	// And trusting the head restores eligibility.
	if res := evaluateLeafEligibility(nativeOnly, caps, trustingHead); res.eligible != 1 || res.trustBlocked != 0 {
		t.Errorf("native leaf, trusted head: eligible=%d trustBlocked=%d, want 1/0", res.eligible, res.trustBlocked)
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
	res := evaluateLeafEligibility(leafs, caps, trustingHead)

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

// TestCheckIdentity_PresentButUnreadable_DoesNotAdviseInit reproduces TODO #25:
// when the keypair is PRESENT but won't load — the shape a data-dir relocation to
// another user produces (a partial/corrupt copy, or a private key the running user
// can't read) — doctor reported "keypair present but unreadable" with the remedy
// "re-run: lettuce-volunteer init". That advice is actively harmful: `init` mints a
// NEW identity and abandons the account's accrued credit. The remedy must instead
// be an actionable relocation fix (re-copy / fix ownership) and must NOT tell the
// user to run init.
func TestCheckIdentity_PresentButUnreadable_DoesNotAdviseInit(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "identity.key")
	pubFile := filepath.Join(dir, "identity.pub")

	// Both files exist (KeyPairExists is true) but the private key is the wrong
	// size — the same "present but won't load" shape a partial copy produces.
	if err := os.WriteFile(keyFile, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubFile, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkIdentity(rep, keyFile, pubFile)
	out := buf.String()

	if rep.fails != 1 {
		t.Fatalf("fails = %d, want 1 for an unreadable keypair; report:\n%s", rep.fails, out)
	}
	if strings.Contains(out, "lettuce-volunteer init") {
		t.Errorf("remedy must NOT advise running init (it mints a new identity, abandoning the account); got:\n%s", out)
	}
	if !strings.Contains(out, "re-copy") {
		t.Errorf("remedy should tell the user to re-copy the key files from the original data dir; got:\n%s", out)
	}
}

// TestCheckIdentity_PermissionDenied_GivesOwnershipRemedy reproduces the headline
// TODO #25 case on POSIX: the keypair was carried to another user but the running
// user can't read the private key (wrong owner/mode after chown). doctor must name
// the ownership fix (chown/chmod) and still must not advise init.
func TestCheckIdentity_PermissionDenied_GivesOwnershipRemedy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-permission denial is not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 is still readable, can't simulate denial")
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "identity.key")
	pubFile := filepath.Join(dir, "identity.pub")
	pub, priv, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKeyPair(keyFile, pubFile, priv, pub); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyFile, 0o600) })

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkIdentity(rep, keyFile, pubFile)
	out := buf.String()

	if rep.fails != 1 {
		t.Fatalf("fails = %d, want 1; report:\n%s", rep.fails, out)
	}
	if strings.Contains(out, "lettuce-volunteer init") {
		t.Errorf("remedy must NOT advise init; got:\n%s", out)
	}
	if !strings.Contains(out, "chown") || !strings.Contains(out, "chmod") {
		t.Errorf("remedy should name the ownership fix (chown/chmod); got:\n%s", out)
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

// TestCheckImageStore_FlagsShortVolume covers the TODO #31 doctor surface: the
// image-store filesystem (engine DockerRootDir / Podman graphroot) is reported
// separately from the data dir, and a volume too small to pull a big image is a
// warning (native + cached-image leafs still run), not a hard failure.
func TestCheckImageStore_FlagsShortVolume(t *testing.T) {
	const storePath = "/var/lib/containers/storage"
	cases := []struct {
		name        string
		availableMB int64
		maxDiskGB   int
		wantFails   int
		wantWarns   int
	}{
		// Ample: at/above the full pull allowance → OK.
		{name: "ample", availableMB: 120 * 1024, maxDiskGB: 100, wantFails: 0, wantWarns: 0},
		// Short: data dir may be roomy, but the image store can't hold the pull →
		// warn (not fail).
		{name: "short", availableMB: 20 * 1024, maxDiskGB: 100, wantFails: 0, wantWarns: 1},
		// Probe failed (non-positive reading) → warn, not a confident pass.
		{name: "unreadable", availableMB: 0, maxDiskGB: 100, wantFails: 0, wantWarns: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := &doctorReport{w: &buf}
			checkImageStore(rep, storePath, tc.availableMB, tc.maxDiskGB)
			if rep.fails != tc.wantFails || rep.warns != tc.wantWarns {
				t.Errorf("checkImageStore(avail=%d, maxGB=%d): fails=%d warns=%d, want fails=%d warns=%d\nreport:\n%s",
					tc.availableMB, tc.maxDiskGB, rep.fails, rep.warns, tc.wantFails, tc.wantWarns, buf.String())
			}
			if !strings.Contains(buf.String(), storePath) {
				t.Errorf("report should name the image-store path %q; got:\n%s", storePath, buf.String())
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
