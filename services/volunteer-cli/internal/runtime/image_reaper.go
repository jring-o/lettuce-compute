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

	c.reapRepos(ctx, keep, map[string]bool{repo: true})
}

// ReapStaleImages reaps superseded cached copies across EVERY repository the
// volunteer currently wants cached. Unlike the per-pull reapStaleImages it is
// NOT gated on a fresh pull, so it cleans orphans the pull-triggered reaper never
// sees — most importantly a stale sibling tag left when a leaf moved to a new tag
// (e.g. lbry.science/beyblade :latest / :v2-progress → :v3-checkpoint) while this
// volunteer already had the old image cached, so no pull ever re-fires the
// reaper. Confirmed lingering in the field on v0.8.11 across both Docker and
// rootless Podman.
//
// Safe to run at startup and whenever the enabled-image set changes; best-effort
// and never blocks compute. No-op until SetWantedImages is installed (so a
// native-only volunteer or one with no enabled container leaf removes nothing).
func (c *ContainerRuntime) ReapStaleImages(ctx context.Context) {
	if c.wantedImages == nil {
		return
	}
	keep := make(map[string]bool)
	repos := make(map[string]bool)
	for _, ref := range c.wantedImages() {
		repo := repoFromImageRef(ref)
		if repo == "" {
			continue
		}
		repos[repo] = true
		if id, err := c.dockerClient.ImageID(ctx, ref); err == nil && id != "" {
			keep[id] = true
		}
	}
	if len(keep) == 0 {
		// No wanted image resolves yet (none pulled, or the engine is unreachable):
		// refuse to remove anything rather than risk deleting a live image.
		c.logger.Debug("ReapStaleImages: empty keep-set, skipping")
		return
	}
	c.reapRepos(ctx, keep, repos)
}

// reapRepos removes every cached image that belongs to one of repos but whose
// content ID is not in keep — the shared core of the per-pull and across-wanted
// reapers. It only ever touches a repository the caller manages (one that has a
// wanted/just-pulled image), so unrelated images are never candidates. Removal
// is non-force, so the backend refuses to delete an image still referenced by a
// container and the reaper simply skips it. Every failure is logged and ignored;
// it must never block compute.
func (c *ContainerRuntime) reapRepos(ctx context.Context, keep, repos map[string]bool) {
	images, err := c.dockerClient.ImageList(ctx)
	if err != nil {
		c.logger.Debug("reapStaleImages: image list failed, skipping", "error", err)
		return
	}

	var removed int
	var reclaimedBytes int64
	for _, img := range images {
		if keep[img.ID] {
			continue
		}
		repo, ok := matchRepo(img, repos)
		if !ok {
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
			"removed", removed, "reclaimed_mb", reclaimedBytes/(1024*1024))
	}
}

// matchRepo returns the repository from repos that img belongs to, and whether
// any matched.
func matchRepo(img ImageSummary, repos map[string]bool) (string, bool) {
	for repo := range repos {
		if imageInRepo(img, repo) {
			return repo, true
		}
	}
	return "", false
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
