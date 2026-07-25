package leaf

// Wiring tests for the leaf-mutation → dispatch-cache invalidation hook (PB-16 /
// PB-38b): the dispatch cache keeps a per-leaf snapshot that nothing used to
// invalidate — `InvalidateLeaf` had zero production callers — so a visibility flip
// kept dispatching on the old snapshot (UNLISTED units handed to volunteers that
// never pinned the leaf). Every successful leaf mutation must now notify the wired
// DispatchInvalidator, and no REFUSED mutation may (a refused write changed
// nothing, so dispatch has nothing to re-learn). Pool-free via the mockUpdateRepo
// harness; the delete path needs a live database for its CanDelete guard, so its
// call site is covered by the cache-side tests plus the DB-backed handler suite.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// recordingLeafInvalidator captures InvalidateLeaf calls.
type recordingLeafInvalidator struct{ calls []types.ID }

func (r *recordingLeafInvalidator) InvalidateLeaf(id types.ID) { r.calls = append(r.calls, id) }

// mockVersionRepo extends the pool-free mockUpdateRepo with the artifact-version
// surface the publish/activate handlers use (h.artifactRepo type-asserts the repo).
// The embedded nil interface makes any unexercised version method panic loudly.
type mockVersionRepo struct {
	*mockUpdateRepo
	ArtifactVersionRepository
	currentSet []types.ID
}

func (m *mockVersionRepo) PublishVersion(_ context.Context, v *ArtifactVersion) error {
	v.ID = types.NewID()
	return nil
}

func (m *mockVersionRepo) SetCurrentVersion(_ context.Context, _, versionID types.ID) error {
	m.currentSet = append(m.currentSet, versionID)
	return nil
}

func (m *mockVersionRepo) GetVersionByID(_ context.Context, id types.ID) (*ArtifactVersion, error) {
	return &ArtifactVersion{ID: id, VersionLabel: "v1"}, nil
}

func postTransition(t *testing.T, handle http.HandlerFunc, id types.ID, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/leafs/"+id.String()+"/"+path, nil)
	req.SetPathValue("leaf_id", id.String())
	rec := httptest.NewRecorder()
	handle(rec, req)
	return rec
}

// visibilityTestLeaf is newUpdateTestLeaf with a creator identity: the metadata
// validation a visibility change triggers requires one.
func visibilityTestLeaf() *Leaf {
	lf := newUpdateTestLeaf()
	creator := types.NewID()
	lf.CreatorID = &creator
	return lf
}

// TestHandleUpdate_VisibilityFlipInvalidatesDispatch is the wiring for the exact
// repro path of the closeout-#2 leak: PUT {"visibility":"UNLISTED"} must invalidate
// the dispatch cache's snapshot of the leaf in the same request.
func TestHandleUpdate_VisibilityFlipInvalidatesDispatch(t *testing.T) {
	lf := visibilityTestLeaf()
	inv := &recordingLeafInvalidator{}
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}
	h.SetDispatchInvalidator(inv)

	rec := doUpdate(t, h, lf.ID, `{"visibility":"UNLISTED"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != lf.ID {
		t.Fatalf("InvalidateLeaf calls = %v, want exactly [%s]: a visibility flip that does not invalidate is how UNLISTED units leaked to un-pinned volunteers (PB-38b)", inv.calls, lf.ID)
	}
}

// TestHandleUpdate_RefusedUpdateDoesNotInvalidate: a refused write changed nothing,
// so it must not touch the dispatch cache.
func TestHandleUpdate_RefusedUpdateDoesNotInvalidate(t *testing.T) {
	lf := newUpdateTestLeaf()
	inv := &recordingLeafInvalidator{}
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}
	h.SetDispatchInvalidator(inv)

	rec := doUpdate(t, h, lf.ID, `{"validation_config":{"agreement_threshold":1.5}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("setup: update must be refused; body=%s", rec.Body.String())
	}
	if len(inv.calls) != 0 {
		t.Fatalf("InvalidateLeaf calls = %v, want none for a refused update", inv.calls)
	}
}

