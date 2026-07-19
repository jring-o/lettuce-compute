package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lettuce-compute/infrastructure/netguard"
)

// ContainerRuntime executes work units inside Docker containers.
type ContainerRuntime struct {
	dataDir       string
	logger        *slog.Logger
	dockerClient  DockerClient
	backend       ContainerBackend // which container backend (podman, docker)
	maxCPUCores   int              // from config; 0 means no CPU limit
	gpus          []*GpuDetectionResult
	maxGPUVRAMPct int
	memCeilingMB  int // volunteer's configured memory budget (0 = unset); clamps per-unit BookedMemMB
	diskCeilingMB int // volunteer's configured disk budget in MB (0 = unset); clamps per-unit BookedDiskMB
	maxPids       int // fork-bomb PID cap from config (<=0 = built-in default)
	capAdd        []string
	gpuRelaxUser  bool         // BG-13 GPU carve-out: relax non-root/caps for GPU leaves
	httpClient    *http.Client // for viz bundle downloads

	// wantedImages, when set, returns every image ref the volunteer currently
	// wants cached (all enabled leaves across all heads). The stale-image reaper
	// keeps these; see SetWantedImages / reapStaleImages.
	wantedImages func() []string
}

// NewContainerRuntime creates a ContainerRuntime, connecting to the Docker daemon.
func NewContainerRuntime(dataDir string, logger *slog.Logger) (*ContainerRuntime, error) {
	dc, err := NewDockerClientWrapper(logger)
	if err != nil {
		return nil, fmt.Errorf("connect to docker: %w", err)
	}
	return &ContainerRuntime{
		dataDir:      dataDir,
		logger:       logger,
		dockerClient: dc,
		backend:      BackendDocker,
		httpClient:   NewGuardedHTTPClient(),
	}, nil
}

// NewContainerRuntimeWithClient creates a ContainerRuntime with an injected DockerClient (for testing).
func NewContainerRuntimeWithClient(dataDir string, logger *slog.Logger, dc DockerClient) *ContainerRuntime {
	return &ContainerRuntime{
		dataDir:      dataDir,
		logger:       logger,
		dockerClient: dc,
		httpClient:   NewGuardedHTTPClient(),
	}
}

// NewContainerRuntimeForBackend creates a ContainerRuntime using the specified backend.
// For Podman: connects Docker SDK to Podman's compatible socket.
// For Docker: uses default Docker connection.
// For None: returns an error.
func NewContainerRuntimeForBackend(dataDir string, logger *slog.Logger, backend BackendInfo) (*ContainerRuntime, error) {
	var dc DockerClient
	var err error

	switch backend.Backend {
	case BackendPodman:
		host := podmanHostString(backend.SocketPath)
		dc, err = NewDockerClientWrapperWithHost(host, logger)
		if err != nil {
			return nil, fmt.Errorf("connect to podman at %s: %w", backend.SocketPath, err)
		}
		logger.Info("using Podman container backend", "socket", backend.SocketPath, "version", backend.Version)
	case BackendDocker:
		dc, err = NewDockerClientWrapper(logger)
		if err != nil {
			return nil, fmt.Errorf("connect to docker: %w", err)
		}
		logger.Info("using Docker container backend")
	default:
		return nil, fmt.Errorf("no container runtime available")
	}

	return &ContainerRuntime{
		dataDir:      dataDir,
		logger:       logger,
		dockerClient: dc,
		backend:      backend.Backend,
		httpClient:   NewGuardedHTTPClient(),
	}, nil
}

// SetBackend sets the container backend (for testing).
func (c *ContainerRuntime) SetBackend(b ContainerBackend) {
	c.backend = b
}

// SetMaxCPUCores sets the CPU core limit from volunteer config.
func (c *ContainerRuntime) SetMaxCPUCores(cores int) {
	c.maxCPUCores = cores
}

// SetGPUs sets the detected GPUs available for container execution.
func (c *ContainerRuntime) SetGPUs(gpus []*GpuDetectionResult) {
	c.gpus = gpus
}

// SetMaxGPUVRAMPct sets the maximum GPU VRAM percentage from config.
func (c *ContainerRuntime) SetMaxGPUVRAMPct(pct int) {
	c.maxGPUVRAMPct = pct
}

// SetMemoryCeilingMB sets the volunteer's configured memory budget
// (config.ResourceLimits.MaxMemoryMB). Per-unit enforcement clamps the declared
// memory to this ceiling via BookedMemMB so enforcement matches admission (BG-16).
func (c *ContainerRuntime) SetMemoryCeilingMB(mb int) { c.memCeilingMB = mb }

