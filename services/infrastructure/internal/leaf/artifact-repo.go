package leaf

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// artifactVersionColumns is the standard column list for SELECTs (matches
// scanArtifactVersion order).
const artifactVersionColumns = `id, leaf_id, version_label, runtime_type,
	execution_config, image_digest, notes, published_by,
	published_at, superseded_at, created_at, updated_at`

// artifactVersionColumnsAV is the same list aliased to `av` for JOIN queries.
const artifactVersionColumnsAV = `av.id, av.leaf_id, av.version_label, av.runtime_type,
	av.execution_config, av.image_digest, av.notes, av.published_by,
	av.published_at, av.superseded_at, av.created_at, av.updated_at`

// terminalWorkUnitStates are the work-unit states that no longer execute, so a
// version they pin is safe to delete. Kept inline (not importing workunit) to avoid
// an import cycle (workunit already depends on leaf).
const sqlNonTerminalPinFilter = `state NOT IN ('VALIDATED','FAILED')`

// sqlOpenAuditVersionFilter, embedded in the PRUNE paths only (audit spec §7.5, F-M7),
// additionally protects a version referenced by an OPEN (QUEUED/CLAIMED) result audit: the
// runner must be able to re-execute the pinned artifact for the life of the audit. The join
// target is the pruning candidate aliased `r`. DeleteVersion is DELIBERATELY not extended —
// a new owner-visible refusal on an all-terminal version would leak an audit-presence oracle;
// there, deletion proceeds and the audit degrades to INCONCLUSIVE via SET NULL at claim time.
const sqlOpenAuditVersionFilter = `NOT EXISTS (
			SELECT 1 FROM result_audits ra
			WHERE ra.artifact_version_id = r.id AND ra.status IN ('QUEUED','CLAIMED')
		)`

func scanArtifactVersion(row pgx.Row) (*ArtifactVersion, error) {
	var v ArtifactVersion
	err := row.Scan(
		&v.ID,
		&v.LeafID,
		&v.VersionLabel,
		&v.RuntimeType,
		&v.ExecutionConfig,
		&v.ImageDigest,
		&v.Notes,
		&v.PublishedBy,
		&v.PublishedAt,
		&v.SupersededAt,
		&v.CreatedAt,
		&v.UpdatedAt,
	)
	return &v, err
}

// PublishVersion inserts an immutable version row.
func (r *PgxRepository) PublishVersion(ctx context.Context, v *ArtifactVersion) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO leaf_artifact_versions (
			leaf_id, version_label, runtime_type, execution_config,
			image_digest, notes, published_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+artifactVersionColumns,
		v.LeafID, v.VersionLabel, v.RuntimeType, v.ExecutionConfig,
		v.ImageDigest, v.Notes, v.PublishedBy,
	)
	result, err := scanArtifactVersion(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return apierror.Conflict(
				"artifact version label already exists for this leaf (labels are immutable)",
				map[string]string{"version_label": v.VersionLabel},
			)
		}
		return apierror.Internal("failed to publish artifact version", err)
	}
	*v = *result
	return nil
}

// GetVersionByID returns one version row.
func (r *PgxRepository) GetVersionByID(ctx context.Context, id types.ID) (*ArtifactVersion, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+artifactVersionColumns+" FROM leaf_artifact_versions WHERE id = $1", id)
	v, err := scanArtifactVersion(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("artifact_version", id.String())
		}
		return nil, apierror.Internal("failed to get artifact version", err)
	}
	return v, nil
}

// ListVersions returns a leaf's versions newest-first.
func (r *PgxRepository) ListVersions(ctx context.Context, leafID types.ID) ([]ArtifactVersion, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+artifactVersionColumns+" FROM leaf_artifact_versions WHERE leaf_id = $1 ORDER BY published_at DESC", leafID)
	if err != nil {
		return nil, apierror.Internal("failed to list artifact versions", err)
	}
	defer rows.Close()

	var out []ArtifactVersion
	for rows.Next() {
		v, err := scanArtifactVersion(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan artifact version", err)
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to read artifact versions", err)
	}
	return out, nil
}