// TestHandleUpdate_NilInvalidatorIsNoOp: the hook is optional (tests, deployments
// that never start the dispatch cache).
func TestHandleUpdate_NilInvalidatorIsNoOp(t *testing.T) {
	lf := visibilityTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"visibility":"UNLISTED"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no invalidator wired; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleTransition_InvalidatesDispatch: lifecycle state is dispatch-relevant (a
// pause must stop hand-outs within the snapshot's trust window), so a successful
// transition invalidates — and a refused one does not.
func TestHandleTransition_InvalidatesDispatch(t *testing.T) {
	lf := newUpdateTestLeaf() // StateActive
	inv := &recordingLeafInvalidator{}
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}
	h.SetDispatchInvalidator(inv)

	if rec := postTransition(t, h.HandlePause, lf.ID, "pause"); rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != lf.ID {
		t.Fatalf("InvalidateLeaf calls after pause = %v, want exactly [%s]", inv.calls, lf.ID)
	}

	// PAUSED -> ARCHIVED is valid; ARCHIVED -> anything is not. Drive the refusal
	// from ARCHIVED so the no-call-on-refusal half is pinned too.
	if rec := postTransition(t, h.HandleArchive, lf.ID, "archive"); rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 2 {
		t.Fatalf("InvalidateLeaf calls after archive = %v, want 2", inv.calls)
	}
	if rec := postTransition(t, h.HandleResume, lf.ID, "resume"); rec.Code == http.StatusOK {
		t.Fatal("setup: resume from ARCHIVED must be refused")
	}
	if len(inv.calls) != 2 {
		t.Fatalf("InvalidateLeaf calls = %v, want still 2 after a refused transition", inv.calls)
	}
}

// TestHandlePublishVersion_InvalidatesOnActivationOnly: publishing snapshots the
// leaf's execution_config into an immutable version row — the leaf row itself only
// changes when the version is ACTIVATED (SetCurrentVersion denormalizes the config),
// so only the activating publish invalidates.
func TestHandlePublishVersion_InvalidatesOnActivationOnly(t *testing.T) {
	lf := newUpdateTestLeaf()
	lf.ExecutionConfig.Runtime = "NATIVE" // skip the container immutability lint
	inv := &recordingLeafInvalidator{}
	repo := &mockVersionRepo{mockUpdateRepo: &mockUpdateRepo{leaf: lf}}
	h := &LeafHandler{repo: repo, logger: slog.Default()}
	h.SetDispatchInvalidator(inv)

	publish := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/leafs/"+lf.ID.String()+"/versions", bytes.NewBufferString(body))
		req.SetPathValue("leaf_id", lf.ID.String())
		rec := httptest.NewRecorder()
		h.HandlePublishVersion(rec, req)
		return rec
	}

	// Publish WITHOUT activation: the leaf's dispatch-relevant state is unchanged.
	if rec := publish(`{"version_label":"v1","activate":false}`); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 0 {
		t.Fatalf("InvalidateLeaf calls after non-activating publish = %v, want none", inv.calls)
	}

	// Publish WITH activation (the default): the leaf's execution_config is
	// denormalized to the new version — invalidate.
	if rec := publish(`{"version_label":"v2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != lf.ID {
		t.Fatalf("InvalidateLeaf calls after activating publish = %v, want exactly [%s]", inv.calls, lf.ID)
	}
}

// TestHandleActivateVersion_InvalidatesDispatch: promote/rollback repoints the
// leaf's current version (and its denormalized execution_config) — the change
// InvalidateLeaf was originally documented for (TODO #38).
func TestHandleActivateVersion_InvalidatesDispatch(t *testing.T) {
	lf := newUpdateTestLeaf()
	inv := &recordingLeafInvalidator{}
	repo := &mockVersionRepo{mockUpdateRepo: &mockUpdateRepo{leaf: lf}}
	h := &LeafHandler{repo: repo, logger: slog.Default()}
	h.SetDispatchInvalidator(inv)

	versionID := types.NewID()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/leafs/"+lf.ID.String()+"/versions/"+versionID.String()+"/activate", nil)
	req.SetPathValue("leaf_id", lf.ID.String())
	req.SetPathValue("version_id", versionID.String())
	rec := httptest.NewRecorder()
	h.HandleActivateVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate-version status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inv.calls) != 1 || inv.calls[0] != lf.ID {
		t.Fatalf("InvalidateLeaf calls after version activation = %v, want exactly [%s]", inv.calls, lf.ID)
	}
	if len(repo.currentSet) != 1 || repo.currentSet[0] != versionID {
		t.Fatalf("SetCurrentVersion calls = %v, want exactly [%s]", repo.currentSet, versionID)
	}
}
