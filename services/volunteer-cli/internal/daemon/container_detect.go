package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Late container-engine detection (TB-59).
//
// The container runtime used to be built exactly once, before registration,
// and every head was told this machine's runtimes exactly once. An engine
// that came up AFTER the daemon — Docker Desktop still launching behind a
// login item, a Podman machine another program or the user starts later, a
// machine start slower than the 60 s wait — was invisible until a restart,
// while `doctor` (a fresh probe) said it was there. The daemon now keeps the
// detector for its whole life: while at least one head is trusted for
// CONTAINER and no container runtime is registered, it probes again every
// containerRedetectInterval and on request (the management API's redetect
// verb, and a machine start or setup that succeeds through it). When an
// engine answers, the runtime is built exactly as at start, registered, and
// every head is re-registered with the new capability on its next contact.

// containerRedetectInterval is how often a daemon without a container runtime
// probes for an engine again. A minute is short enough that a login-time
// race (Docker Desktop or a Podman machine coming up after the daemon) costs
// a minute of WASM-only work rather than a restart, and long enough that the
// probe — a binary lookup, a `podman --version`, a Docker ping — is noise
// nobody notices.
const containerRedetectInterval = 60 * time.Second

// machineSetupRetryInterval bounds how often a re-detection attempt may run
// the EXPENSIVE step of bringing a Podman machine up (`podman machine init`
// / `start` plus the wait for its socket) after that step has failed. The
// cheap probe keeps running every tick — the machine may come up by somebody
// else's hand in the meantime, and a socket that answers is registered at
// once — but a machine whose start keeps failing (WSL not ready, a broken
// hypervisor) is not re-driven every minute.
const machineSetupRetryInterval = runtimeAbandonCooldown

// machineReadyWait is how long a machine setup waits for the Podman socket to
// answer before giving up on this attempt (the same wait start-up applies).
const machineReadyWait = 60 * time.Second

// Errors RequestContainerRedetect returns when an on-demand probe is not
// applicable, so the management API can answer with a reason instead of a
// silent no-op.
var (
	ErrContainerRuntimeRegistered = errors.New("a container runtime is already registered")
	ErrContainerNotTrusted        = errors.New("no attached head is trusted to run container work on this machine")
	ErrContainerDetectUnavailable = errors.New("this daemon has no container-engine detector")
)

// ContainerRuntimeFactory detects a container engine and builds the container
// runtime for it — the backend connection, the resource ceilings and
// hardening knobs from config, the GPU list, and on macOS/Windows the Podman
// machine bring-up. One factory lives for the daemon's lifetime: the first
// Build runs before registration (cli buildRuntimeRegistry) and every later
// one from the daemon's re-detection loop, so the Podman machine manager —
// and with it the record of whether THIS process started the machine, which
// decides whether the daemon may stop it at shutdown (PB-27) — persists
// across attempts instead of being recreated blank each time.
type ContainerRuntimeFactory struct {
	cfg    *config.Config
	logger *slog.Logger

	mu sync.Mutex
	// mm is the Podman machine manager, created the first time a Podman
	// binary is found and kept from then on. nil until then, and always nil
	// on a Docker-only host.
	mm *runtime.PodmanMachineManager
	// backend is the engine the runtime returned by the last successful Build
	// was connected to; built records that a Build has succeeded.
	backend runtime.BackendInfo
	built   bool
	// lastErr is why the most recent Build produced no runtime although an
	// engine was found (a connection failure, typically a socket that is not
	// up yet). Empty when the last Build succeeded or found nothing.
	lastErr string
	// lastMachineSetupFailure is when a machine init/start or the wait for its
	// socket last failed; see machineSetupRetryInterval.
	lastMachineSetupFailure time.Time
	// engineMemoryMB is the memory of the VM the engine runs inside, as the
	// engine reported it at the last successful Build (0 on a host whose
	// containers share its RAM, or when the engine did not say); budgetMB is
	// the memory budget that Build gave container work — the configured budget
	// clipped to the VM (runtime.ContainerMemoryBudgetMB, TB-63). Both are 0
	// until a runtime has been built.
	engineMemoryMB int
	budgetMB       int

	// Seams for tests: engine detection, runtime construction and the engine
	// VM memory probe. Production wiring is DetectContainerBackendPreferred,
	// NewContainerRuntimeForBackend plus the configuration start-up applies,
	// and the engine's own /info on a VM platform.
	detect       func(preferred runtime.ContainerBackend) runtime.BackendInfo
	construct    func(backend runtime.BackendInfo) (runtime.Runtime, error)
	engineMemory func(rt runtime.Runtime) int
	now          func() time.Time
}

