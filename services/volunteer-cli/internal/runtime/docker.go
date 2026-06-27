package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// DockerClient abstracts the Docker Engine API operations needed by ContainerRuntime.
type DockerClient interface {
	Ping(ctx context.Context) error
	ImagePull(ctx context.Context, ref string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageID resolves a reference to its content image ID, or "" if not present.
	ImageID(ctx context.Context, ref string) (string, error)
	// ImageList returns every cached image (used by the stale-image reaper).
	ImageList(ctx context.Context) ([]ImageSummary, error)
	// ImageRemove deletes a cached image by ID. It is non-force, so the backend
	// refuses to delete an image still referenced by any container.
	ImageRemove(ctx context.Context, imageID string) error
	ContainerCreate(ctx context.Context, cfg *ContainerConfig) (string, error)
	ContainerStart(ctx context.Context, containerID string) error
	ContainerWait(ctx context.Context, containerID string) (int64, error)
	ContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error)
	ContainerInspect(ctx context.Context, containerID string) (*ContainerStats, error)
	// ContainerStop requests a graceful stop: the backend sends the entrypoint a
	// termination signal and kills it only if it does not exit within timeout. Used
	// on cancellation so a leaf can flush a final checkpoint before it is killed.
	ContainerStop(ctx context.Context, containerID string, timeout time.Duration) error
	ContainerRemove(ctx context.Context, containerID string) error
	ContainerPause(ctx context.Context, containerID string) error
	ContainerUnpause(ctx context.Context, containerID string) error
	Close() error
}

// ImageSummary is a backend-agnostic view of a cached image, used by the
// stale-image reaper. RepoTags entries look like "repo:tag" ("<none>:<none>"
// when untagged); RepoDigests entries look like "repo@sha256:…". A superseded
// copy left by a re-pushed mutable tag typically has no tag but keeps a repo
// digest — exactly the copy plain `image prune` will not reclaim.
type ImageSummary struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	Size        int64 // bytes
}

// ContainerConfig holds the configuration for creating a Docker container.
type ContainerConfig struct {
	Image       string
	Cmd         []string
	Env         []string          // KEY=value format
	WorkDir     string            // container working directory
	Binds       []string          // host:container volume mounts
	MemoryBytes int64             // memory limit
	CPUQuota    int64             // CPU quota (microseconds per period)
	CPUPeriod   int64             // CPU period (default 100000)
	DiskQuota   int64             // not enforced by Docker directly; use tmpfs size
	NetworkMode string            // "none", "bridge", "host"
	Labels      map[string]string // for identification/cleanup
	Backend     ContainerBackend  // which container backend is in use

	// GPU support
	GPUDeviceIDs   []string        // NVIDIA: GPU device IDs for DeviceRequest
	GPUCount       int             // NVIDIA: number of GPUs (-1 = all, 0 = none)
	DeviceMappings []DeviceMapping // host device passthrough (e.g., AMD GPUs)
}

// DeviceMapping maps a host device into a container.
type DeviceMapping struct {
	PathOnHost      string
	PathInContainer string
	Permissions     string // e.g., "rwm"
}

// ContainerStats holds resource usage from a completed container.
type ContainerStats struct {
	CPUUsageTotal  uint64 // nanoseconds
	CPUUsageUser   uint64
	CPUUsageKernel uint64
	MemoryPeak     int64 // bytes
	NetworkRxBytes int64
	NetworkTxBytes int64
}

// dockerClientWrapper wraps the Docker SDK client to implement DockerClient.
type dockerClientWrapper struct {
	cli    *client.Client
	logger *slog.Logger
}

// NewDockerClientWrapper connects to the Docker daemon via the default socket.
func NewDockerClientWrapper(logger *slog.Logger) (DockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &dockerClientWrapper{cli: cli, logger: logger}, nil
}

// NewDockerClientWrapperWithHost connects to a Docker-compatible API at the given host.
// host is a Docker client host string: "unix:///path/to/socket" or "npipe:////./pipe/name".
func NewDockerClientWrapperWithHost(host string, logger *slog.Logger) (DockerClient, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client for host %s: %w", host, err)
	}
	return &dockerClientWrapper{cli: cli, logger: logger}, nil
}

