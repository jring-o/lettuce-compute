package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

func TestCountEligibleLeafs(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "native", ExecutionSpec: &lettucev1.ExecutionSpec{Binaries: map[string]string{"linux-amd64": "url"}}},
		{Id: "container", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/x/y:1"}},
		{Id: "nospec"}, // no execution spec → runs on native/wasm
	}

	// Without a usable container runtime, the image leaf is blocked.
	total, eligible, blocked := countEligibleLeafs(leafs, false)
	if total != 3 || eligible != 2 || blocked != 1 {
		t.Errorf("no container: total=%d eligible=%d blocked=%d, want 3/2/1", total, eligible, blocked)
	}

	// With a usable container runtime, everything is runnable.
	total, eligible, blocked = countEligibleLeafs(leafs, true)
	if total != 3 || eligible != 3 || blocked != 0 {
		t.Errorf("with container: total=%d eligible=%d blocked=%d, want 3/3/0", total, eligible, blocked)
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
