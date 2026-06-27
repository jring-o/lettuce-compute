package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestServerBlockedLeafIDs_Blocklist proves bug 4's fix: a per-server BLOCKLIST
// (by slug) is translated to the disabled leaf's ID so the head can be told not
// to dispatch it — closing the any-leaf fallback hole where the per-server
// blocklist used to be ignored.
func TestServerBlockedLeafIDs_Blocklist(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:     "BLOCKLIST",
				Disabled: []string{"beyblade-arena-native"},
			},
		},
	})
	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name: "srv-a",
		Leafs: []CachedLeafInfo{
			{ID: "id-container", Slug: "beyblade-arena"},
			{ID: "id-native", Slug: "beyblade-arena-native"},
		},
	}
	d.leafCache.mu.Unlock()

	blocked := d.serverBlockedLeafIDs("srv-a")
	if len(blocked) != 1 || blocked[0] != "id-native" {
		t.Fatalf("expected blocked = [id-native], got %v", blocked)
	}
}

// TestServerBlockedLeafIDs_AllModeBlocksNothing verifies ALL mode blocks nothing.
func TestServerBlockedLeafIDs_AllModeBlocksNothing(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{GRPCAddress: "localhost:9090", Name: "srv-a", LeafPreferences: config.LeafPreferences{Mode: "ALL"}},
	})
	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name:  "srv-a",
		Leafs: []CachedLeafInfo{{ID: "id-1", Slug: "a"}, {ID: "id-2", Slug: "b"}},
	}
	d.leafCache.mu.Unlock()

	if blocked := d.serverBlockedLeafIDs("srv-a"); len(blocked) != 0 {
		t.Fatalf("ALL mode should block nothing, got %v", blocked)
	}
}

func TestMergeUnique(t *testing.T) {
	got := mergeUnique([]string{"a", "b", ""}, []string{"b", "c", ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("mergeUnique = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeUnique = %v, want %v", got, want)
		}
	}
}