// SetDiskCeilingMB sets the volunteer's configured disk budget in MB
// (config.ResourceLimits.MaxDiskGB * 1024). The /work size watchdog and any
// StorageOpt quota are driven by BookedDiskMB clamped to this, never by the
// attacker-declared MaxDiskMB (BG-16c).
func (c *ContainerRuntime) SetDiskCeilingMB(mb int) { c.diskCeilingMB = mb }

// SetHardeningConfig sets the BG-13 container-hardening knobs from config: the PID
// cap, the explicit capability re-adds, and whether GPU leaves may relax the
// non-root/minimal-capability posture that CPU leaves always get.
func (c *ContainerRuntime) SetHardeningConfig(maxPids int, capAdd []string, gpuRelaxUser bool) {
	c.maxPids = maxPids
	c.capAdd = capAdd
	c.gpuRelaxUser = gpuRelaxUser
}

// defaultContainerPidsLimit is the fork-bomb PID cap used when max_pids is unset.
// Generous for compute leaves; blunts a fork bomb.
const defaultContainerPidsLimit int64 = 512

// nobodyUser is the non-root uid:gid CPU leaves run as (nobody:nogroup).
const nobodyUser = "65534:65534"

// applyHardening sets the BG-13 security posture on the container config. CPU leaves
// run fully locked down: no-new-privileges, all capabilities dropped, a read-only
// rootfs with a small writable tmpfs /tmp, a PID cap, and a non-root user. GPU
// leaves keep the structural protections (no-new-privileges, read-only rootfs, PID
// cap) but — when container_gpu_relax_user is set — leave the user and capabilities
// at the backend default, because device passthrough (/dev/nvidia*, /dev/kfd,
// /dev/dri/renderD*) commonly needs it. The relaxation is explicit and logged.
func (c *ContainerRuntime) applyHardening(cfg *ContainerConfig, wu *WorkUnit) {
	// BG-13c: the explicit ":true" form. Docker honors the bare "no-new-privileges",
	// but some Podman-compat backends are spelling-sensitive and only recognize the
	// key:value form; ":true" is accepted by both.
	cfg.SecurityOpt = []string{"no-new-privileges:true"}
	cfg.ReadonlyRootfs = true
	// A small writable tmpfs at /tmp lets images that only need scratch space run
	// under a read-only rootfs; /work and /work/checkpoint stay writable via binds.
	cfg.TmpfsMounts = map[string]string{"/tmp": "rw,noexec,nosuid,size=64m"}
	pids := int64(c.maxPids)
	if pids <= 0 {
		pids = defaultContainerPidsLimit
	}
	cfg.PidsLimit = pids

	if len(c.capAdd) > 0 {
		cfg.CapAdd = append([]string(nil), c.capAdd...)
	}

	if wu.ExecutionSpec.GPURequired && c.gpuRelaxUser {
		// GPU carve-out: keep the structural protections but leave user/caps at the
		// backend default so the device nodes stay usable.
		c.logger.Info("container hardening: GPU leaf runs with relaxed user/capabilities (container_gpu_relax_user)",
			"work_unit_id", wu.ID)
		return
	}

	// CPU leaves (and GPU leaves when the carve-out is disabled): drop every
	// capability and run as a non-root user.
	cfg.CapDrop = []string{"ALL"}
	cfg.User = nobodyUser
}

