package project

import (
	"testing"
)

// TestAttachLeaf_MultiplePinsOneHead: several unlisted leafs can be pinned on
// one head, and each can be detached individually.
func TestAttachLeaf_MultiplePinsOneHead(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachServerWithTLS("localhost", 18081, 18080, true, "", []string{}); err != nil {
		t.Fatalf("attach server: %v", err)
	}
	if err := mgr.AttachLeaf("leaf-a", "localhost:18081", "http://localhost:18080", "localhost"); err != nil {
		t.Fatalf("pin leaf-a: %v", err)
	}
	if err := mgr.AttachLeaf("leaf-b", "localhost:18081", "http://localhost:18080", "localhost"); err != nil {
		t.Fatalf("pin leaf-b: %v", err)
	}

	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(mgr.cfg.Servers))
	}
	if pins := mgr.cfg.Servers[0].PinnedLeafIDs; len(pins) != 2 {
		t.Fatalf("pins = %v, want [leaf-a leaf-b]", pins)
	}

	if err := mgr.DetachLeaf("leaf-a"); err != nil {
		t.Fatalf("detach leaf-a: %v", err)
	}
	if pins := mgr.cfg.Servers[0].PinnedLeafIDs; len(pins) != 1 || pins[0] != "leaf-b" {
		t.Fatalf("pins after detach = %v, want [leaf-b]", pins)
	}
}
