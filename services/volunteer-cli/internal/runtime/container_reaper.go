package runtime

import "context"

// WorkUnitIDLabel is the container label the volunteer stamps on every work-unit
// container at creation. It identifies a container as a Lettuce work-unit
// container, so the stranded-container reaper can find and remove leftovers
// without ever touching an unrelated container.
const WorkUnitIDLabel = "lettuce.work-unit-id"

// DataDirLabel is the container label naming the data directory of the
// volunteer that created the container. Two volunteers can share one engine
// (two data dirs on one machine), and a head can hand a unit from one to the
// other, so the unit id alone does not say whose container it is; the label
// keeps each volunteer from ever removing the other's. A container without it
// was created by a build before the label existed and is treated as this
// volunteer's own — those are exactly the leftovers a first launch after the
// upgrade must clean up (TB-74).
const DataDirLabel = "lettuce.data-dir"

// ownsContainer reports whether a labelled work-unit container is this
// volunteer's to manage: it names this data dir, or predates the label.
func (c *ContainerRuntime) ownsContainer(ct ContainerSummary) bool {
	dir, labelled := ct.Labels[DataDirLabel]
	return !labelled || dir == c.dataDir
}

// ReapStrandedContainers force-removes this volunteer's own leftover work-unit
// containers (labeled WorkUnitIDLabel) — those whose unit it no longer owns (an
// active slot or a resumed prefetch unit) — in ANY state. A crash, OOM kill, or
// dirty shutdown skips the normal post-run removal and leaves a container
// created, exited or dead; while it lingers it (a) pins the leaf image — the
// non-force image reaper cannot reclaim an image any container still
// references — and (b) accumulates without bound. A quit that suspended the
// unit leaves its container PAUSED, holding its full memory: when the next
// session could not adopt it (see ResumeWorkUnitContainer), it is a leftover
// like any other, and on a Podman-machine host one frozen 7 GB job is enough to
// kill the next one at model load (exit 137 — TB-74). Running counts too: a
// unit this volunteer does not own has no slot supervising its container.
// Removing them unpins the images and frees the memory.
//
// It is deliberately conservative in what it considers: only containers
// carrying the lettuce work-unit label (never an operator's own containers),
// only this volunteer's own (DataDirLabel), and never a container whose unit
// the volunteer still owns (a just-resumed task's freshly-created container,
// briefly "created", or the container a resumed slot adopted). Best-effort:
// every failure is logged and ignored; it must never block startup.
// ownedWorkUnitIDs may be nil.
func (c *ContainerRuntime) ReapStrandedContainers(ctx context.Context, ownedWorkUnitIDs map[string]bool) {
	containers, err := c.dockerClient.ContainerList(ctx, WorkUnitIDLabel)
	if err != nil {
		c.logger.Debug("ReapStrandedContainers: list failed, skipping", "error", err)
		return
	}

	var removed int
	for _, ct := range containers {
		if !c.ownsContainer(ct) {
			continue // another volunteer's, sharing this engine — leave it
		}
		if wu := ct.Labels[WorkUnitIDLabel]; wu != "" && ownedWorkUnitIDs[wu] {
			continue // a unit we just resumed or still own — leave it
		}
		if err := c.dockerClient.ContainerRemove(ctx, ct.ID); err != nil {
			c.logger.Debug("ReapStrandedContainers: skipped container",
				"container", shortImageID(ct.ID), "state", ct.State, "error", err)
			continue
		}
		removed++
		c.logger.Info("removed stranded work-unit container",
			"container", shortImageID(ct.ID), "state", ct.State,
			"work_unit_id", ct.Labels[WorkUnitIDLabel])
	}
	if removed > 0 {
		c.logger.Info("reaped stranded work-unit containers (unpins their images for the image reaper)",
			"removed", removed)
	}
}

