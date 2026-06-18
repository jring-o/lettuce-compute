package leaf

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PublishVersionRequest is the body of POST /api/v1/leafs/{leaf_id}/versions. It
// freezes the leaf's CURRENT execution_config into a new immutable version under an
// operator-chosen label (TODO #38). Set the artifact via PUT /leafs/{id} (or
// /configure) first; publishing snapshots it. Defaults to activating the new version.
type PublishVersionRequest struct {
	VersionLabel string  `json:"version_label"`
	Notes        *string `json:"notes,omitempty"`
	// ImageDigest is optional; for a @sha256-pinned image ref it is auto-derived.
	ImageDigest *string `json:"image_digest,omitempty"`
	// Activate defaults to true: make the new version the leaf's current one.
	Activate *bool `json:"activate,omitempty"`
}

// artifactRepo returns the repo as an ArtifactVersionRepository, or writes a 500 and
// returns nil when the configured repo does not support versioning (a non-Pgx test
// repo). The production *PgxRepository implements it.
func (h *LeafHandler) artifactRepo(w http.ResponseWriter) ArtifactVersionRepository {
	av, ok := h.repo.(ArtifactVersionRepository)
	if !ok {
		apierror.WriteError(w, apierror.Internal("artifact versioning not supported by this repository", nil))
		return nil
	}
	return av
}

// HandlePublishVersion handles POST /api/v1/leafs/{leaf_id}/versions.
func (h *LeafHandler) HandlePublishVersion(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	av := h.artifactRepo(w)
	if av == nil {
		return
	}
	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}
	var req PublishVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	if strings.TrimSpace(req.VersionLabel) == "" {
		apierror.WriteError(w, apierror.ValidationError("version_label is required", nil))
		return
	}

	lf, err := h.repo.GetByID(r.Context(), leafID)
	if err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Immutability lint: a CONTAINER artifact must be pinned to an immutable ref
	// (digest or non-:latest tag) so a re-pushed image can never silently change what
	// volunteers run — the footgun behind TODO #38.
	if strings.EqualFold(lf.ExecutionConfig.Runtime, "CONTAINER") {
		if apiErr := validateContainerImageImmutable(strOrEmpty(lf.ExecutionConfig.Image)); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
	}

	imageDigest := req.ImageDigest
	if d := imageDigestFromRef(strOrEmpty(lf.ExecutionConfig.Image)); d != "" {
		imageDigest = &d
	}

	v := &ArtifactVersion{
		LeafID:          leafID,
		VersionLabel:    strings.TrimSpace(req.VersionLabel),
		RuntimeType:     lf.ExecutionConfig.Runtime,
		ExecutionConfig: lf.ExecutionConfig,
		ImageDigest:     imageDigest,
		Notes:           req.Notes,
	}
	if err := av.PublishVersion(r.Context(), v); err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Activate by default: make it the current version (also denormalizes the leaf's
	// execution_config to this snapshot). Cross-replica dispatch caches pick the change
	// up within their leaf-snapshot TTL — no head restart (TODO #38).
	activate := req.Activate == nil || *req.Activate
	if activate {
		if err := av.SetCurrentVersion(r.Context(), leafID, v.ID); err != nil {
			apierror.WriteError(w, apierror.FromError(err))
			return
		}
	}
	l.Info("artifact version published", "leaf_id", leafID, "version_id", v.ID, "label", v.VersionLabel, "activated", activate)
	writeJSON(w, http.StatusCreated, v)
}

// HandleListVersions handles GET /api/v1/leafs/{leaf_id}/versions (publish history,
// newest first).
func (h *LeafHandler) HandleListVersions(w http.ResponseWriter, r *http.Request) {
	av := h.artifactRepo(w)
	if av == nil {
		return
	}
	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}
	versions, err := av.ListVersions(r.Context(), leafID)
	if err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if versions == nil {
		versions = []ArtifactVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": versions})
}

// HandleActivateVersion handles POST /api/v1/leafs/{leaf_id}/versions/{version_id}/activate.
// This is both "promote a freshly published version" and "roll back to a prior one".
func (h *LeafHandler) HandleActivateVersion(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	av := h.artifactRepo(w)
	if av == nil {
		return
	}
	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}
	versionID, err := types.ParseID(r.PathValue("version_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid version_id: must be a valid UUID", nil))
		return
	}
	if err := av.SetCurrentVersion(r.Context(), leafID, versionID); err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	v, err := av.GetVersionByID(r.Context(), versionID)
	if err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	l.Info("artifact version activated", "leaf_id", leafID, "version_id", versionID, "label", v.VersionLabel)
	writeJSON(w, http.StatusOK, v)
}

// HandleDeleteVersion handles DELETE /api/v1/leafs/{leaf_id}/versions/{version_id}
// (manual purge; refused for the current version or a version pinned by in-flight work).
func (h *LeafHandler) HandleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	av := h.artifactRepo(w)
	if av == nil {
		return
	}
	versionID, err := types.ParseID(r.PathValue("version_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid version_id: must be a valid UUID", nil))
		return
	}
	if err := av.DeleteVersion(r.Context(), versionID); err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// imageDigestFromRef extracts "sha256:<hex>" from a digest-pinned image ref, else "".
func imageDigestFromRef(image string) string {
	if i := strings.Index(image, "@sha256:"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// validateContainerImageImmutable rejects a mutable container image ref (a bare image
// or one tagged :latest), which is the root footgun of TODO #38.
func validateContainerImageImmutable(image string) *apierror.APIError {
	if strings.TrimSpace(image) == "" {
		return apierror.ValidationError("container artifact requires an image reference", nil)
	}
	if strings.Contains(image, "@sha256:") {
		return nil // a digest ref is content-addressed and immutable
	}
	name := image
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	colon := strings.LastIndex(name, ":")
	if colon < 0 {
		return apierror.ValidationError("container image must be pinned to an immutable tag or @sha256 digest (a bare image defaults to the mutable ':latest')", nil)
	}
	tag := name[colon+1:]
	if tag == "" || strings.EqualFold(tag, "latest") {
		return apierror.ValidationError("container image must not use the mutable ':latest' tag; pin an immutable version tag (e.g. ':2.0') or an @sha256 digest", nil)
	}
	return nil
}