// NewContainerRuntimeFactory returns the production factory for this config.
func NewContainerRuntimeFactory(cfg *config.Config, logger *slog.Logger) *ContainerRuntimeFactory {
	f := &ContainerRuntimeFactory{cfg: cfg, logger: logger, now: time.Now}
	f.detect = func(preferred runtime.ContainerBackend) runtime.BackendInfo {
		return runtime.DetectContainerBackendPreferred(runtime.BundledPodmanPath(), preferred)
	}
	f.construct = f.constructContainerRuntime
	f.engineMemory = probeEngineMemoryMB
	return f
}

// NewContainerRuntimeFactoryForTest returns a factory whose detection and
// construction are the given functions, so a test can make an engine "appear"
// without a container engine on the host. The engine is treated as one whose
// VM memory is unknown (no memory clip); SetEngineMemoryProbeForTest changes
// that. Exported for the management package's tests.
func NewContainerRuntimeFactoryForTest(cfg *config.Config, logger *slog.Logger,
	detect func(preferred runtime.ContainerBackend) runtime.BackendInfo,
	construct func(backend runtime.BackendInfo) (runtime.Runtime, error)) *ContainerRuntimeFactory {
	return &ContainerRuntimeFactory{cfg: cfg, logger: logger, now: time.Now, detect: detect, construct: construct,
		engineMemory: func(runtime.Runtime) int { return 0 }}
}

// SetEngineMemoryProbeForTest replaces the engine VM memory probe: the given
// function answers "how much memory does the VM this engine runs inside have"
// for a runtime the construction seam returned (0 = none / unknown).
func (f *ContainerRuntimeFactory) SetEngineMemoryProbeForTest(probe func(rt runtime.Runtime) int) {
	f.engineMemory = probe
}

// probeEngineMemoryMB is the production engine VM memory probe: on a platform
// whose engine runs inside a VM (runtime.ContainerEngineRunsInVM) it asks the
// engine for its total memory — the VM's, since that is where the engine
// daemon runs — and reports 0 elsewhere, or when the engine does not answer.
// The engine's own figure is used rather than `podman machine inspect`, whose
// number is what Podman recorded at init and, on WSL, not what the VM has.
func probeEngineMemoryMB(rt runtime.Runtime) int {
	if !runtime.ContainerEngineRunsInVM() {
		return 0
	}
	cr, ok := rt.(*runtime.ContainerRuntime)
	if !ok || cr == nil || cr.Client() == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cr.Client().Info(ctx)
	if err != nil || info == nil || info.MemTotalMB <= 0 {
		return 0
	}
	return int(info.MemTotalMB)
}