// GetCurrentVersion returns the leaf's current version, or (nil, nil) if unset.
func (r *PgxRepository) GetCurrentVersion(ctx context.Context, leafID types.ID) (*ArtifactVersion, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+artifactVersionColumnsAV+`
		FROM leafs l
		JOIN leaf_artifact_versions av ON av.id = l.current_artifact_version_id
		WHERE l.id = $1`, leafID)
	v, err := scanArtifactVersion(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Leaf has no current version: legacy path (dispatch from execution_config).
			return nil, nil
		}
		return nil, apierror.Internal("failed to get current artifact version", err)
	}
	return v, nil
}

// SetCurrentVersion repoints the leaf at versionID atomically.
func (r *PgxRepository) SetCurrentVersion(ctx context.Context, leafID, versionID types.ID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return apierror.Internal("failed to begin tx", err)
	}
	defer tx.Rollback(ctx)

	// The version must belong to this leaf.
	var belongs bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM leaf_artifact_versions WHERE id = $1 AND leaf_id = $2)`,
		versionID, leafID).Scan(&belongs); err != nil {
		return apierror.Internal("failed to verify artifact version", err)
	}
	if !belongs {
		return apierror.NotFound("artifact_version", versionID.String())
	}

	// Read the previously-current version (may be NULL).
	var oldCurrent *types.ID
	if err := tx.QueryRow(ctx,
		`SELECT current_artifact_version_id FROM leafs WHERE id = $1`, leafID).Scan(&oldCurrent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apierror.NotFound("leaf", leafID.String())
		}
		return apierror.Internal("failed to read current artifact version", err)
	}

	if oldCurrent != nil && *oldCurrent != versionID {
		if _, err := tx.Exec(ctx,
			`UPDATE leaf_artifact_versions SET superseded_at = NOW()
			 WHERE id = $1 AND superseded_at IS NULL`, *oldCurrent); err != nil {
			return apierror.Internal("failed to supersede previous version", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE leaf_artifact_versions SET superseded_at = NULL WHERE id = $1`, versionID); err != nil {
		return apierror.Internal("failed to clear supersede on new version", err)
	}
	// Repoint the leaf AND denormalize its execution_config to the activated version's
	// snapshot. Keeping leafs.execution_config a mirror of the current version means
	// every existing reader (the unpinned dispatch path, GetHeadInfo badges, capability
	// matching, validation-config bounds, the browser volunteer) sees the current
	// published artifact with no version-awareness of its own — the registry layers
	// history/rollback/pinning on top without rewiring them (TODO #38).
	if _, err := tx.Exec(ctx,
		`UPDATE leafs SET
			current_artifact_version_id = $1,
			execution_config = (SELECT execution_config FROM leaf_artifact_versions WHERE id = $1)
		 WHERE id = $2`, versionID, leafID); err != nil {
		return apierror.Internal("failed to set current artifact version", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return apierror.Internal("failed to commit current-version change", err)
	}
	return nil
}