// cleanContainerPath normalizes a Linux container path (forward-slash, absolute)
// for set membership — trimming whitespace and any trailing slash. It deliberately
// does NOT use path/filepath, whose Windows behavior would corrupt a container path
// on a Windows daemon host.
func cleanContainerPath(p string) string {
	p = strings.TrimSpace(p)
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// ourMountTargets are the container paths the runtime always mounts itself (the
// /work binds and the /tmp tmpfs). A declared VOLUME at one of these is already
// backed by our mount, so it is left alone by neutralizeImageVolumes.
var ourMountTargets = map[string]bool{
	"/work/input":      true,
	"/work/output":     true,
	"/work/checkpoint": true,
	"/tmp":             true,
}

// neutralizeImageVolumes replaces every image-declared VOLUME with a bounded tmpfs
// mount (BG-13b). An image VOLUME is exempt from ReadonlyRootfs, and left alone
// Docker/OCI backs it with a writable ANONYMOUS host volume that (a) escapes the
// /work disk watchdog with unbounded writes and (b) leaks onto host storage. A
// size-bounded tmpfs over each declared path makes those writes RAM-backed and
// bounded by the container's memory cgroup (BookedMemMB) — never host disk — and
// stops the engine from creating the anonymous volume at all. Paths we already
// mount (the /work binds, /tmp) are skipped. Best-effort: an image we cannot
// inspect logs a warning and relies on RemoveVolumes at cleanup for the leak half.
func (c *ContainerRuntime) neutralizeImageVolumes(ctx context.Context, cfg *ContainerConfig, bookedMemMB int) {
	vols, err := c.dockerClient.ImageDeclaredVolumes(ctx, cfg.Image)
	if err != nil {
		c.logger.Warn("container: image inspect for declared VOLUMEs failed; disk bounding skipped (RemoveVolumes still reclaims the leak)",
			"image", cfg.Image, "error", err)
		return
	}
	if len(vols) == 0 {
		return
	}
	if cfg.TmpfsMounts == nil {
		cfg.TmpfsMounts = map[string]string{}
	}
	sizeMB := bookedMemMB
	if sizeMB <= 0 {
		sizeMB = 512
	}
	// nosuid,nodev harden the mount; no noexec, since a scratch VOLUME may legitimately
	// need to run helper binaries. The memory cgroup caps the SUM across all tmpfs
	// mounts and the process, so size= is an upper label, not the true bound.
	opts := fmt.Sprintf("rw,nosuid,nodev,size=%dm", sizeMB)
	for _, v := range vols {
		clean := cleanContainerPath(v)
		if clean == "" || clean == "/" || ourMountTargets[clean] {
			continue
		}
		if _, already := cfg.TmpfsMounts[clean]; already {
			continue
		}
		cfg.TmpfsMounts[clean] = opts
		c.logger.Info("container: neutralized image-declared VOLUME with a bounded tmpfs (BG-13b)",
			"volume", clean, "size_mb", sizeMB, "image", cfg.Image)
	}
}

// registryHostFromImage extracts the registry authority host from an OCI image
// reference, or "" when the reference uses the default public registry (no registry
// component). Mirrors the head-side validateImageRegistryHost split: the registry is
// the first '/'-separated component only when it looks like an authority (contains
// '.'/':' or is "localhost"); a plain first component is a path on the default
// registry. Any port or IPv6 brackets are stripped.
func registryHostFromImage(image string) string {
	slash := strings.IndexByte(image, '/')
	if slash < 0 {
		return ""
	}
	first := image[:slash]
	if first != "localhost" && !strings.ContainsAny(first, ".:") {
		return ""
	}
	host := first
	if h, _, err := net.SplitHostPort(first); err == nil {
		host = h
	} else if strings.HasPrefix(first, "[") && strings.HasSuffix(first, "]") {
		host = first[1 : len(first)-1]
	}
	return host
}

// screenImageRegistry refuses an image pull whose registry authority is an internal
// address (BG-14d). An IP-literal registry is screened directly against netguard; a
// registry hostname is resolved and every returned IP screened. A hostname we cannot
// resolve is allowed through (the engine's own pull will fail if it is truly
// unreachable) — the concrete literal-metadata-IP attack is closed deterministically.
// localhost is refused. Best-effort DNS still leaves a TOCTOU/rebinding gap versus the
// engine's own resolution; the head-side registry screen is the companion layer.
func screenImageRegistry(ctx context.Context, image string) error {
	host := registryHostFromImage(image)
	if host == "" {
		return nil
	}
	if host == "localhost" {
		return fmt.Errorf("refusing image pull: registry host %q is internal", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if reason := netguard.DisallowedIPReason(ip); reason != "" {
			return fmt.Errorf("refusing image pull: registry %s is an internal address (%s)", ip, reason)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil // transient/unresolvable: let the engine attempt and fail
	}
	for _, ipa := range ips {
		if reason := netguard.DisallowedIPReason(ipa.IP); reason != "" {
			return fmt.Errorf("refusing image pull: registry %q resolves to an internal address %s (%s)", host, ipa.IP, reason)
		}
	}
	return nil
}

// Name returns "container".
func (c *ContainerRuntime) Name() string { return "container" }

// Client returns the underlying DockerClient for suspend/resume operations.
func (c *ContainerRuntime) Client() DockerClient { return c.dockerClient }

// CanHandle returns true if the spec has an OCI image reference.
func (c *ContainerRuntime) CanHandle(spec *ExecutionSpec) bool {
	return spec != nil && spec.Image != ""
}

// Prepare verifies Docker availability, creates work directories, writes input files,
// and pulls the container image if not locally cached.
func (c *ContainerRuntime) Prepare(ctx context.Context, wu *WorkUnit) (*PrepareResult, error) {
	// SECURITY (H2): defense-in-depth — wu.ID is the trailing component of workDir
	// below, and the resulting input/output dirs are bind-mounted into the
	// container. Reject non-UUID IDs before building any path so a malicious head
	// can't escape c.dataDir or mount an arbitrary host directory.
	if err := ValidateWorkUnitID(wu.ID); err != nil {
		c.logger.Warn("container.Prepare: rejecting work unit with invalid ID", "work_unit_id", wu.ID, "error", err)
		return nil, err
	}

	// Verify Docker daemon is accessible.
	if err := c.dockerClient.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker is not available: %w", err)
	}

	// Create work directory structure. The checkpoint dir is bind-mounted rw into the
	// container at /work/checkpoint and archived/restored by the checkpoint manager.
	workDir := filepath.Join(c.dataDir, "container-work", wu.ID)
	inputDir := filepath.Join(workDir, "input")
	outputDir := filepath.Join(workDir, "output")
	checkpointDir := filepath.Join(workDir, "checkpoint")
	for _, dir := range []string{inputDir, outputDir, checkpointDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create work dir: %w", err)
		}
	}

	// Write input data (inline or external download).
	if len(wu.InputData) > 0 {
		if err := os.WriteFile(filepath.Join(inputDir, "input.dat"), wu.InputData, 0o644); err != nil {
			return nil, fmt.Errorf("write input data: %w", err)
		}
	} else if wu.InputDataURL != "" {
		// SECURITY (BG-14, design finding #3): the container input path must use the
		// runtime's netguard-guarded client — the default-image SSRF surface — not an
		// unscreened default client. Passing c.httpClient routes it through the same
		// dial screen as every other fetch (and is the test seam).
		data, _, err := DownloadExternalDataWithClient(ctx, artifactClientForUnit(c.httpClient, wu, c.logger), wu.InputDataURL, DefaultMaxDownloadBytes)
		if err != nil {
			return nil, fmt.Errorf("download input data: %w", err)
		}
		if err := os.WriteFile(filepath.Join(inputDir, "input.dat"), data, 0o644); err != nil {
			return nil, fmt.Errorf("write downloaded input data: %w", err)
		}
	}

	// Write parameters JSON.
	if wu.ParametersJSON != "" {
		if err := os.WriteFile(filepath.Join(inputDir, "parameters.json"), []byte(wu.ParametersJSON), 0o644); err != nil {
			return nil, fmt.Errorf("write parameters: %w", err)
		}
	}

	// Pull policy (TODO #38): make sure we run the artifact the head actually points
	// at, not a staler cached copy under the same tag.
	//
	//   * Digest-pinned ref (…@sha256:…): content-addressed and immutable, so a copy
	//     already present is provably the right bytes — pull only on a cache miss. A
	//     new version means a NEW digest -> new ref -> miss -> pull. This is the
	//     recommended path (the head's publish lint pushes container leaves to digests).
	//   * Tag ref (which MAY be mutable, e.g. a re-pushed :latest — the exact footgun
	//     behind #38): attempt a pull so a re-pushed tag is refreshed. If the pull
	//     fails but we already hold the image, fall back to the cached copy so a
	//     registry outage can't break an otherwise-runnable unit.
	image := wu.ExecutionSpec.Image
	exists, err := c.dockerClient.ImageExists(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("check image: %w", err)
	}
	// SECURITY (BG-14d): a pull egresses through the Docker/Podman engine, OUTSIDE the
	// daemon's netguard dial screen, so an image reference naming an internal registry
	// (e.g. 169.254.169.254/repo) would let the engine reach cloud metadata / loopback.
	// Screen the registry host before any pull. A cached image that needs no pull
	// performs no egress and is not re-screened. This is the load-bearing daemon layer
	// (a malicious head can hand any image directly, bypassing the head-side screen).
	needPull := !(strings.Contains(image, "@sha256:") && exists)
	if needPull {
		if err := screenImageRegistry(ctx, image); err != nil {
			return nil, err
		}
	}
	pulled := false
	if strings.Contains(image, "@sha256:") {
		if !exists {
			if err := c.dockerClient.ImagePull(ctx, image); err != nil {
				return nil, interpretPullError(c.backend, image, err)
			}
			pulled = true
		}
	} else {
		if err := c.dockerClient.ImagePull(ctx, image); err != nil {
			if !exists {
				return nil, interpretPullError(c.backend, image, err)
			}
			c.logger.Warn("container.Prepare: image pull failed; using cached image",
				"image", image, "error", err)
		} else {
			pulled = true
		}
	}

	// After a fresh pull, reap superseded cached copies of the same repository so
	// a re-pushed mutable tag (or a new digest) does not leave orphaned images
	// consuming the volunteer's disk allowance until the disk gate trips (#60 —
	// the disk-reclamation companion to #38's artifact-freshness work).
	// Best-effort and runtime-only; never blocks compute.
	if pulled {
		c.reapStaleImages(ctx, image)
	}

	result := &PrepareResult{WorkDir: workDir}

	// Download and extract viz bundle if present. Viz is a dashboard-only concern
	// (the container never reads it); a bad/missing bundle must NEVER block compute,
	// so we warn and continue without it. See TODO #39.
	vizPath, err := PrepareVizBundle(ctx, c.dataDir, workDir, &wu.ExecutionSpec, artifactClientForUnit(c.httpClient, wu, c.logger), c.logger)
	if err != nil {
		c.logger.Warn("container.Prepare: viz bundle prep failed; continuing without viz (compute unaffected)",
			"work_unit_id", wu.ID, "error", err)
		vizPath = ""
	}
	result.VizBundlePath = vizPath

	return result, nil
}

// diskExhaustionSignatures are substrings (lowercased) that indicate an image
// pull failed because the backend ran out of disk space.
var diskExhaustionSignatures = []string{
	"no space left on device",
	"enospc",
	"not enough space",
	"insufficient disk",
	"out of disk",
	"write: no space",
}

// interpretPullError turns a raw image-pull failure into an actionable error.
// Disk-exhaustion failures are the common mode for large-image leaves (some
// ship tens of GB), so when one is detected we explain the likely cause and
// where to free space — the backend's image storage, which on Windows/macOS is
// a VM disk (Podman machine) or the Docker Desktop WSL2/host volume, not the
// directory the leaf's work lives in. Other failures are wrapped unchanged.
func interpretPullError(backend ContainerBackend, image string, err error) error {
	if err == nil {
		return nil
	}
	if !isDiskExhaustionError(err) {
		return fmt.Errorf("pull image %q: %w", image, err)
	}

	location := "the container backend's image storage"
	switch backend {
	case BackendPodman:
		location = "the Podman machine's disk (on Windows/macOS this is a VM; on Linux it is the host filesystem)"
	case BackendDocker:
		location = "Docker's image store (on Linux this is /var/lib/docker, or the containerd root such as /var/lib/containerd when Docker uses the containerd snapshotter; on Windows/macOS it is the Docker Desktop WSL2/VM disk on the system drive)"
	}

	return fmt.Errorf("pull image %q failed: out of disk space on %s. "+
		"Large leaves can need 100+ GB free to pull and unpack their image — "+
		"free up space (or move the backend's storage to a larger drive) and retry: %w",
		image, location, err)
}

// isDiskExhaustionError reports whether err looks like a disk-space failure.
func isDiskExhaustionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range diskExhaustionSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// Execute runs the work unit in a Docker container and returns results.
func (c *ContainerRuntime) Execute(ctx context.Context, wu *WorkUnit, prep *PrepareResult) (*ExecutionResult, error) {
	c.logger.Info("executing work unit", "work_unit_id", wu.ID, "leaf_id", wu.LeafID, "runtime", wu.Runtime)

	inputDir := filepath.Join(prep.WorkDir, "input")
	outputDir := filepath.Join(prep.WorkDir, "output")
	// Checkpoint dir, bind-mounted rw at /work/checkpoint. Recreated here (not just in
	// Prepare) so it exists for a resumed unit, where Execute runs without Prepare but
	// the work dir was preserved.
	checkpointDir := filepath.Join(prep.WorkDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}

	// Build environment variables.
	env := make([]string, 0, len(wu.EnvVars)+8)
	for k, v := range wu.EnvVars {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"LETTUCE_WORK_UNIT_ID="+wu.ID,
		"LETTUCE_INPUT_DIR=/work/input",
		"LETTUCE_OUTPUT_DIR=/work/output",
		"LETTUCE_PARAMETERS_FILE=/work/input/parameters.json",
		"LETTUCE_PROGRESS_FILE=/work/output/progress.txt",
		"LETTUCE_CHECKPOINT_DIR=/work/checkpoint",
		"LETTUCE_CHECKPOINT_FILE=/work/checkpoint/checkpoint.dat",
	)

	// GPU passthrough.
	var selectedGPU *GpuDetectionResult
	var gpuDeviceIdx int
	if wu.ExecutionSpec.GPURequired {
		selectedGPU, gpuDeviceIdx = c.selectGPU(wu.ExecutionSpec.GPUType)
		if selectedGPU == nil {
			return nil, fmt.Errorf("work unit requires GPU but no matching GPU found")
		}

		// Enforce VRAM limit as safety net (assignment engine already filters,
		// but we enforce at runtime per spec section 13.1).
		if c.maxGPUVRAMPct > 0 && selectedGPU.VRAMMB > 0 {
			allowedVRAMMB := int64(c.maxGPUVRAMPct) * int64(selectedGPU.VRAMMB) / 100
			requiredVRAMMB := int64(wu.ExecutionSpec.MinVRAMMB)
			if requiredVRAMMB > allowedVRAMMB {
				c.logger.Warn("work unit VRAM requirement exceeds volunteer limit",
					"required_mb", requiredVRAMMB,
					"allowed_mb", allowedVRAMMB,
					"gpu", selectedGPU.Model,
					"max_vram_pct", c.maxGPUVRAMPct,
					"work_unit_id", wu.ID,
				)
			}
			env = append(env,
				fmt.Sprintf("LETTUCE_GPU_VRAM_LIMIT_MB=%d", allowedVRAMMB),
			)
		}

		env = append(env,
			"LETTUCE_GPU_ENABLED=true",
			"LETTUCE_GPU_VENDOR="+selectedGPU.Vendor,
		)
	}

	// Compute resource limits. SECURITY (BG-16): the memory ceiling is BookedMemMB, so
	// a declared 0 is bounded to the per-task default (never Docker's unlimited-0) and
	// a huge declaration is clamped to the volunteer's configured budget. The container
	// can therefore never exceed what admission booked for it.
	bookedMemMB := BookedMemMB(int(wu.ExecutionSpec.MaxMemoryMB), c.memCeilingMB)
	memoryBytes := int64(bookedMemMB) * 1024 * 1024

	var cpuQuota, cpuPeriod int64
	if c.maxCPUCores > 0 {
		cpuPeriod = 100000
		cpuQuota = int64(c.maxCPUCores) * cpuPeriod
	}

	// Network mode.
	networkMode := "none"
	if wu.ExecutionSpec.NetworkAccess {
		networkMode = "bridge"
	}

	cfg := &ContainerConfig{
		Image:   wu.ExecutionSpec.Image,
		Env:     env,
		WorkDir: "/work",
		Backend: c.backend,
		Binds: []string{
			inputDir + ":/work/input:ro",
			outputDir + ":/work/output",
			checkpointDir + ":/work/checkpoint",
		},
		MemoryBytes: memoryBytes,
		CPUQuota:    cpuQuota,
		CPUPeriod:   cpuPeriod,
		NetworkMode: networkMode,
		Labels: map[string]string{
			WorkUnitIDLabel:   wu.ID,
			"lettuce.leaf-id": wu.LeafID,
		},
	}

	// SECURITY (BG-13): apply the hardened container posture (no-new-privileges,
	// dropped capabilities, read-only rootfs + tmpfs /tmp, PID cap, non-root user),
	// with the GPU carve-out for leaves that need device passthrough.
	c.applyHardening(cfg, wu)

	// SECURITY (BG-13b): replace any image-declared VOLUME with a bounded tmpfs so it
	// cannot open a writable, host-backed path that escapes ReadonlyRootfs and the
	// /work disk watchdog. Uses the same bookedMemMB ceiling as the memory limit.
	c.neutralizeImageVolumes(ctx, cfg, bookedMemMB)

	// Configure GPU device passthrough on the container.
	if selectedGPU != nil {
		switch selectedGPU.Vendor {
		case "nvidia":
			cfg.GPUDeviceIDs = []string{strconv.Itoa(gpuDeviceIdx)}
			// Restrict visible GPUs via NVIDIA_VISIBLE_DEVICES for VRAM isolation.
			cfg.Env = append(cfg.Env,
				"NVIDIA_VISIBLE_DEVICES="+strconv.Itoa(gpuDeviceIdx),
			)
		case "amd":
			renderDev := fmt.Sprintf("/dev/dri/renderD%d", 128+gpuDeviceIdx)
			cfg.DeviceMappings = []DeviceMapping{
				{PathOnHost: renderDev, PathInContainer: renderDev, Permissions: "rwm"},
				{PathOnHost: "/dev/kfd", PathInContainer: "/dev/kfd", Permissions: "rwm"},
			}
		}
	}

	// Create container.
	containerID, err := c.dockerClient.ContainerCreate(ctx, cfg)
	if err != nil {
		c.logger.Error("container create failed", "work_unit_id", wu.ID, "image", cfg.Image, "backend", c.backend, "error", err)
		return nil, fmt.Errorf("create container: %w", err)
	}

	// Best-effort removal when done.
	defer func() {
		rmErr := c.dockerClient.ContainerRemove(context.Background(), containerID)
		if rmErr != nil {
			c.logger.Warn("failed to remove container", "container", containerID, "error", rmErr)
		}
	}()

	// Start container.
	if err := c.dockerClient.ContainerStart(ctx, containerID); err != nil {
		c.logger.Error("container start failed", "work_unit_id", wu.ID, "container", containerID, "image", cfg.Image, "backend", c.backend, "error", err)
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Notify caller of container ID for suspend/resume support.
	if prep.ContainerIDCallback != nil {
		prep.ContainerIDCallback(containerID)
	}

	// Start GPU metrics collection.
	var gpuExecMetrics *GPUExecutionMetrics
	var gpuMetricsCancel context.CancelFunc
	var gpuMetricsDone chan struct{}
	if selectedGPU != nil {
		gpuCollector := NewGPUMetricsCollector(selectedGPU.Vendor, gpuDeviceIdx, c.logger)
		var gpuMetricsCtx context.Context
		gpuMetricsCtx, gpuMetricsCancel = context.WithCancel(ctx)
		gpuMetricsDone = make(chan struct{})
		go func() {
			gpuExecMetrics = gpuCollector.CollectDuringExecution(gpuMetricsCtx, 5*time.Second)
			if gpuExecMetrics.GPUModel == "" {
				gpuExecMetrics.GPUModel = selectedGPU.Model
			}
			close(gpuMetricsDone)
		}()
	}

	startTime := time.Now()

	// Apply deadline.
	waitCtx := ctx
	var cancel context.CancelFunc
	if wu.DeadlineSeconds > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(wu.DeadlineSeconds)*time.Second)
		defer cancel()
	}

	// BG-16c: bound /work growth at bookedDiskMB — the volunteer's configured disk
	// ceiling — never at the attacker-declared MaxDiskMB (which the head accepts with
	// no upper clamp). On overshoot, stop the container and fail the unit at the
	// config ceiling. The watchdog polls, so a unit can overshoot by up to one
	// interval's writes before termination — bounded, not zero.
	bookedDiskMB := BookedDiskMB(int(wu.ExecutionSpec.MaxDiskMB), c.diskCeilingMB)
	var diskExceeded atomic.Bool
	stopWatchdog := startDiskWatchdog(waitCtx, int64(bookedDiskMB)*1024*1024,
		[]string{outputDir, checkpointDir}, func(size int64) {
			diskExceeded.Store(true)
			c.logger.Warn("disk watchdog: /work exceeded booked disk budget; stopping container",
				"work_unit_id", wu.ID, "size_bytes", size, "booked_disk_mb", bookedDiskMB)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), gracefulShutdownGrace+5*time.Second)
			defer stopCancel()
			if stopErr := c.dockerClient.ContainerStop(stopCtx, containerID, gracefulShutdownGrace); stopErr != nil {
				c.logger.Warn("disk watchdog: container stop failed", "work_unit_id", wu.ID, "container", containerID, "error", stopErr)
			}
		})
	defer stopWatchdog()

	// Wait for container to exit.
	exitCode, err := c.dockerClient.ContainerWait(waitCtx, containerID)
	wallClock := time.Since(startTime)

	// Stop GPU metrics collection.
	if gpuMetricsCancel != nil {
		gpuMetricsCancel()
		<-gpuMetricsDone
	}

	// If the disk watchdog stopped the container, the unit failed its disk budget.
	if diskExceeded.Load() {
		return nil, fmt.Errorf("work unit terminated: exceeded disk budget of %d MB", bookedDiskMB)
	}

	if err != nil {
		if waitCtx.Err() != nil {
			// Cancelled (graceful stop) or deadline: stop the container with a grace
			// period so its entrypoint receives a termination signal and can flush a
			// final checkpoint before being killed. Detached context — waitCtx is done.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), gracefulShutdownGrace+5*time.Second)
			if stopErr := c.dockerClient.ContainerStop(stopCtx, containerID, gracefulShutdownGrace); stopErr != nil {
				c.logger.Warn("graceful container stop failed", "work_unit_id", wu.ID, "container", containerID, "error", stopErr)
			}
			stopCancel()
			if ctx.Err() != nil {
				return nil, fmt.Errorf("execution cancelled: %w", ctx.Err())
			}
			return nil, fmt.Errorf("execution deadline exceeded: %w", waitCtx.Err())
		}
		return nil, fmt.Errorf("container wait: %w", err)
	}

	// Capture logs to execution.log (capped at 10 MB).
	c.captureContainerLogs(ctx, containerID, prep.WorkDir)

	// Inspect container for resource stats.
	stats, inspectErr := c.dockerClient.ContainerInspect(ctx, containerID)
	if inspectErr != nil {
		c.logger.Warn("failed to inspect container", "error", inspectErr)
		stats = &ContainerStats{}
	}

	// Read output.
	outputData, err := c.readOutput(outputDir)
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}

	// Build metrics.
	metrics := c.buildMetrics(stats, wallClock)

	// Merge GPU metrics.
	if gpuExecMetrics != nil {
		metrics.GPUSeconds = gpuExecMetrics.GPUSeconds
		metrics.GPUModel = gpuExecMetrics.GPUModel
		metrics.GPUVRAMUsedMB = int32(gpuExecMetrics.PeakVRAMMB)
	}

	c.logger.Info("execution finished", "work_unit_id", wu.ID, "exit_code", int(exitCode), "wall_clock_s", wallClock.Seconds())

	return &ExecutionResult{
		OutputData:     outputData,
		OutputChecksum: checksumSHA256(outputData),
		ExitCode:       int(exitCode),
		Metrics:        metrics,
	}, nil
}

