package daemon

import (
	"context"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// PB-5 regression test, daemon half: the readiness line must not count leafs
// the per-head runtime trust would refuse. A volunteer attached with trust
// "none" (WASM-only) to a head whose only leaf is NATIVE logged
// "eligible_leafs: 1" and then sat idle forever with no hint.
func TestReadinessCounts_TrustGate(t *testing.T) {
	mc := &mockClient{}
	d := newTestDaemon(mc, &mockRuntime{canHandle: true})

	// A single NATIVE leaf on the attached head.
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id:    "leaf-1",
				Slug:  "native-leaf",
				State: "ACTIVE",
				ExecutionSpec: &lettucev1.ExecutionSpec{
					Binaries: map[string]string{"linux_amd64": "https://example.com/bin"},
				},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	// The test harness head trusts every runtime: the leaf counts as eligible.
	if total, eligible, _, trustBlocked := d.readinessCounts(); total != 1 || eligible != 1 || trustBlocked != 0 {
		t.Fatalf("trusted head: total=%d eligible=%d trustBlocked=%d, want 1/1/0", total, eligible, trustBlocked)
	}

	// Trust "none" (WASM-only) for the same head: the NATIVE leaf can never be
	// received or run — it must count as trust-blocked, not eligible.
	d.multiClient.Servers()[0].Config.TrustedRuntimes = nil
	if total, eligible, _, trustBlocked := d.readinessCounts(); total != 1 || eligible != 0 || trustBlocked != 1 {
		t.Fatalf("untrusted head: total=%d eligible=%d trustBlocked=%d, want 1/0/1", total, eligible, trustBlocked)
	}
}