// DeleteVersion removes one version of leafID, refusing if it is current or
// live-pinned. Every statement is scoped `AND leaf_id = leafID`, so a version
// that belongs to a DIFFERENT leaf is invisible here: the existence check
// returns NotFound before the current-version / live-pin guards run, and the
// guards themselves cannot observe another leaf's state. This is the object
// scoping behind the authOwner wrapper on the route — the wrapper authorizes
// the path {leaf_id}, and this makes the SQL act on that same leaf and no
// other (BG-11c).
func (r *PgxRepository) DeleteVersion(ctx context.Context, leafID, id types.ID) error {
	// Existence-within-leaf first: a version id whose leaf_id != leafID is
	// NotFound, and we never consult its current/pin state (no cross-leaf oracle).
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM leaf_artifact_versions WHERE id = $1 AND leaf_id = $2)`,
		id, leafID).Scan(&exists); err != nil {
		return apierror.Internal("failed to check artifact version", err)
	}
	if !exists {
		return apierror.NotFound("artifact_version", id.String())
	}

	var isCurrent bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM leafs WHERE id = $2 AND current_artifact_version_id = $1)`,
		id, leafID).Scan(&isCurrent); err != nil {
		return apierror.Internal("failed to check current-version usage", err)
	}
	if isCurrent {
		return apierror.Conflict("cannot delete the current artifact version; activate another first", nil)
	}

	var livePinned bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM work_units
			WHERE pinned_artifact_version_id = $1 AND leaf_id = $2 AND `+sqlNonTerminalPinFilter+`)`,
		id, leafID).Scan(&livePinned); err != nil {
		return apierror.Internal("failed to check work-unit pins", err)
	}
	if livePinned {
		return apierror.Conflict("cannot delete a version still pinned by in-flight work units", nil)
	}

	tag, err := r.pool.Exec(ctx,
		`DELETE FROM leaf_artifact_versions WHERE id = $1 AND leaf_id = $2`, id, leafID)
	if err != nil {
		return apierror.Internal("failed to delete artifact version", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("artifact_version", id.String())
	}
	return nil
}

// PruneVersions enforces a keep-newest-N retention for one leaf.
func (r *PgxRepository) PruneVersions(ctx context.Context, leafID types.ID, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil // keep all
	}

	rows, err := r.pool.Query(ctx, `
		WITH ranked AS (
			SELECT id, row_number() OVER (ORDER BY published_at DESC) AS rn
			FROM leaf_artifact_versions
			WHERE leaf_id = $1
		)
		SELECT r.id FROM ranked r
		WHERE r.rn > $2
		  AND r.id NOT IN (
			SELECT current_artifact_version_id FROM leafs
			WHERE id = $1 AND current_artifact_version_id IS NOT NULL
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM work_units w
			WHERE w.pinned_artifact_version_id = r.id AND w.`+sqlNonTerminalPinFilter+`
		  )
		  AND `+sqlOpenAuditVersionFilter,
		leafID, keep)
	if err != nil {
		return 0, apierror.Internal("failed to select prunable versions", err)
	}
	var ids []types.ID
	for rows.Next() {
		var id types.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, apierror.Internal("failed to scan prunable version id", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, apierror.Internal("failed to read prunable versions", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tag, err := r.pool.Exec(ctx,
		`DELETE FROM leaf_artifact_versions WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, apierror.Internal("failed to prune artifact versions", err)
	}
	return int(tag.RowsAffected()), nil
}

// EnsureWorkUnitPin pins workUnitID to currentVersionID if unpinned (first-writer-
// wins via COALESCE) and returns the effective pin.
func (r *PgxRepository) EnsureWorkUnitPin(ctx context.Context, workUnitID, currentVersionID types.ID) (types.ID, error) {
	var pinned types.ID
	err := r.pool.QueryRow(ctx, `
		UPDATE work_units
		SET pinned_artifact_version_id = COALESCE(pinned_artifact_version_id, $2)
		WHERE id = $1
		RETURNING pinned_artifact_version_id`,
		workUnitID, currentVersionID).Scan(&pinned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.ID{}, apierror.NotFound("work_unit", workUnitID.String())
		}
		return types.ID{}, apierror.Internal("failed to pin work unit artifact version", err)
	}
	return pinned, nil
}

// PruneAllVersions enforces keep-newest-N retention across all leaves in one sweep.
func (r *PgxRepository) PruneAllVersions(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil // keep all
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM leaf_artifact_versions
		WHERE id IN (
			WITH ranked AS (
				SELECT id, row_number() OVER (PARTITION BY leaf_id ORDER BY published_at DESC) AS rn
				FROM leaf_artifact_versions
			)
			SELECT r.id FROM ranked r
			WHERE r.rn > $1
			  AND r.id NOT IN (
				SELECT current_artifact_version_id FROM leafs WHERE current_artifact_version_id IS NOT NULL
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM work_units w
				WHERE w.pinned_artifact_version_id = r.id AND w.`+sqlNonTerminalPinFilter+`
			  )
			  AND `+sqlOpenAuditVersionFilter+`
		)`, keep)
	if err != nil {
		return 0, apierror.Internal("failed to prune artifact versions (all leaves)", err)
	}
	return int(tag.RowsAffected()), nil
}

// ResolveWorkUnitVersion returns the unit's pinned version, else the leaf's current
// version (nil if unversioned), in one read.
func (r *PgxRepository) ResolveWorkUnitVersion(ctx context.Context, workUnitID types.ID) (*types.ID, error) {
	var vid *types.ID
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(wu.pinned_artifact_version_id, l.current_artifact_version_id)
		FROM work_units wu
		JOIN leafs l ON l.id = wu.leaf_id
		WHERE wu.id = $1`, workUnitID).Scan(&vid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("work_unit", workUnitID.String())
		}
		return nil, apierror.Internal("failed to resolve work unit version", err)
	}
	return vid, nil
}
