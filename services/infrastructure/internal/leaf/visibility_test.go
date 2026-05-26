//go:build integration

package leaf

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// viewerInjector mirrors the server package's leafViewer wrapper: it injects a
// fixed Viewer into the request context so the read handlers can enforce
// per-leaf visibility. A nil viewer means an anonymous request.
func viewerInjector(v *Viewer, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v != nil {
			r = r.WithContext(WithViewer(r.Context(), *v))
		}
		next(w, r)
	}
}

// setupVisibilityServer wires the read routes through viewerInjector with the
// given viewer so tests can simulate anonymous/owner/admin/other callers.
func setupVisibilityServer(t *testing.T, pool *pgxpool.Pool, v *Viewer) *httptest.Server {
	t.Helper()
	repo := NewPgxRepository(pool)
	handler := NewLeafHandler(repo, pool, testLogger())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}", viewerInjector(v, handler.HandleGet))
	mux.HandleFunc("GET /api/v1/leafs", viewerInjector(v, handler.HandleList))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// insertLeaf inserts a leaf with the given visibility/creator directly via the
// repository and returns it (populated with id + slug).
func insertLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID, name string, vis LeafVisibility) *Leaf {
	t.Helper()
	repo := NewPgxRepository(pool)
	p := newTestLeaf(&creatorID)
	p.Name = name
	p.Visibility = vis
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("insert leaf %q: %v", name, err)
	}
	return p
}

// --- handleGet visibility ---

func TestGetPrivateLeafAnonymous404(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-priv-anon")
	priv := insertLeaf(t, pool, owner, "Private Anon Leaf", VisibilityPrivate)

	ts := setupVisibilityServer(t, pool, nil) // anonymous

	// By UUID.
	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.ID.String(), nil)
	requireStatus(t, resp, http.StatusNotFound, "anon GET private by UUID")
	resp.Body.Close()

	// By slug.
	resp = doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.Slug, nil)
	requireStatus(t, resp, http.StatusNotFound, "anon GET private by slug")
	resp.Body.Close()
}

func TestGetPrivateLeafForeignUser404(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-priv-owner")
	other := createTestUser(t, pool, "vis-priv-other")
	priv := insertLeaf(t, pool, owner, "Private Foreign Leaf", VisibilityPrivate)

	ts := setupVisibilityServer(t, pool, &Viewer{UserID: other, Authed: true})

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.ID.String(), nil)
	requireStatus(t, resp, http.StatusNotFound, "foreign user GET private by UUID")
	resp.Body.Close()

	resp = doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.Slug, nil)
	requireStatus(t, resp, http.StatusNotFound, "foreign user GET private by slug")
	resp.Body.Close()
}

func TestGetPrivateLeafOwner200(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-priv-self")
	priv := insertLeaf(t, pool, owner, "Private Owner Leaf", VisibilityPrivate)

	ts := setupVisibilityServer(t, pool, &Viewer{UserID: owner, Authed: true})

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.ID.String(), nil)
	requireStatus(t, resp, http.StatusOK, "owner GET private by UUID")
	var got Leaf
	decodeJSON(t, resp, &got)
	if got.ID != priv.ID {
		t.Errorf("ID = %v, want %v", got.ID, priv.ID)
	}

	resp = doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.Slug, nil)
	requireStatus(t, resp, http.StatusOK, "owner GET private by slug")
	resp.Body.Close()
}

func TestGetPrivateLeafAdmin200(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-priv-adminowner")
	priv := insertLeaf(t, pool, owner, "Private Admin Leaf", VisibilityPrivate)

	// Admin with a different (here nil) user id still sees PRIVATE leafs.
	ts := setupVisibilityServer(t, pool, &Viewer{UserID: types.NilID(), IsAdmin: true, Authed: true})

	resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+priv.ID.String(), nil)
	requireStatus(t, resp, http.StatusOK, "admin GET private by UUID")
	resp.Body.Close()
}

func TestGetPublicAndUnlistedLeafAnonymous200(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-pubunl")
	pub := insertLeaf(t, pool, owner, "Public Leaf", VisibilityPublic)
	unl := insertLeaf(t, pool, owner, "Unlisted Leaf", VisibilityUnlisted)

	ts := setupVisibilityServer(t, pool, nil) // anonymous

	for _, p := range []*Leaf{pub, unl} {
		resp := doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+p.ID.String(), nil)
		requireStatus(t, resp, http.StatusOK, "anon GET "+string(p.Visibility)+" by UUID")
		resp.Body.Close()

		resp = doRequest(t, "GET", ts.URL+"/api/v1/leafs/"+p.Slug, nil)
		requireStatus(t, resp, http.StatusOK, "anon GET "+string(p.Visibility)+" by slug")
		resp.Body.Close()
	}
}

