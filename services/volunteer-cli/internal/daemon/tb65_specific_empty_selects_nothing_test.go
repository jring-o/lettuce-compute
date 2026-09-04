package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Invariant pins for TB-65. The config validator used to refuse a per-head
// SPECIFIC preference with no enabled leaf, on the theory that an empty list
// might one day be read as "all". These tests are the guard that replaces
// the rule: every reader of the per-head preference selects exactly the
// listed slugs, so an empty list selects nothing — the head stays attached
// and is asked for no work. A future reader that treats "no slugs" as "no
// filter" trips here.

func tb65SpecificEmptyDaemon() *Daemon {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:    "SPECIFIC",
				Enabled: []string{},
				// A weight override left over from when the leaf was enabled
				// must not resurrect it.
				Weights: map[string]int{"leaf-x": 300},
			},
		},
	})
	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name: "srv-a",
		Leafs: []CachedLeafInfo{
			{ID: "id-x", Slug: "leaf-x"},
			{ID: "id-y", Slug: "leaf-y"},
		},
		DefaultWeights: map[string]int{"leaf-x": 150, "leaf-y": 200},
	}
	d.leafCache.mu.Unlock()
	return d
}

// TestTB65_EnabledLeafsSpecificEmptySelectsNothing: the fetcher's per-head
// leaf list is empty, so no request is made for this head.
func TestTB65_EnabledLeafsSpecificEmptySelectsNothing(t *testing.T) {
	d := tb65SpecificEmptyDaemon()
	if got := d.enabledLeafs("srv-a"); len(got) != 0 {
		t.Fatalf("enabledLeafs = %v, want none (SPECIFIC with no enabled leaf must never read as ALL)", got)
	}
	if got := d.allEnabledLeafs(); len(got) != 0 {
		t.Fatalf("allEnabledLeafs = %v, want none", got)
	}
}

// TestTB65_InitializeWeightsSpecificEmptyHasNoLeafWeights: the weighted
// selector gets no leaf for the head either, override or not.
func TestTB65_InitializeWeightsSpecificEmptyHasNoLeafWeights(t *testing.T) {
	d := tb65SpecificEmptyDaemon()
	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()
	if lw := d.weightedSelector.leafWeights["srv-a"]; len(lw) != 0 {
		t.Fatalf("leaf weights for srv-a = %v, want none", lw)
	}
}
