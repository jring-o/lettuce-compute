package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// PB-16 regression coverage (fetch half): a leaf pinned on a head must be in
// the fetcher's poll set even though the head's public catalog (GetHeadInfo —
// PUBLIC ACTIVE leafs only, by design) does not list it. Pre-fix the pin never
// reached the fetch loop at all, so an UNLISTED leaf's units were permanently
// unreachable for CLI volunteers.
func TestEnabledLeafs_IncludesPinnedUnlistedLeaf(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	srvName := d.multiClient.Servers()[0].Name

	// Wire the daemon's config to carry a pin for this head.
	d.cfg.Servers = []config.ServerConfig{{
		GRPCAddress:   d.multiClient.Servers()[0].Config.GRPCAddress,
		Name:          srvName,
		PinnedLeafIDs: []string{"unlisted-leaf-id"},
	}}

	// The head's catalog lists only a public leaf.
	d.leafCache.PopulateForTest(srvName, &CachedHeadInfo{
		Name:  srvName,
		Leafs: []CachedLeafInfo{{ID: "public-leaf-id", Slug: "public", State: "ACTIVE"}},
	})

	leafs := d.enabledLeafs(srvName)
	var havePublic, havePinned bool
	for _, l := range leafs {
		if l.ID == "public-leaf-id" {
			havePublic = true
		}
		if l.ID == "unlisted-leaf-id" {
			havePinned = true
		}
	}
	if !havePublic {
		t.Error("public catalog leaf missing from the poll set")
	}
	if !havePinned {
		t.Errorf("pinned unlisted leaf missing from the poll set (PB-16); got %+v", leafs)
	}
}

// TestEnabledLeafs_PinBypassesPreferenceFilters: an explicit pin wins over the
// slug-based leaf_preferences filters (an unlisted leaf has no slug the
// operator could reference, and the attach is the stronger signal).
func TestEnabledLeafs_PinBypassesPreferenceFilters(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	srvName := d.multiClient.Servers()[0].Name

	d.cfg.Servers = []config.ServerConfig{{
		GRPCAddress:   d.multiClient.Servers()[0].Config.GRPCAddress,
		Name:          srvName,
		PinnedLeafIDs: []string{"unlisted-leaf-id"},
		LeafPreferences: config.LeafPreferences{
			Mode:    "SPECIFIC",
			Enabled: []string{"public"}, // by slug; the pin has none
		},
	}}
	d.leafCache.PopulateForTest(srvName, &CachedHeadInfo{
		Name:  srvName,
		Leafs: []CachedLeafInfo{{ID: "public-leaf-id", Slug: "public", State: "ACTIVE"}},
	})

	leafs := d.enabledLeafs(srvName)
	var havePinned bool
	for _, l := range leafs {
		if l.ID == "unlisted-leaf-id" {
			havePinned = true
		}
	}
	if !havePinned {
		t.Errorf("pin filtered out by slug-based preferences; got %+v", leafs)
	}
}

// TestEnabledLeafs_PinnedWithoutCatalog: a head with no cached catalog at all
// (GetHeadInfo unavailable) still polls its pinned leafs by id instead of
// falling back to an any-leaf request that can never name them.
func TestEnabledLeafs_PinnedWithoutCatalog(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	srvName := d.multiClient.Servers()[0].Name

	d.cfg.Servers = []config.ServerConfig{{
		GRPCAddress:   d.multiClient.Servers()[0].Config.GRPCAddress,
		Name:          srvName,
		PinnedLeafIDs: []string{"unlisted-leaf-id"},
	}}

	leafs := d.enabledLeafs(srvName)
	if len(leafs) != 1 || leafs[0].ID != "unlisted-leaf-id" {
		t.Errorf("enabledLeafs = %+v, want just the pin", leafs)
	}
}
