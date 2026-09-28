package daemon

import (
	"context"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

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
