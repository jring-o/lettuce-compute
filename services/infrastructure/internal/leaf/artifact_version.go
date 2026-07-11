package leaf

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// ArtifactVersion is one immutable, content-addressed published artifact for a leaf
// (TODO #38). A leaf's runnable code (native binaries + per-platform SHA-256, or a
// digest-pinned container image) is captured here as a frozen ExecutionConfig
// snapshot under an operator-chosen VersionLabel. Publishing a new artifact inserts a
// NEW row; it never mutates an existing one. leafs.current_artifact_version_id points
// at the row the leaf currently dispatches; "rollback" is moving that pointer to an
// earlier row.
//
// Because a version row is immutable once published, it is safe to cache by id
// forever — only the leaf's CURRENT pointer changes, which the dispatch path resolves
// with bounded staleness. That is what fixes the running-volunteers-keep-old-artifact
// bug without a wire change: the head simply resolves the current (or, for redundancy,
// the unit-pinned) version per assignment instead of a frozen in-process leaf snapshot.
type ArtifactVersion struct {
	ID           types.ID `json:"id"`
	LeafID       types.ID `json:"leaf_id"`
	VersionLabel string   `json:"version_label"`
	RuntimeType  string   `json:"runtime_type"`
	// ExecutionConfig is the frozen artifact snapshot assignments build from. Once
	// published it is never mutated (PublishVersion is insert-only; Update touches
	// only superseded_at/notes).
	ExecutionConfig ExecutionConfig `json:"execution_config"`
	// ImageDigest is the container content address (sha256:<hex>) when known. The
	// native content address is the per-platform SHA-256 inside ExecutionConfig.
	ImageDigest  *string    `json:"image_digest,omitempty"`
	Notes        *string    `json:"notes,omitempty"`
	PublishedBy  *types.ID  `json:"published_by,omitempty"`
	PublishedAt  time.Time  `json:"published_at"`
	SupersededAt *time.Time `json:"superseded_at,omitempty"` // when it stopped being current; nil = current/never
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// IsCurrent reports whether this version is the leaf's active (non-superseded) one.
func (v *ArtifactVersion) IsCurrent() bool { return v != nil && v.SupersededAt == nil }

// ArtifactVersionRepository is the persistence surface for the per-leaf artifact
// version registry. It is a separate interface from Repository so existing leaf.
// Repository mocks are unaffected; PgxRepository implements both.
type ArtifactVersionRepository interface {
	// PublishVersion inserts an immutable version row. A reused (leaf_id,
	// version_label) is rejected with a Conflict (labels are immutable, never
	// overwritten). On return v is populated with the generated id/timestamps.
	PublishVersion(ctx context.Context, v *ArtifactVersion) error

	// GetVersionByID returns one version row (NotFound if absent). Rows are
	// immutable, so callers may cache the result indefinitely.
	GetVersionByID(ctx context.Context, id types.ID) (*ArtifactVersion, error)

	// ListVersions returns a leaf's versions, newest first (publish history).
	ListVersions(ctx context.Context, leafID types.ID) ([]ArtifactVersion, error)

	// GetCurrentVersion returns the leaf's current version, or (nil, nil) when the
	// leaf has no published version (the legacy path: dispatch from
	// leafs.execution_config directly).
	GetCurrentVersion(ctx context.Context, leafID types.ID) (*ArtifactVersion, error)

	// SetCurrentVersion repoints the leaf at versionID (publish-activate / rollback):
	// stamps superseded_at on the previously-current version, clears it on the new
	// one, and updates leafs.current_artifact_version_id — atomically. The version
	// must belong to the leaf (else NotFound).
	SetCurrentVersion(ctx context.Context, leafID, versionID types.ID) error

	// DeleteVersion removes one version (manual purge) that belongs to leafID.
	// Refused (Conflict) if it is the current version or is pinned by an
	// in-flight (non-terminal) work unit. Every statement is scoped to leafID:
	// a version id whose leaf is not leafID resolves to NotFound and no
	// cross-leaf state is consulted, so an owner of one leaf can never delete
	// or probe another leaf's version (BG-11c).
	DeleteVersion(ctx context.Context, leafID, id types.ID) error

	// PruneVersions enforces a retention policy for one leaf: keep the `keep`
	// most-recently-published versions, delete the rest — never the current version
	// nor any version pinned by an in-flight work unit. keep <= 0 means "keep all"
	// (no-op). Returns the number deleted.
	PruneVersions(ctx context.Context, leafID types.ID, keep int) (int, error)

	// EnsureWorkUnitPin pins a work unit to currentVersionID if it is not already
	// pinned (first-writer-wins) and returns the EFFECTIVE pinned version id (the
	// pre-existing pin if one was set, else currentVersionID). Called at a unit's
	// first dispatch so all redundant replicas of that unit run ONE homogeneous
	// artifact version, even across a mid-flight publish.
	EnsureWorkUnitPin(ctx context.Context, workUnitID, currentVersionID types.ID) (types.ID, error)

	// ResolveWorkUnitVersion returns the artifact version a result for this unit should
	// be stamped with: the unit's pinned version if set, else the leaf's current
	// version (nil when the leaf is unversioned). One read; used at SubmitResult.
	ResolveWorkUnitVersion(ctx context.Context, workUnitID types.ID) (*types.ID, error)

	// PruneAllVersions enforces keep-newest-N retention across ALL leaves in one sweep
	// (the leader-gated GC job). Never deletes a current version or one pinned by
	// in-flight work. keep <= 0 is a no-op. Returns the number deleted.
	PruneAllVersions(ctx context.Context, keep int) (int, error)
}
