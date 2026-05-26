package leaf

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Viewer describes the caller making a leaf read request. It is injected into
// the request context by the server package (which performs authentication) so
// the leaf handlers can apply visibility checks WITHOUT importing the server
// package (doing so would create an import cycle: server imports leaf).
//
// When no Viewer is present in the context the caller is treated as anonymous:
// only PUBLIC (and link-accessible UNLISTED) leafs are visible.
type Viewer struct {
	// UserID is the authenticated caller's user id. Only meaningful when Authed.
	UserID types.ID
	// IsAdmin is true when the caller has the ADMIN role.
	IsAdmin bool
	// Authed is true when a valid credential resolved to a user.
	Authed bool
}

type viewerContextKey struct{}

// WithViewer returns a copy of ctx carrying the given Viewer.
func WithViewer(ctx context.Context, v Viewer) context.Context {
	return context.WithValue(ctx, viewerContextKey{}, v)
}

// ViewerFromContext extracts the Viewer from ctx. The boolean is false when no
// viewer was injected (anonymous request).
func ViewerFromContext(ctx context.Context) (Viewer, bool) {
	v, ok := ctx.Value(viewerContextKey{}).(Viewer)
	return v, ok
}

// canViewLeaf reports whether the given viewer may read a leaf with the given
// visibility and creator. PUBLIC and UNLISTED leafs are readable by anyone
// (UNLISTED is intentionally link-accessible). PRIVATE leafs are readable only
// by an authenticated admin or the leaf's own creator.
//
// ok is false (no viewer in context) is treated as anonymous.
func canViewLeaf(visibility LeafVisibility, creatorID *types.ID, v Viewer, ok bool) bool {
	if visibility != VisibilityPrivate {
		return true
	}
	if !ok || !v.Authed {
		return false
	}
	if v.IsAdmin {
		return true
	}
	return creatorID != nil && *creatorID == v.UserID
}