func (d *dockerClientWrapper) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}
	return nil
}

func (d *dockerClientWrapper) ImagePull(ctx context.Context, ref string) error {
	d.logger.Info("pulling docker image", "image", ref)
	reader, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull %s: %w", ref, err)
	}
	defer reader.Close()
	// The Docker/Podman API reports pull failures (e.g. a manifest-unknown for a
	// superseded/removed digest) INSIDE the progress stream — the call above only
	// surfaces request-level failures. Discarding the stream would treat a failed
	// pull as success, and the failure would resurface much later as a confusing
	// "no such image" at container-create. Scan the stream and surface any error.
	if err := checkPullStream(reader); err != nil {
		return fmt.Errorf("image pull %s: %w", ref, err)
	}
	d.logger.Debug("image pull complete", "image", ref)
	return nil
}

// checkPullStream drains a docker/podman image-pull progress stream and returns
// the first in-stream error it reports (the `error`/`errorDetail` JSON fields),
// or nil once the stream ends cleanly.
func checkPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if decErr := dec.Decode(&msg); decErr != nil {
			if errors.Is(decErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode pull progress stream: %w", decErr)
		}
		if detail := msg.ErrorDetail.Message; detail != "" {
			return errors.New(detail)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

func (d *dockerClientWrapper) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, _, err := d.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("image inspect %s: %w", ref, err)
	}
	return true, nil
}

func (d *dockerClientWrapper) ImageID(ctx context.Context, ref string) (string, error) {
	inspect, _, err := d.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		if client.IsErrNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("image inspect %s: %w", ref, err)
	}
	return inspect.ID, nil
}

func (d *dockerClientWrapper) ImageList(ctx context.Context) ([]ImageSummary, error) {
	summaries, err := d.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("image list: %w", err)
	}
	out := make([]ImageSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, ImageSummary{
			ID:          s.ID,
			RepoTags:    s.RepoTags,
			RepoDigests: s.RepoDigests,
			Size:        s.Size,
		})
	}
	return out, nil
}

func (d *dockerClientWrapper) ImageRemove(ctx context.Context, imageID string) error {
	// Non-force: the backend refuses to delete an image still referenced by any
	// container (running or stopped), so an in-use image is never pulled out from
	// under a workload — the reaper just skips it. PruneChildren reclaims layers
	// orphaned by the removal.
	_, err := d.cli.ImageRemove(ctx, imageID, image.RemoveOptions{Force: false, PruneChildren: true})
	if err != nil {
		return fmt.Errorf("image remove %s: %w", imageID, err)
	}
	return nil
}

// buildGPUDeviceRequests translates a ContainerConfig's GPU settings into Docker
// DeviceRequests. It prefers CDI device names (Driver "cdi", e.g.
// "nvidia.com/gpu=0") when CDI is available — always for Podman (whose
// Docker-compatible API ignores Driver "nvidia", upstream containers/podman#22645),
// and for Docker when an NVIDIA CDI spec is present on the host. CDI works under
// the NVIDIA Container Toolkit's default CDI/auto mode (>=1.17) with no host
// runtime reconfiguration. When no CDI spec exists, it falls back to the legacy
// Driver "nvidia" request, which requires the nvidia runtime in legacy mode.
// Returns nil when the config requests no GPU.
func buildGPUDeviceRequests(cfg *ContainerConfig, cdiAvailable bool) []container.DeviceRequest {
	if len(cfg.GPUDeviceIDs) == 0 && cfg.GPUCount == 0 {
		return nil
	}

	useCDI := cfg.Backend == BackendPodman || cdiAvailable
	if useCDI {
		var cdiDevices []string
		if len(cfg.GPUDeviceIDs) > 0 {
			for _, id := range cfg.GPUDeviceIDs {
				cdiDevices = append(cdiDevices, "nvidia.com/gpu="+id)
			}
		} else {
			cdiDevices = []string{"nvidia.com/gpu=all"}
		}
		return []container.DeviceRequest{{
			Driver:    "cdi",
			DeviceIDs: cdiDevices,
		}}
	}

	// Legacy fallback: standard NVIDIA DeviceRequest.
	dr := container.DeviceRequest{
		Driver:       "nvidia",
		Capabilities: [][]string{{"gpu"}},
	}
	if len(cfg.GPUDeviceIDs) > 0 {
		dr.DeviceIDs = cfg.GPUDeviceIDs
	} else {
		dr.Count = cfg.GPUCount
	}
	return []container.DeviceRequest{dr}
}

