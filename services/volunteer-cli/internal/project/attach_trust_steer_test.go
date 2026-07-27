package project

import (
	"strings"
	"testing"
)

// TestAttachServerDuplicatePointsAtHeadsTrust is a TB-7 regression. A volunteer
// whose head was attached during `init` was never asked the runtime-trust consent
// and landed silently WASM-only, so re-running `attach --server <head>` to get the
// question is the natural move — and it was refused with a bare "already attached
// to server <addr>" that named no way forward. The refusal is still correct; it now
// says where the trust decision actually lives.
func TestAttachServerDuplicatePointsAtHeadsTrust(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachServerWithTLS("example.com", 443, 443, false, "", []string{}); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	err := mgr.AttachServerWithTLS("example.com", 443, 443, false, "", []string{"CONTAINER"})
	if err == nil {
		t.Fatal("duplicate server attach should still be refused")
	}
	if !strings.Contains(err.Error(), "heads trust") {
		t.Errorf("refusal %q does not name the command that changes what the head may run", err)
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("refusal %q does not name the head", err)
	}
}