// Cleanup removes the work directory. Docker images are preserved for caching.
func (c *ContainerRuntime) Cleanup(prep *PrepareResult) error {
	if prep == nil || prep.WorkDir == "" {
		return nil
	}
	return os.RemoveAll(prep.WorkDir)
}

// captureContainerLogs writes container stdout/stderr to execution.log capped at 10 MB.
func (c *ContainerRuntime) captureContainerLogs(ctx context.Context, containerID, workDir string) {
	logReader, err := c.dockerClient.ContainerLogs(ctx, containerID)
	if err != nil {
		c.logger.Warn("failed to get container logs", "error", err)
		return
	}
	defer logReader.Close()

	logPath := filepath.Join(workDir, "execution.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		c.logger.Warn("failed to create execution log", "error", err)
		return
	}
	defer logFile.Close()

	const maxLogSize = 10 * 1024 * 1024
	_, _ = io.Copy(logFile, io.LimitReader(logReader, maxLogSize))
}

// readOutput reads output.dat from the output directory. If output.dat doesn't
// exist, it reads the first regular file in the output directory.
//
// SECURITY (BG-15): reads go through readRegularNoFollow, which refuses any entry
// that is not a regular file — above all a symlink. A container that writes
// output.dat as a symlink to the volunteer's signing key (the /work/output bind is
// host-backed) therefore exfiltrates nothing; the link is skipped, on the primary
// read AND every fallback entry.
func (c *ContainerRuntime) readOutput(outputDir string) ([]byte, error) {
	outputPath := filepath.Join(outputDir, "output.dat")
	if data, err := readRegularNoFollow(outputPath); err == nil {
		return data, nil
	}

	// Fallback: read the first regular file in the output directory (never
	// descending into subdirectories, and never following a symlinked entry).
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil, nil // empty output
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := readRegularNoFollow(filepath.Join(outputDir, entry.Name()))
		if err != nil {
			continue
		}
		return data, nil
	}

	return nil, nil
}

