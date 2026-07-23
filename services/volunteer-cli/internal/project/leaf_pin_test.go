package project

import (
	"testing"
)

// PB-16 regression coverage (attach half): pinning a leaf on an
// ALREADY-ATTACHED head must MERGE the pin into the existing entry, keeping
// its connection settings — the old behavior appended a whole second entry
// (dropping --insecure/--trust) that the daemon then collapsed at startup,
// silently discarding the pin. This test uses only the pre-existing AttachLeaf
// signature, so it compiles against the pre-fix tree and fails there (two
// entries; flags on the surviving one only by luck of collapse order).
func TestAttachLeaf_MergesPinIntoExistingHeadEntry(t *testing.T) {
	mgr, _ := testManager(t)

	// The head is attached first, with connection settings that must survive.
	if err := mgr.AttachServerWithTLS("localhost", 18081, 18080, true, "", []string{"NATIVE"}); err != nil {
		t.Fatalf("attach server: %v", err)
	}

	// Both attach forms funnel through AttachLeaf with the head's address.
	if err := mgr.AttachLeaf("85baa67f-4b31-4f34-9d1e-30c1a2b3c4d5", "localhost:18081", "http://localhost:18080", "localhost"); err != nil {
		t.Fatalf("attach leaf: %v", err)
	}

	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1 — pinning a leaf must not append a duplicate entry (PB-16)", len(mgr.cfg.Servers))
	}
	srv := mgr.cfg.Servers[0]
	if !srv.Insecure {
		t.Error("the head entry's insecure setting was lost by the leaf attach (PB-16)")
	}
	if !srv.TrustsRuntime("NATIVE") {
		t.Errorf("the head entry's runtime trust was lost by the leaf attach (PB-16); trusted = %v", srv.EffectiveTrustedRuntimes())
	}
}