func (d *dockerClientWrapper) ContainerCreate(ctx context.Context, cfg *ContainerConfig) (string, error) {
	containerCfg := &container.Config{
		Image:  cfg.Image,
		Cmd:    cfg.Cmd,
		Env:    cfg.Env,
		Labels: cfg.Labels,
	}
	if cfg.WorkDir != "" {
		containerCfg.WorkingDir = cfg.WorkDir
	}

	hostCfg := &container.HostConfig{
		Binds:       cfg.Binds,
		NetworkMode: container.NetworkMode(cfg.NetworkMode),
		Resources: container.Resources{
			Memory:    cfg.MemoryBytes,
			CPUQuota:  cfg.CPUQuota,
			CPUPeriod: cfg.CPUPeriod,
		},
	}

	// NVIDIA GPU passthrough.
	if reqs := buildGPUDeviceRequests(cfg, nvidiaCDIAvailable()); reqs != nil {
		hostCfg.DeviceRequests = reqs
	}

	// Device mappings (AMD GPUs, etc).
	for _, dm := range cfg.DeviceMappings {
		hostCfg.Devices = append(hostCfg.Devices, container.DeviceMapping{
			PathOnHost:        dm.PathOnHost,
			PathInContainer:   dm.PathInContainer,
			CgroupPermissions: dm.Permissions,
		})
	}

	resp, err := d.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}
	return resp.ID, nil
}

func (d *dockerClientWrapper) ContainerStart(ctx context.Context, containerID string) error {
	if err := d.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	return nil
}

func (d *dockerClientWrapper) ContainerWait(ctx context.Context, containerID string) (int64, error) {
	statusCh, errCh := d.cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return -1, fmt.Errorf("container wait: %w", err)
		}
		// errCh closed without error; wait for status.
		status := <-statusCh
		if status.Error != nil {
			return status.StatusCode, fmt.Errorf("container exited with error: %s", status.Error.Message)
		}
		return status.StatusCode, nil
	case status := <-statusCh:
		if status.Error != nil {
			return status.StatusCode, fmt.Errorf("container exited with error: %s", status.Error.Message)
		}
		return status.StatusCode, nil
	}
}

func (d *dockerClientWrapper) ContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	reader, err := d.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("container logs: %w", err)
	}
	return reader, nil
}

func (d *dockerClientWrapper) ContainerInspect(ctx context.Context, containerID string) (*ContainerStats, error) {
	inspect, err := d.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("container inspect: %w", err)
	}

	stats := &ContainerStats{}
	if inspect.State != nil && inspect.HostConfig != nil {
		// Peak memory from HostConfig limit as a fallback; real stats come from
		// the stats API but inspect gives us what we need for basic metrics.
		stats.MemoryPeak = inspect.HostConfig.Memory
	}
	return stats, nil
}

func (d *dockerClientWrapper) ContainerStop(ctx context.Context, containerID string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if err := d.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &secs}); err != nil {
		return fmt.Errorf("container stop: %w", err)
	}
	return nil
}

func (d *dockerClientWrapper) ContainerRemove(ctx context.Context, containerID string) error {
	if err := d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("container remove: %w", err)
	}
	return nil
}

func (d *dockerClientWrapper) ContainerPause(ctx context.Context, containerID string) error {
	return d.cli.ContainerPause(ctx, containerID)
}

func (d *dockerClientWrapper) ContainerUnpause(ctx context.Context, containerID string) error {
	return d.cli.ContainerUnpause(ctx, containerID)
}

func (d *dockerClientWrapper) Close() error {
	return d.cli.Close()
}

// IsDockerAvailable returns true if the Docker daemon is running and accessible.
func IsDockerAvailable() bool {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer cli.Close()
	_, err = cli.Ping(context.Background())
	return err == nil
}