// selectGPU finds a GPU matching the requested type from detected GPUs.
// Returns the GPU and its index, or nil/-1 if no match found.
func (c *ContainerRuntime) selectGPU(gpuType string) (*GpuDetectionResult, int) {
	gpuType = strings.ToLower(gpuType)
	for i, gpu := range c.gpus {
		switch gpuType {
		case "nvidia":
			if gpu.Vendor == "nvidia" {
				return gpu, i
			}
		case "amd":
			if gpu.Vendor == "amd" {
				return gpu, i
			}
		default: // "any" or empty
			return gpu, i
		}
	}
	return nil, -1
}

// buildMetrics maps Docker container stats to ExecutionMetrics.
func (c *ContainerRuntime) buildMetrics(stats *ContainerStats, wallClock time.Duration) ExecutionMetrics {
	metrics := ExecutionMetrics{
		WallClockSeconds: int64(math.Ceil(wallClock.Seconds())),
	}

	if stats == nil {
		return metrics
	}

	metrics.CPUSecondsUser = float64(stats.CPUUsageUser) / 1e9
	metrics.CPUSecondsSystem = float64(stats.CPUUsageKernel) / 1e9
	metrics.PeakMemoryMB = int32(stats.MemoryPeak / (1024 * 1024))

	// Estimate CPU cores used from total CPU time / wall clock.
	totalCPU := metrics.CPUSecondsUser + metrics.CPUSecondsSystem
	if wallClock.Seconds() > 0 && totalCPU > 0 {
		metrics.CPUCoresUsed = int32(math.Ceil(totalCPU / wallClock.Seconds()))
	}
	if metrics.CPUCoresUsed < 1 {
		metrics.CPUCoresUsed = 1
	}

	return metrics
}
