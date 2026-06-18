//go:build integration

package leaf

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func publishTestVersion(t *testing.T, repo *PgxRepository, leafID types.ID, label string) *ArtifactVersion {
	t.Helper()
	v := &ArtifactVersion{
		LeafID:       leafID,
		VersionLabel: label,
		RuntimeType:  "NATIVE",
		ExecutionConfig: ExecutionConfig{
			Runtime:         "NATIVE",
			BinaryChecksums: map[string]string{"linux_amd64": label + "-checksum"},
		},
	}
	if err := repo.PublishVersion(context.Background(), v); err != nil {
		t.Fatalf("PublishVersion(%s): %v", label, err)
	}
	return v
}

// TestArtifactVersionRegistry covers publish (with immutable-label dedup), activate,
// supersede + execution_config denormalization, history order, rollback, and the
// delete guard for the current version (TODO #38).
func TestArtifactVersionRegistry(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	userID := createTestUser(t, pool, "artifactowner")
	lf := newTestLeaf(&userID)
	if err := repo.Create(ctx, lf); err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	v1 := publishTestVersion(t, repo, lf.ID, "v1")
	if err := repo.SetCurrentVersion(ctx, lf.ID, v1.ID); err != nil {
		t.Fatalf("SetCurrentVersion v1: %v", err)
	}
	cur, err := repo.GetCurrentVersion(ctx, lf.ID)
	if err != nil || cur == nil || cur.ID != v1.ID {
		t.Fatalf("GetCurrentVersion: want v1, got %v err=%v", cur, err)
	}

	// Immutable labels: re-publishing a used label is rejected.
	dup := &ArtifactVersion{LeafID: lf.ID, VersionLabel: "v1", RuntimeType: "NATIVE"}
	if err := repo.PublishVersion(ctx, dup); err == nil {
		t.Fatal("expected duplicate version_label to be rejected")
	}

	// Publish + activate v2 -> v1 superseded, leaf execution_config denormalized to v2.
	v2 := publishTestVersion(t, repo, lf.ID, "v2")
	if err := repo.SetCurrentVersion(ctx, lf.ID, v2.ID); err != nil {
		t.Fatalf("SetCurrentVersion v2: %v", err)
	}
	reread, err := repo.GetByID(ctx, lf.ID)
	if err != nil {
		t.Fatalf("re-read leaf: %v", err)
	}
	if reread.CurrentArtifactVersionID == nil || *reread.CurrentArtifactVersionID != v2.ID {
		t.Fatalf("leaf current pointer not moved to v2")
	}
	if got := reread.ExecutionConfig.BinaryChecksums["linux_amd64"]; got != "v2-checksum" {
		t.Fatalf("execution_config not denormalized to v2: got %q", got)
	}

	// History newest-first.
	versions, err := repo.ListVersions(ctx, lf.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 || versions[0].ID != v2.ID {
		t.Fatalf("history: want [v2,v1], got %d entries (first=%v)", len(versions), versions[0].ID)
	}

	// Rollback to v1.
	if err := repo.SetCurrentVersion(ctx, lf.ID, v1.ID); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	if cur, _ = repo.GetCurrentVersion(ctx, lf.ID); cur == nil || cur.ID != v1.ID {
		t.Fatalf("after rollback: want v1, got %v", cur)
	}

	// The current version cannot be deleted; a non-current/unpinned one can.
	if err := repo.DeleteVersion(ctx, v1.ID); err == nil {
		t.Fatal("expected delete of current version to be refused")
	}
	if err := repo.DeleteVersion(ctx, v2.ID); err != nil {
		t.Fatalf("delete non-current v2: %v", err)
	}
}

// TestArtifactVersionPinning covers EnsureWorkUnitPin (first-writer-wins),
// ResolveWorkUnitVersion, and the delete guard for a version pinned by a live unit.
func TestArtifactVersionPinning(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	userID := createTestUser(t, pool, "pinowner")
	lf := newTestLeaf(&userID)
	if err := repo.Create(ctx, lf); err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	v1 := publishTestVersion(t, repo, lf.ID, "v1")
	if err := repo.SetCurrentVersion(ctx, lf.ID, v1.ID); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	v2 := publishTestVersion(t, repo, lf.ID, "v2")

	wuID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_units (id, leaf_id, state, code_artifact_ref, deadline_seconds)
		VALUES ($1, $2, 'QUEUED', 'na', 600)`, wuID, lf.ID); err != nil {
		t.Fatalf("insert work unit: %v", err)
	}

	// First pin wins; a later pin to a different version is ignored.
	if got, err := repo.EnsureWorkUnitPin(ctx, wuID, v1.ID); err != nil || got != v1.ID {
		t.Fatalf("EnsureWorkUnitPin first: got=%v err=%v", got, err)
	}
	if got, err := repo.EnsureWorkUnitPin(ctx, wuID, v2.ID); err != nil || got != v1.ID {
		t.Fatalf("EnsureWorkUnitPin second must stay v1: got=%v err=%v", got, err)
	}

	// ResolveWorkUnitVersion returns the pin.
	if rv, err := repo.ResolveWorkUnitVersion(ctx, wuID); err != nil || rv == nil || *rv != v1.ID {
		t.Fatalf("ResolveWorkUnitVersion: got=%v err=%v", rv, err)
	}

	// A version pinned by a non-terminal unit cannot be deleted.
	if err := repo.DeleteVersion(ctx, v1.ID); err == nil {
		t.Fatal("expected delete of a live-pinned version to be refused")
	}
}