// Build probes for a container engine once and, when one answers, returns the
// container runtime built for it and the engine it was built against. A nil
// runtime with a nil error means no engine answered; a nil runtime with an
// error means an engine was found but the runtime could not be built (the
// error says why — usually a socket that is not up yet). When a Podman binary
// is found on a platform that needs a machine, the machine is initialised and
// started as at start-up, unless that step failed within
// machineSetupRetryInterval and forceMachineSetup is false.
//
// Callers serialise Build (start-up runs it once before the daemon exists;
// afterwards only the daemon's RedetectContainerRuntime calls it, under its
// own lock). The factory's mutex guards field access only — a machine
// bring-up can take a minute or two, and the management API reads the
// factory's state meanwhile to report "starting".
func (f *ContainerRuntimeFactory) Build(forceMachineSetup bool) (runtime.Runtime, runtime.BackendInfo, error) {
	// Honor the operator's configured backend preference (container_backend).
	// When set to "docker", Docker is chosen if present so large images use
	// host storage instead of a Podman-machine VM. Empty = auto (Podman first).
	preferred := runtime.ContainerBackend(f.cfg.ContainerBackend)
	backend := f.detect(preferred)
	if backend.Backend == runtime.BackendPodman {
		mm := f.machineManagerFor(backend.BinaryPath)
		if mm.NeedsMachine() {
			if forceMachineSetup || f.machineSetupDue() {
				f.recordMachineSetup(f.setUpMachine(mm))
				// Re-detect backend after machine setup to get updated socket path.
				backend = f.detect(preferred)
			} else {
				f.logger.Debug("podman machine setup failed recently; probing the socket only until the retry interval passes",
					"retry_interval", machineSetupRetryInterval)
			}
		}
	}
	if backend.Backend == runtime.BackendNone {
		f.recordResult(backend, false, "")
		return nil, backend, nil
	}
	rt, err := f.construct(backend)
	if err != nil {
		f.recordResult(backend, false, err.Error())
		return nil, backend, err
	}
	f.applyContainerMemoryBudget(rt)
	f.recordResult(backend, true, "")
	return rt, backend, nil
}

// applyContainerMemoryBudget gives a freshly built container runtime its
// memory ceiling: the configured budget, clipped to the engine VM's memory less
// headroom when the engine runs inside a VM whose size the probe reports
// (runtime.ContainerMemoryBudgetMB, TB-63). The figures are recorded on the
// factory so the daemon can advertise the same budget to heads, book admission
// against it, and name the VM in its diagnostics. Before this the ceiling was
// the configuration alone, so a Mac with a 2 GiB Podman machine advertised
// 8192 MB, was sent 7000 MB units, and had each one killed at model load.
func (f *ContainerRuntimeFactory) applyContainerMemoryBudget(rt runtime.Runtime) {
	configMB := f.cfg.ResourceLimits.MaxMemoryMB
	engineMB := f.engineMemory(rt)
	budget := runtime.ContainerMemoryBudgetMB(configMB, engineMB)
	if cr, ok := rt.(*runtime.ContainerRuntime); ok && cr != nil {
		cr.SetEngineMemoryMB(engineMB)
		cr.SetMemoryCeilingMB(budget)
	}
	f.mu.Lock()
	f.engineMemoryMB = engineMB
	f.budgetMB = budget
	f.mu.Unlock()
	if engineMB > 0 {
		f.logger.Info("container engine runs inside a VM; container work is budgeted against the VM's memory",
			"engine_vm_memory_mb", engineMB, "headroom_mb", runtime.ContainerVMHeadroomMB,
			"container_memory_budget_mb", budget, "max_memory_mb", configMB)
	}
}