// workUnitContainers returns this volunteer's own containers for one work
// unit, in any state — normally none or one; two is the TB-74 twin.
func (c *ContainerRuntime) workUnitContainers(ctx context.Context, workUnitID string) ([]ContainerSummary, error) {
	containers, err := c.dockerClient.ContainerList(ctx, WorkUnitIDLabel)
	if err != nil {
		return nil, err
	}
	var out []ContainerSummary
	for _, ct := range containers {
		if ct.Labels[WorkUnitIDLabel] == workUnitID && c.ownsContainer(ct) {
			out = append(out, ct)
		}
	}
	return out, nil
}

// RemoveWorkUnitContainers force-removes every container this volunteer holds
// for the work unit except keep (empty keeps none) and reports how many it
// removed. Execute calls it before creating a unit's container — a twin left by
// a previous session (one the relaunch could not adopt, or a stop the engine
// never confirmed) would otherwise run beside the new one on the same work dir,
// holding its memory — and when it adopts a persisted container, for that
// container's siblings (TB-74). The engine's remove is forced: a paused
// container cannot be removed otherwise. Best-effort; failures are logged.
func (c *ContainerRuntime) RemoveWorkUnitContainers(ctx context.Context, workUnitID, keep string) int {
	containers, err := c.workUnitContainers(ctx, workUnitID)
	if err != nil {
		c.logger.Debug("RemoveWorkUnitContainers: list failed, skipping", "work_unit_id", workUnitID, "error", err)
		return 0
	}
	removed := 0
	for _, ct := range containers {
		if ct.ID == keep {
			continue
		}
		if err := c.dockerClient.ContainerRemove(ctx, ct.ID); err != nil {
			c.logger.Warn("failed to remove leftover container of this unit",
				"work_unit_id", workUnitID, "container", shortImageID(ct.ID), "state", ct.State, "error", err)
			continue
		}
		removed++
		c.logger.Info("removed leftover container of this unit",
			"work_unit_id", workUnitID, "container", shortImageID(ct.ID), "state", ct.State)
	}
	return removed
}

// ResumeWorkUnitContainer reports whether the container a previous session
// recorded for the work unit is running again and can be adopted (see
// PrepareResult.OrphanContainerID): a paused one — suspended at quit — is
// unpaused, a running one — left by a crash — is taken as it is. Any other
// state (gone, exited, created, dead) or a failed unpause reports false, and
// the caller re-executes the unit from its preserved work dir; Execute removes
// the leftover first. Only a paused or running container is adopted because
// only those are still the unit's own run: an exited one may be the tail of a
// stop this daemon issued, and collecting its output as a result would submit
// a partial run (TB-74).
func (c *ContainerRuntime) ResumeWorkUnitContainer(ctx context.Context, workUnitID, containerID string) bool {
	containers, err := c.workUnitContainers(ctx, workUnitID)
	if err != nil {
		c.logger.Warn("cannot look up the unit's persisted container; re-executing the unit",
			"work_unit_id", workUnitID, "container", shortImageID(containerID), "error", err)
		return false
	}
	for _, ct := range containers {
		if ct.ID != containerID {
			continue
		}
		switch ct.State {
		case "running":
			c.logger.Info("persisted container is still running; adopting it",
				"work_unit_id", workUnitID, "container", shortImageID(containerID))
			return true
		case "paused":
			if err := c.dockerClient.ContainerUnpause(ctx, containerID); err != nil {
				c.logger.Warn("failed to unpause the unit's persisted container; re-executing the unit",
					"work_unit_id", workUnitID, "container", shortImageID(containerID), "error", err)
				return false
			}
			c.logger.Info("unpaused the unit's persisted container; adopting it",
				"work_unit_id", workUnitID, "container", shortImageID(containerID))
			return true
		default:
			c.logger.Info("persisted container is not resumable; re-executing the unit",
				"work_unit_id", workUnitID, "container", shortImageID(containerID), "state", ct.State)
			return false
		}
	}
	c.logger.Info("persisted container no longer exists; re-executing the unit",
		"work_unit_id", workUnitID, "container", shortImageID(containerID))
	return false
}
