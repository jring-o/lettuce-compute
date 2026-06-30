package runtime

import "context"

// WorkUnitIDLabel is the container label the volunteer stamps on every work-unit
// container at creation. It identifies a container as this volunteer's own, so
// the stranded-container reaper can find and remove leftovers without ever
// touching an unrelated container.
const WorkUnitIDLabel = "lettuce.work-unit-id"

// strandedContainerStates are the inactive container states whose work-unit
// containers are safe to reap at startup. A crash, OOM kill, or dirty shutdown
// skips the normal post-run removal and leaves a container in one of these
// states; while it lingers it (a) pins the leaf image — the non-force image
// reaper cannot reclaim an image any container still references — and (b)
// accumulates without bound. A "created" container that never started counts:
// the field reports show weeks-old "Created" leftovers pinning an image.
// Running / paused / restarting containers (active work, or a suspended slot)
// are deliberately absent, so the reaper never touches live work.
var strandedContainerStates = map[string]bool{
	"created": true,
	"exited":  true,
	"dead":    true,
}

// ReapStrandedContainers force-removes this volunteer's own leftover work-unit
// containers (labeled WorkUnitIDLabel) that are in an inactive state and are not
// currently owned (an active slot or a resumed prefetch unit). Removing them
// unpins the leaf images they were holding — which is exactly why a freshly
// started v0.8.12 volunteer could not reclaim a superseded image even though the
// image reaper now runs at startup — and stops crashed/dirty-shutdown containers
// from piling up.
//
// It is deliberately conservative: it only considers containers carrying the
// lettuce work-unit label (never an operator's own containers), only inactive
// states (never a running/paused container), and skips any container whose unit
// the volunteer still owns (so a just-resumed task's freshly-created container,
// briefly in the "created" state, is spared). Best-effort: every failure is
// logged and ignored; it must never block startup. ownedWorkUnitIDs may be nil.
func (c *ContainerRuntime) ReapStrandedContainers(ctx context.Context, ownedWorkUnitIDs map[string]bool) {
	containers, err := c.dockerClient.ContainerList(ctx, WorkUnitIDLabel)
	if err != nil {
		c.logger.Debug("ReapStrandedContainers: list failed, skipping", "error", err)
		return
	}

	var removed int
	for _, ct := range containers {
		if !strandedContainerStates[ct.State] {
			continue // running / paused / restarting — live work, leave it
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