// ContainerMemory reports the memory budget the last built container runtime
// was given and the memory of the VM its engine runs inside (0 when the
// engine shares the host's RAM or its VM size is unknown). Both are 0 until a
// runtime has been built.
func (f *ContainerRuntimeFactory) ContainerMemory() (budgetMB, engineMemoryMB int) {
	if f == nil {
		return 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgetMB, f.engineMemoryMB
}

// ClampAdvertisedMemory lowers the memory budget in a hardware advertisement
// to the container memory budget when the engine VM clips the configuration
// (TB-63), so a head's dispatch gate — which compares a leaf's max_memory_mb
// against this figure on every poll — stops sending this machine units its VM
// cannot hold. It reports whether the advertisement changed. A nil factory,
// no built runtime, or a budget at or above the advertised figure leaves the
// advertisement as it is.
func (f *ContainerRuntimeFactory) ClampAdvertisedMemory(hw *lettucev1.HardwareCapabilities) bool {
	if hw == nil {
		return false
	}
	budget, engineMB := f.ContainerMemory()
	if engineMB <= 0 || budget <= 0 || int32(budget) >= hw.MaxMemoryMb {
		return false
	}
	hw.MaxMemoryMb = int32(budget)
	return true
}

// machineManagerFor returns the machine manager, creating it on the first
// call that found a Podman binary and keeping it from then on (PB-27).
func (f *ContainerRuntimeFactory) machineManagerFor(binaryPath string) *runtime.PodmanMachineManager {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mm == nil {
		f.mm = runtime.NewPodmanMachineManager(binaryPath, f.logger)
	}
	return f.mm
}

// machineSetupDue reports whether the machine bring-up may run: always, until
// it has failed; then only once machineSetupRetryInterval has passed.
func (f *ContainerRuntimeFactory) machineSetupDue() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMachineSetupFailure.IsZero() || f.now().Sub(f.lastMachineSetupFailure) >= machineSetupRetryInterval
}

// recordMachineSetup notes a machine bring-up's outcome for machineSetupDue.
func (f *ContainerRuntimeFactory) recordMachineSetup(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.lastMachineSetupFailure = f.now()
	} else {
		f.lastMachineSetupFailure = time.Time{}
	}
}

// recordResult notes a Build's outcome: the engine a runtime was built
// against (only on success — a later "nothing found" must not erase it), and
// the reason construction failed, if it did.
func (f *ContainerRuntimeFactory) recordResult(backend runtime.BackendInfo, built bool, errStr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastErr = errStr
	if built {
		f.backend = backend
		f.built = true
	}
}

// setUpMachine initialises and starts the Podman machine (idempotent — an
// already-running machine is left as found and NOT owned, PB-27) and waits
// for its socket. Sized from the resource limits with the same floors
// start-up has always used. A machine Lettuce creates gets the configured
// memory PLUS the VM headroom, so the budget container work is given after the
// clip (runtime.ContainerMemoryBudgetMB) is the configured figure and not a
// permanently "clipped" one (TB-63); an existing machine keeps its size.
func (f *ContainerRuntimeFactory) setUpMachine(mm *runtime.PodmanMachineManager) error {
	f.logger.Info("setting up Podman machine for container runtime")
	cpus, memMB, diskGB := MachineSizeFor(f.cfg.ResourceLimits)
	if err := mm.Setup(cpus, memMB, diskGB); err != nil {
		f.logger.Warn("podman machine setup failed, container runtime may be unavailable", "error", err)
		return err
	}
	if err := mm.WaitForReady(machineReadyWait); err != nil {
		f.logger.Warn("podman machine not ready after setup", "error", err)
		return err
	}
	return nil
}

// MachineSizeFor returns the CPUs, memory (MB) and disk (GB) a Podman machine
// Lettuce creates is sized to for the given resource limits: each limit with
// the floor start-up has always applied (2 CPUs, 4096 MB, 20 GB), and the
// memory raised by runtime.ContainerVMHeadroomMB so that, once the VM's own
// reserve is kept back, container work is budgeted the configured figure
// rather than a clipped one (TB-63). Shared with the management API's machine
// setup so the app's "set up" button and start-up size a machine alike.
func MachineSizeFor(rl config.ResourceLimits) (cpus, memMB, diskGB int) {
	cpus, memMB, diskGB = rl.MaxCPUCores, rl.MaxMemoryMB, rl.MaxDiskGB
	if cpus <= 0 {
		cpus = 2
	}
	if memMB <= 0 {
		memMB = 4096
	}
	if diskGB <= 0 {
		diskGB = 20
	}
	memMB += runtime.ContainerVMHeadroomMB
	return cpus, memMB, diskGB
}

