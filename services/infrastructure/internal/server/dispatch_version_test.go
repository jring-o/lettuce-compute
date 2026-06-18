package server

import (
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestGetLeafRefreshesAfterTTLAndInvalidate is the unit proof for TODO #38: the
// dispatch cache's leaf snapshot is bounded by leafSnapshotTTL and re-read after an
// artifact change, and InvalidateLeaf forces an immediate re-read — so a RUNNING
// volunteer's next assignment carries the NEW artifact with no head restart.
func TestGetLeafRefreshesAfterTTLAndInvalidate(t *testing.T) {
	leafID, err := types.ParseID("33333333-3333-3333-3333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	mkLeaf := func(checksum string) *leaf.Leaf {
		return &leaf.Leaf{
			ID: leafID,
			ExecutionConfig: leaf.ExecutionConfig{
				Runtime:         "NATIVE",
				BinaryChecksums: map[string]string{"linux_amd64": checksum},
			},
		}
	}

	leafRepo := &fakeLeafRepo{leafs: map[types.ID]*leaf.Leaf{leafID: mkLeaf("aaa")}}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})

	// Deterministic clock.
	now := time.Now()
	c.now = func() time.Time { return now }

	checksum := func() string {
		lf, gerr := c.getLeaf(leafID)
		if gerr != nil {
			t.Fatalf("getLeaf: %v", gerr)
		}
		return lf.ExecutionConfig.BinaryChecksums["linux_amd64"]
	}
	setLeaf := func(cs string) {
		leafRepo.mu.Lock()
		leafRepo.leafs[leafID] = mkLeaf(cs)
		leafRepo.mu.Unlock()
	}

	// First read caches the original artifact.
	if got := checksum(); got != "aaa" {
		t.Fatalf("initial: want aaa, got %q", got)
	}

	// Operator updates the artifact (new checksum). Within the TTL the cached snapshot
	// is still served (no per-request DB thrash).
	setLeaf("bbb")
	if got := checksum(); got != "aaa" {
		t.Fatalf("within TTL: want stale aaa, got %q", got)
	}

	// Once the TTL elapses, the next build-path read re-fetches the NEW artifact.
	now = now.Add(c.cfg.leafSnapshotTTL + time.Second)
	if got := checksum(); got != "bbb" {
		t.Fatalf("after TTL: want bbb, got %q", got)
	}

	// An explicit invalidation (publish/rollback hook) forces an immediate re-read,
	// independent of the clock.
	setLeaf("ccc")
	if got := checksum(); got != "bbb" {
		t.Fatalf("freshly re-cached, pre-invalidate: want bbb, got %q", got)
	}
	c.InvalidateLeaf(leafID)
	if got := checksum(); got != "ccc" {
		t.Fatalf("after InvalidateLeaf: want ccc, got %q", got)
	}
}
