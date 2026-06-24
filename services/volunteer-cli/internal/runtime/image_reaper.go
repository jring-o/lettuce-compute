package runtime

import (
	"context"
	"strings"
)

// SetWantedImages installs a callback returning every container image reference
// the volunteer currently wants cached — the images of all enabled leaves across
// all attached heads. The stale-image reaper keeps these and removes only
// superseded copies. When unset (nil), the reaper keeps only the image it just
// pulled, which is still safe (it never removes the live image).
func (c *ContainerRuntime) SetWantedImages(fn func() []string) { c.wantedImages = fn }

// reapStaleImages removes cached images of the same repository as pulledRef that
// the volunteer no longer wants — the superseded copies left behind when a
// mutable tag is re-pushed or a leaf moves to a new image digest (TODO #60; the
// disk-reclamation companion to the artifact-freshness fix #38).
//
// Without this, every republish leaks a full image copy (the Beyblade leaf's
// ~471 MB, GREP's tens of GB) into the backend's image store. Crucially,
// `podman/docker image prune` does NOT reclaim these: a superseded copy keeps a
// repo digest reference ("repo@sha256:…") and so is not "dangling", so the copies
// accumulate until they consume the volunteer's max_disk_gb allowance and the
// disk gate stalls all work fetching.
//
// It is deliberately conservative:
//   - it only considers images sharing pulledRef's repository;
//   - it KEEPS every image the volunteer still wants — the just-pulled image plus
//     every image backing another enabled leaf — so two active leaves that share a
//     repository under different tags/digests (e.g. grep-cpu :1.2 and grep-gpu
//     :1.3-gpu) never reap each other;
//   - removal is non-force, so the backend refuses to delete an image still
//     referenced by a container and the reaper simply skips it.
//
// Best-effort: every failure is logged and ignored. It must never block compute.
func (c *ContainerRuntime) reapStaleImages(ctx context.Context, pulledRef string) {
	repo := repoFromImageRef(pulledRef)
	if repo == "" {
		return
	}

	// Build the keep-set: content IDs of every image the volunteer still wants.
	keep := make(map[string]bool)
	if id, err := c.dockerClient.ImageID(ctx, pulledRef); err == nil && id != "" {
		keep[id] = true
	}
	if c.wantedImages != nil {
		for _, ref := range c.wantedImages() {
			// Only same-repo refs can collide with what we might remove below.
			if repoFromImageRef(ref) != repo {
				continue
			}
			if id, err := c.dockerClient.ImageID(ctx, ref); err == nil && id != "" {
				keep[id] = true
			}
		}
	}
	if len(keep) == 0 {
		// Couldn't even resolve the just-pulled image: refuse to remove anything
		// rather than risk deleting the live image.
		c.logger.Debug("reapStaleImages: empty keep-set, skipping", "repo", repo)
		return
	}

	images, err := c.dockerClient.ImageList(ctx)
	if err != nil {
		c.logger.Debug("reapStaleImages: image list failed, skipping", "repo", repo, "error", err)
		return
	}

	var removed int
	var reclaimedBytes int64
	for _, img := range images {
		if keep[img.ID] || !imageInRepo(img, repo) {
			continue
		}
		if err := c.dockerClient.ImageRemove(ctx, img.ID); err != nil {
			// In use by a container, multi-tagged without force, or already gone.
			c.logger.Debug("reapStaleImages: skipped image",
				"repo", repo, "image_id", shortImageID(img.ID), "error", err)
			continue
		}
		removed++
		reclaimedBytes += img.Size
		c.logger.Info("reaped superseded cached image",
			"repo", repo, "image_id", shortImageID(img.ID), "size_mb", img.Size/(1024*1024))
	}
	if removed > 0 {
		c.logger.Info("reclaimed superseded image copies",
			"repo", repo, "removed", removed, "reclaimed_mb", reclaimedBytes/(1024*1024))
	}
}

// repoFromImageRef returns the repository portion of an OCI image reference,
// stripping any tag and/or digest. Examples:
//
//	lbry.science/beyblade:v2-progress        -> lbry.science/beyblade
//	lbry.science/beyblade@sha256:abc…        -> lbry.science/beyblade
//	registry:5000/team/img:tag               -> registry:5000/team/img
//	ghcr.io/jring-o/extract2-lettuce:1.3-gpu -> ghcr.io/jring-o/extract2-lettuce
//
// Returns "" for an empty reference.
func repoFromImageRef(ref string) string {
	if ref == "" {
		return ""
	}
	// Strip digest.
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// Strip tag: the tag separator is the last ':' after the last '/', so the
	// registry-host port colon in "host:port/path" is not mistaken for it.
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return ref
}

// imageInRepo reports whether a cached image belongs to repo. The trailing
// separator (":" for tags, "@" for digests) prevents a repo from matching a
// longer sibling — e.g. "lbry.science/beyblade" must not match
// "lbry.science/beyblade-native:latest".
func imageInRepo(img ImageSummary, repo string) bool {
	for _, t := range img.RepoTags {
		if strings.HasPrefix(t, repo+":") {
			return true
		}
	}
	for _, dg := range img.RepoDigests {
		if strings.HasPrefix(dg, repo+"@") {
			return true
		}
	}
	return false
}

// shortImageID trims an image ID ("sha256:abcdef…" or "abcdef…") to 12 hex chars
// for readable logs.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