// constructContainerRuntime is the production construction step: the backend
// connection plus every knob start-up applies (BG-16 booked disk clamp, BG-13
// hardening, the GPU list, Podman's GPU readiness). The memory ceiling is
// applied by Build (applyContainerMemoryBudget) once the engine's VM size is
// known, so it is not set here.
func (f *ContainerRuntimeFactory) constructContainerRuntime(backend runtime.BackendInfo) (runtime.Runtime, error) {
	cr, err := runtime.NewContainerRuntimeForBackend(f.cfg.DataDir, f.logger, backend)
	if err != nil {
		return nil, err
	}
	cr.SetMaxCPUCores(f.cfg.ResourceLimits.MaxCPUCores)
	cr.SetMaxGPUVRAMPct(f.cfg.ResourceLimits.MaxGPUVRAMPct)
	cr.SetDiskCeilingMB(f.cfg.ResourceLimits.MaxDiskGB * 1024)
	cr.SetHardeningConfig(f.cfg.ResourceLimits.MaxPids, f.cfg.ContainerCapAdd, f.cfg.ContainerGPURelaxUser)
	if gpus := runtime.DetectGPUs(); len(gpus) > 0 {
		cr.SetGPUs(gpus)
	}
	if backend.Backend == runtime.BackendPodman {
		runtime.EnsurePodmanGPUReady(f.logger)
	}
	return cr, nil
}

// MachineManager returns the Podman machine manager created by the first
// Build that found a Podman binary, or nil.
func (f *ContainerRuntimeFactory) MachineManager() *runtime.PodmanMachineManager {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mm
}

// Backend returns the engine the last successfully built runtime was
// connected to, and whether any Build has succeeded.
func (f *ContainerRuntimeFactory) Backend() (runtime.BackendInfo, bool) {
	if f == nil {
		return runtime.BackendInfo{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backend, f.built
}

// LastError returns why the most recent Build found an engine but produced no
// runtime; empty otherwise.
func (f *ContainerRuntimeFactory) LastError() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErr
}

// --- the daemon's side ---

// containerRedetectActive reports whether the re-detection loop has anything
// to do: a detector, no container runtime yet, and a head trusted for
// CONTAINER (the same condition under which start-up probes at all). Trust is
// read from config, as start-up reads it, so a head that was down at start
// still counts.
func (d *Daemon) containerRedetectActive() bool {
	if d.containerFactory == nil || d.runtimeRegistry == nil || d.cfg == nil {
		return false
	}
	if d.runtimeRegistry.GetRuntime("container") != nil {
		return false
	}
	for _, srv := range d.cfg.Servers {
		if srv.TrustsRuntime("CONTAINER") {
			return true
		}
	}
	return false
}

// ContainerRedetectActive is containerRedetectActive for the management API,
// so the app can say "Lettuce is checking for an engine" only when it is.
func (d *Daemon) ContainerRedetectActive() bool {
	return d.containerRedetectActive()
}

// runContainerRedetect is the re-detection loop, started by Run and tied to
// the run context. It exits for good once a container runtime is registered:
// a runtime that later stops answering is the runtime breaker's business
// (runtimeAbandonCooldown), not this loop's.
func (d *Daemon) runContainerRedetect(ctx context.Context) {
	if !d.containerRedetectActive() {
		return
	}
	d.logger.Info("no container runtime at start; re-checking for a container engine periodically — start Docker or the Podman machine and container work begins without a restart",
		"interval", containerRedetectInterval)
	timer := time.NewTimer(containerRedetectInterval)
	defer timer.Stop()
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-d.containerRedetectCh:
			// An explicit request (the API verb, a machine start that just
			// succeeded): probe now and let a machine bring-up run even inside
			// its retry interval — the person asking has usually just fixed it.
			force = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if d.RedetectContainerRuntime(ctx, force) {
			return
		}
		timer.Reset(containerRedetectInterval)
	}
}