// --- handleList visibility (the ?creator_id enumeration fix) ---

// listIDs fetches the list response and returns the set of leaf IDs.
func listIDs(t *testing.T, ts *httptest.Server, url string) map[types.ID]bool {
	t.Helper()
	resp := doRequest(t, "GET", url, nil)
	requireStatus(t, resp, http.StatusOK, "list "+url)
	var listResp types.ListResponse[LeafSummary]
	decodeJSON(t, resp, &listResp)
	ids := make(map[types.ID]bool, len(listResp.Data))
	for _, s := range listResp.Data {
		ids[s.ID] = true
	}
	return ids
}

func TestListByCreatorAnonymousPublicOnly(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-list-anon")
	pub := insertLeaf(t, pool, owner, "List Public", VisibilityPublic)
	priv := insertLeaf(t, pool, owner, "List Private", VisibilityPrivate)
	unl := insertLeaf(t, pool, owner, "List Unlisted", VisibilityUnlisted)

	ts := setupVisibilityServer(t, pool, nil) // anonymous

	ids := listIDs(t, ts, ts.URL+"/api/v1/leafs?creator_id="+owner.String())
	if !ids[pub.ID] {
		t.Error("anon list should include the PUBLIC leaf")
	}
	if ids[priv.ID] {
		t.Error("anon list LEAKED the PRIVATE leaf via ?creator_id")
	}
	if ids[unl.ID] {
		t.Error("anon list LEAKED the UNLISTED leaf via ?creator_id")
	}
}

func TestListByCreatorForeignUserPublicOnly(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-list-owner")
	other := createTestUser(t, pool, "vis-list-other")
	pub := insertLeaf(t, pool, owner, "Foreign List Public", VisibilityPublic)
	priv := insertLeaf(t, pool, owner, "Foreign List Private", VisibilityPrivate)

	ts := setupVisibilityServer(t, pool, &Viewer{UserID: other, Authed: true})

	ids := listIDs(t, ts, ts.URL+"/api/v1/leafs?creator_id="+owner.String())
	if !ids[pub.ID] {
		t.Error("foreign list should include the PUBLIC leaf")
	}
	if ids[priv.ID] {
		t.Error("foreign list LEAKED the owner's PRIVATE leaf via ?creator_id")
	}
}

func TestListByCreatorOwnerSeesAll(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-list-self")
	pub := insertLeaf(t, pool, owner, "Own List Public", VisibilityPublic)
	priv := insertLeaf(t, pool, owner, "Own List Private", VisibilityPrivate)
	unl := insertLeaf(t, pool, owner, "Own List Unlisted", VisibilityUnlisted)

	ts := setupVisibilityServer(t, pool, &Viewer{UserID: owner, Authed: true})

	ids := listIDs(t, ts, ts.URL+"/api/v1/leafs?creator_id="+owner.String())
	if !ids[pub.ID] || !ids[priv.ID] || !ids[unl.ID] {
		t.Errorf("owner list should include all own leafs; got %v", ids)
	}
}

func TestListByCreatorAdminSeesAll(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-list-adminowner")
	pub := insertLeaf(t, pool, owner, "Admin List Public", VisibilityPublic)
	priv := insertLeaf(t, pool, owner, "Admin List Private", VisibilityPrivate)

	ts := setupVisibilityServer(t, pool, &Viewer{UserID: types.NilID(), IsAdmin: true, Authed: true})

	ids := listIDs(t, ts, ts.URL+"/api/v1/leafs?creator_id="+owner.String())
	if !ids[pub.ID] || !ids[priv.ID] {
		t.Errorf("admin list should include all leafs; got %v", ids)
	}
}

func TestListNoCreatorPublicOnlyEvenWhenAuthed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	owner := createTestUser(t, pool, "vis-list-nocreator")
	pub := insertLeaf(t, pool, owner, "Global List Public", VisibilityPublic)
	priv := insertLeaf(t, pool, owner, "Global List Private", VisibilityPrivate)

	// Authenticated as the owner, but WITHOUT a creator_id filter: the global
	// browse view must remain PUBLIC-only.
	ts := setupVisibilityServer(t, pool, &Viewer{UserID: owner, Authed: true})

	ids := listIDs(t, ts, ts.URL+"/api/v1/leafs")
	if !ids[pub.ID] {
		t.Error("global list should include the PUBLIC leaf")
	}
	if ids[priv.ID] {
		t.Error("global list (no creator_id) should NOT include PRIVATE leafs")
	}
}
