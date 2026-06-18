package validation

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func mustVID(t *testing.T, s string) types.ID {
	t.Helper()
	id, err := types.ParseID(s)
	if err != nil {
		t.Fatalf("parse id %q: %v", s, err)
	}
	return id
}

// TestVersionHomogeneousGroup verifies validation never compares results produced by
// different artifact versions (TODO #38, #12): it returns the largest single-version
// group, leaving single-version / legacy inputs unchanged.
func TestVersionHomogeneousGroup(t *testing.T) {
	vA := mustVID(t, "11111111-1111-1111-1111-111111111111")
	vB := mustVID(t, "22222222-2222-2222-2222-222222222222")
	res := func(v *types.ID) *result.Result { return &result.Result{ArtifactVersionID: v} }

	t.Run("single result unchanged", func(t *testing.T) {
		if got := versionHomogeneousGroup([]*result.Result{res(&vA)}); len(got) != 1 {
			t.Fatalf("want 1, got %d", len(got))
		}
	})
	t.Run("all same version unchanged", func(t *testing.T) {
		if got := versionHomogeneousGroup([]*result.Result{res(&vA), res(&vA)}); len(got) != 2 {
			t.Fatalf("want 2, got %d", len(got))
		}
	})
	t.Run("all legacy nil unchanged", func(t *testing.T) {
		if got := versionHomogeneousGroup([]*result.Result{res(nil), res(nil)}); len(got) != 2 {
			t.Fatalf("want 2, got %d", len(got))
		}
	})
	t.Run("mixed versions selects the largest single-version group", func(t *testing.T) {
		got := versionHomogeneousGroup([]*result.Result{res(&vA), res(&vB), res(&vA)})
		if len(got) != 2 {
			t.Fatalf("want 2 (the vA group), got %d", len(got))
		}
		for _, r := range got {
			if r.ArtifactVersionID == nil || *r.ArtifactVersionID != vA {
				t.Fatalf("expected only vA results, got %v", r.ArtifactVersionID)
			}
		}
	})
	t.Run("nil group competes as its own version", func(t *testing.T) {
		// One vA, two nil -> the nil (legacy) group is larger and is chosen.
		got := versionHomogeneousGroup([]*result.Result{res(&vA), res(nil), res(nil)})
		if len(got) != 2 {
			t.Fatalf("want 2 (the nil group), got %d", len(got))
		}
		for _, r := range got {
			if r.ArtifactVersionID != nil {
				t.Fatalf("expected only nil-version results, got %v", r.ArtifactVersionID)
			}
		}
	})
}