// RequestContainerRedetect asks the loop to probe for an engine now instead of
// at its next tick. It returns an error when a probe is not applicable, so the
// caller can say why rather than wait for nothing.
func (d *Daemon) RequestContainerRedetect() error {
	switch {
	case d.containerFactory == nil:
		return ErrContainerDetectUnavailable
	case d.runtimeRegistry != nil && d.runtimeRegistry.GetRuntime("container") != nil:
		return ErrContainerRuntimeRegistered
	case !d.containerRedetectActive():
		return ErrContainerNotTrusted
	}
	select {
	case d.containerRedetectCh <- struct{}{}:
	default: // a probe is already queued
	}
	return nil
}

// RedetectContainerRuntime runs one detection attempt now and registers the
// container runtime if an engine answers. It returns true when a container
// runtime is registered afterwards (already, or by this call). Attempts are
// serialised, so the loop and an on-demand request cannot build two runtimes.
func (d *Daemon) RedetectContainerRuntime(ctx context.Context, forceMachineSetup bool) bool {
	d.containerRedetectMu.Lock()
	defer d.containerRedetectMu.Unlock()
	if d.runtimeRegistry == nil || d.containerFactory == nil {
		return false
	}
	if d.runtimeRegistry.GetRuntime("container") != nil {
		return true
	}

	rt, backend, err := d.containerFactory.Build(forceMachineSetup)
	if rt == nil {
		// Log only when the answer changes, so a host that simply has no engine
		// is not told so every minute; the first "still nothing" after the
		// start-up WARN is silent.
		outcome := "none"
		if err != nil {
			outcome = "error: " + err.Error()
		}
		if outcome != d.lastRedetectOutcome {
			if err != nil {
				d.logger.Warn("container engine found but its runtime could not be built; will keep re-checking",
					"backend", backend.Backend, "socket", backend.SocketPath, "error", err)
			} else {
				d.logger.Info("no container engine answered; will keep re-checking", "interval", containerRedetectInterval)
			}
		} else {
			d.logger.Debug("container engine re-check unchanged", "outcome", outcome)
		}
		d.lastRedetectOutcome = outcome
		return false
	}
	d.lastRedetectOutcome = "registered"
	d.registerContainerRuntime(ctx, rt, backend)
	return true
}

// registerContainerRuntime puts a late-built container runtime into service:
// into the registry (the fetcher's per-leaf runtime gate, SelectRuntime and
// the disk gate all read it live), with the keep-set for its stale-image
// reaper and the start-up reclaim of stranded containers that Run gives a
// runtime present at start (#58, #60); then every head is flagged for
// re-registration and the no-runnable-leaf verdict is re-evaluated (TB-60).
func (d *Daemon) registerContainerRuntime(ctx context.Context, rt runtime.Runtime, backend runtime.BackendInfo) {
	d.runtimeRegistry.Register(rt)
	if cr, ok := rt.(*runtime.ContainerRuntime); ok && cr != nil {
		cr.SetWantedImages(d.allEnabledImageRefs)
		if d.IsRunning() && d.slotManager != nil && d.prefetchQueue != nil {
			owned := d.ownedWorkUnitIDs()
			go func() {
				cr.ReapStrandedContainers(ctx, owned)
				cr.ReapStaleImages(ctx)
			}()
		}
	}
	// The image-store probe cached "no container runtime" for its TTL; let the
	// disk gate ask the new backend where it keeps images on its next check.
	d.imgStoreMu.Lock()
	d.imgStoreChecked = time.Time{}
	d.imgStoreMu.Unlock()

	runtimes := d.runtimeRegistry.AvailableRuntimes()
	sort.Strings(runtimes)
	d.logger.Info("container runtime registered after start: this machine can now run container work",
		"backend", backend.Backend, "engine", backend.Engine, "version", backend.Version,
		"socket", backend.SocketPath, "runtimes", runtimes)

	// The engine's VM may be smaller than the configured budget (TB-63): lower
	// the advertised figure BEFORE the heads are re-told, so the re-registration
	// and every later poll carry the budget the VM can hold.
	d.applyContainerMemoryBudget()
	d.markRuntimesChanged()
	d.refreshRuntimeBlocked()
}

