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

func (h *containerProcessHandle) PID() int {
	return 0 // containers can't be resumed as orphans
}
