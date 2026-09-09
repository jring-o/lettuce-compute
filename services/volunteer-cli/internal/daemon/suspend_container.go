package daemon

import (
	"context"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// containerCPUUpdateTimeout bounds the engine's update call: a hung socket
// must not stall the coordinator loop that rebalances shares.
const containerCPUUpdateTimeout = 10 * time.Second

// containerProcessHandle suspends/resumes a container via docker pause/unpause.
type containerProcessHandle struct {
	client      runtime.DockerClient
	containerID string
}

func NewContainerProcessHandle(client runtime.DockerClient, containerID string) ProcessHandle {
	return &containerProcessHandle{client: client, containerID: containerID}
}

func (h *containerProcessHandle) Suspend() error {
	return h.client.ContainerPause(context.Background(), h.containerID)
}

func (h *containerProcessHandle) Resume() error {
	return h.client.ContainerUnpause(context.Background(), h.containerID)
}

// SetCPUShare rewrites the container's CPU quota in place through the
// engine's update call (TB-75).
func (h *containerProcessHandle) SetCPUShare(shareCores float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerCPUUpdateTimeout)
	defer cancel()
	quota, period := runtime.CFSQuota(shareCores)
	return h.client.ContainerUpdateCPU(ctx, h.containerID, quota, period)
}

// PID is 0: a container has no host process the daemon could resume by PID.
// The daemon persists ContainerID instead and adopts the container on the
// next launch (TB-74).
func (h *containerProcessHandle) PID() int {
	return 0
}

// ContainerID names the container this handle pauses and unpauses.
func (h *containerProcessHandle) ContainerID() string {
	return h.containerID
}