// markRuntimesChanged flags every head for re-registration: the runtimes this
// machine advertised at start are no longer what it can run. The fetcher
// performs the re-registration at the top of each head's next turn
// (readvertiseIfPending), so a head that is unreachable right now is retried
// when it is next contacted rather than forgotten until a restart, and the
// per-head host id is only ever touched from the fetcher's goroutine.
func (d *Daemon) markRuntimesChanged() {
	if d.multiClient == nil {
		return
	}
	d.readvertiseMu.Lock()
	defer d.readvertiseMu.Unlock()
	if d.readvertisePending == nil {
		d.readvertisePending = make(map[string]bool)
	}
	for _, srv := range d.multiClient.Servers() {
		d.readvertisePending[srv.Name] = true
	}
}

// readvertiseIfPending re-registers this machine with one head if its
// advertised runtimes changed since the head last heard them. Called by the
// fetcher before it asks the head for work, so the head's dispatch gate —
// which matches a leaf's runtime against what we advertised — lets the newly
// runnable leafs through instead of refusing them until a restart. The
// registration echoes the head's own host id, so it is an UPDATE of this
// machine's row (available_runtimes), never a new host and never a
// proof-of-work challenge.
func (d *Daemon) readvertiseIfPending(ctx context.Context, head *ServerConnection) {
	d.readvertiseMu.Lock()
	pending := d.readvertisePending[head.Name]
	d.readvertiseMu.Unlock()
	if !pending {
		return
	}
	rc, ok := head.Client.(registerClient)
	if !ok {
		d.logger.Warn("daemon: head client cannot re-register; the head will learn this machine's new runtimes at the next restart", "server", head.Name)
		d.clearReadvertise(head.Name)
		return
	}
	advertised := d.advertisedRuntimesFor(head.Config)
	req := client.BuildRegistrationRequest(d.pubKey, head.HostID, d.advertisedHardware(), d.cfg, advertised...)
	resp, err := rc.RegisterVolunteer(ctx, req)
	if err != nil {
		d.logger.Warn("daemon: could not advertise this machine's new runtimes to head; will retry on its next contact",
			"server", head.Name, "advertised", advertised, "error", err)
		return
	}
	d.clearReadvertise(head.Name)
	if resp.HostId != head.HostID {
		// Adopt EXACTLY what the head returned, as every registration does.
		head.HostID = resp.HostId
		if d.hostIDStore != nil {
			key := head.Config.GRPCAddress
			var perr error
			if resp.HostId == "" {
				perr = d.hostIDStore.Delete(key)
			} else {
				perr = d.hostIDStore.Set(key, resp.HostId)
			}
			if perr != nil {
				d.logger.Warn("daemon: failed to persist re-issued host id", "server", head.Name, "error", perr)
			}
		}
	}
	d.logger.Info("advertised runtimes to head", "server", head.Name, "advertised", advertised)
}

func (d *Daemon) clearReadvertise(headName string) {
	d.readvertiseMu.Lock()
	delete(d.readvertisePending, headName)
	d.readvertiseMu.Unlock()
}

