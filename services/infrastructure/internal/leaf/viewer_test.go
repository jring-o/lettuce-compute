package leaf

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestViewerContextRoundTrip(t *testing.T) {
	// No viewer injected -> not present.
	if _, ok := ViewerFromContext(context.Background()); ok {
		t.Fatal("expected no viewer in a bare context")
	}

	id := types.NewID()
	want := Viewer{UserID: id, IsAdmin: true, Authed: true}
	ctx := WithViewer(context.Background(), want)
	got, ok := ViewerFromContext(ctx)
	if !ok {
		t.Fatal("expected viewer present after WithViewer")
	}
	if got != want {
		t.Fatalf("viewer round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestCanViewLeaf(t *testing.T) {
	owner := types.NewID()
	other := types.NewID()

	anon := func() (Viewer, bool) { return Viewer{}, false }
	authed := func(id types.ID, admin bool) (Viewer, bool) {
		return Viewer{UserID: id, IsAdmin: admin, Authed: true}, true
	}

	tests := []struct {
		name       string
		visibility LeafVisibility
		creator    *types.ID
		viewer     func() (Viewer, bool)
		want       bool
	}{
		// PUBLIC is readable by anyone.
		{"public/anon", VisibilityPublic, &owner, anon, true},
		{"public/owner", VisibilityPublic, &owner, func() (Viewer, bool) { return authed(owner, false) }, true},
		{"public/other", VisibilityPublic, &owner, func() (Viewer, bool) { return authed(other, false) }, true},

		// UNLISTED is link-accessible by anyone.
		{"unlisted/anon", VisibilityUnlisted, &owner, anon, true},
		{"unlisted/other", VisibilityUnlisted, &owner, func() (Viewer, bool) { return authed(other, false) }, true},

		// PRIVATE is hidden from anonymous and foreign callers.
		{"private/anon", VisibilityPrivate, &owner, anon, false},
		{"private/other", VisibilityPrivate, &owner, func() (Viewer, bool) { return authed(other, false) }, false},
		// PRIVATE visible to owner and admin.
		{"private/owner", VisibilityPrivate, &owner, func() (Viewer, bool) { return authed(owner, false) }, true},
		{"private/admin", VisibilityPrivate, &owner, func() (Viewer, bool) { return authed(other, true) }, true},
		// PRIVATE with nil creator is only visible to admin.
		{"private/nil-creator/owner-like", VisibilityPrivate, nil, func() (Viewer, bool) { return authed(owner, false) }, false},
		{"private/nil-creator/admin", VisibilityPrivate, nil, func() (Viewer, bool) { return authed(owner, true) }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := tc.viewer()
			if got := canViewLeaf(tc.visibility, tc.creator, v, ok); got != tc.want {
				t.Errorf("canViewLeaf(%s) = %v, want %v", tc.visibility, got, tc.want)
			}
		})
	}
}