// advertisedRuntimesFor returns the UPPERCASE runtimes to advertise to one
// head: the intersection of what this machine can run (the live registry) and
// what the volunteer trusts that head to run — exactly what start-up
// advertises (cli advertisedForServer). A backend-less machine never
// advertises CONTAINER even to a head trusted for it; a head not trusted for
// NATIVE never hears NATIVE even on a native-capable machine.
func (d *Daemon) advertisedRuntimesFor(srv config.ServerConfig) []string {
	if d.runtimeRegistry == nil {
		return nil
	}
	capable := make(map[string]bool)
	for _, n := range d.runtimeRegistry.AvailableRuntimes() {
		capable[strings.ToUpper(n)] = true
	}
	var out []string
	for _, r := range srv.EffectiveTrustedRuntimes() {
		if capable[r] {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// --- the no-runnable-leaf verdict (TB-60) ---

// refreshRuntimeBlocked re-evaluates whether EVERY attached leaf is
// runtime-blocked — needs a runtime this machine lacks or the volunteer has
// not trusted its head for (leafRuntimeVerdict, the fetcher's pre-request
// skip) — and keeps the volunteer-facing "runtime_blocked" notice in step
// with it: raised once with a WARN when the condition starts, resolved when
// it ends (a runtime registered late, TB-59; the leaf set changed). It
// returns the verdict. This is a static configuration fact, not evidence
// about the head, so it is kept apart from the "no work after repeated
// polls" streak, which since TB-60 counts only rounds in which a head was
// actually asked.
func (d *Daemon) refreshRuntimeBlocked() bool {
	if d.multiClient == nil || d.runtimeRegistry == nil {
		return false
	}
	total, eligible, containerBlocked, trustBlocked := d.readinessCounts()
	blocked := total > 0 && eligible == 0 && containerBlocked+trustBlocked == total

	d.runtimeBlockedMu.Lock()
	defer d.runtimeBlockedMu.Unlock()
	switch {
	case blocked && !d.runtimeBlocked:
		d.runtimeBlocked = true
		d.warnRuntimeBlocked(total, containerBlocked, trustBlocked)
	case !blocked && d.runtimeBlocked:
		d.runtimeBlocked = false
		d.logger.Info("an attached leaf can run on this machine again; the no-runnable-leaf notice is resolved",
			"eligible_leafs", eligible, "total_leafs", total)
		d.notices.Resolve("runtime_blocked", "", "")
	}
	return blocked
}

// warnRuntimeBlocked emits the one WARN and the notice for the verdict. The
// container wording says the daemon keeps checking for an engine when it does
// (TB-59), so the remedy is "start it", not "restart Lettuce". Called with
// runtimeBlockedMu held.
func (d *Daemon) warnRuntimeBlocked(total, containerBlocked, trustBlocked int) {
	runtimes := d.runtimeRegistry.AvailableRuntimes()
	sort.Strings(runtimes)
	available := strings.Join(runtimes, ", ")

	if containerBlocked == total {
		recheck := "then restart the daemon"
		if d.containerRedetectActive() {
			recheck = "Lettuce checks for an engine every minute and starts container work as soon as one answers, no restart needed"
		}
		d.logger.Warn("no runnable leafs: every attached leaf needs a container runtime and none is available here — install Docker or Podman, or start it if it is installed; the daemon re-checks for an engine every minute",
			"runtimes", runtimes, "container_leafs", containerBlocked)
		d.notices.Notify(NoticeWarn, "runtime_blocked",
			fmt.Sprintf("No attached leaf can run on this machine: all %d attached leaf(s) need a container runtime and none is available here (available runtimes: %s). Install Docker or Podman, or start it if it is installed — %s — or attach a head with WASM or native leafs.",
				total, available, recheck),
			"", "")
		return
	}
	d.logger.Warn("no runnable leafs: every attached leaf needs a runtime this volunteer has not trusted its head to run (or does not have) — opt in per head with 'lettuce-volunteer heads trust <head> <runtime>' if you accept running that head's code, then restart the daemon",
		"runtimes", runtimes, "leafs", total, "trust_blocked_leafs", trustBlocked, "container_blocked_leafs", containerBlocked)
	d.notices.Notify(NoticeWarn, "runtime_blocked",
		fmt.Sprintf("No attached leaf can run on this machine: all %d attached leaf(s) need a runtime you have not trusted their head to run (%d) or this machine does not have (%d); available runtimes: %s. Opt in per head with 'lettuce-volunteer heads trust <head> <runtime>' if you accept running that head's code, then restart the daemon.",
			total, trustBlocked, containerBlocked, available),
		"", "")
}
