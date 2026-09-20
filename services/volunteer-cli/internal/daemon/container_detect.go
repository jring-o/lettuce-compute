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
//
// The same machinery covers the other direction (TB-80): an engine that was
// up and stops answering — a Podman machine whose API socket died behind a
// VM that still reports "running", Docker Desktop quit, a rootless socket
// file nothing listens on. The runtime that reports the outage is taken out
// of service (NoteContainerEngineUnreachable), every buffered container unit
// is returned to its head un-run, the heads are re-told the machine's
// runtimes, and the loop below — which never exits while the daemon runs —
// probes again until the engine answers, then registers a fresh runtime and
// resolves the notice. A probe is a ping, never a fetched unit, and a runtime
// is registered only after its engine has answered one.
//
// One outage is the volunteer's own doing (TB-88): the app's "Stop Machine"
// button, or `podman machine stop` in a terminal. The loop used to treat the
// machine that went away exactly like one that had never been started — its
// next probe, a second after the stop, ran the bring-up and the machine was
// back within the minute. Now a Podman machine that was running under this
// daemon and is found stopped is left stopped: the factory holds the machine
// (machineHeld), the loop only checks each minute whether somebody has
// started it again, and the hold is released by a person's explicit request
// (the app's Start / Setup / "Check again now", which force the bring-up) or
// a daemon restart. The bring-up at daemon start is unchanged.

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
	// ErrPodmanMachineHeldStopped is Build's answer while the volunteer's
	// stopped machine is left stopped (TB-88): no runtime, and not an outage
	// to warn about.
	ErrPodmanMachineHeldStopped = errors.New("the Podman machine was stopped by the volunteer and is left stopped")
)

// Where a Podman machine stop came from, for the hold's record and the
// management API (ContainerRuntimeStatusResponse.MachineStopSource).
const (
	// MachineStopFromApp: the app's Stop Machine button (the management API's
	// stop verb).
	MachineStopFromApp = "app"
	// MachineStopOutside: the machine that was running under this daemon was
	// found stopped by the probe after an outage — `podman machine stop` in a
	// terminal, Podman Desktop, another program.
	MachineStopOutside = "outside"
)

// describeMachineStop is the plain-language form of a stop source.
func describeMachineStop(source string) string {
	if source == MachineStopFromApp {
		return "from the app"
	}
	return "outside Lettuce (podman machine stop, Podman Desktop, or another program)"
}

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
	// machineHeld records that the Podman machine was running under this
	// daemon and was then stopped by the volunteer — machineStopSource says
	// from where — so Build leaves it stopped instead of starting it again
	// (TB-88). Released by a forced Build (a person's request), by a
	// successful registration (somebody started the machine by hand), or by
	// ReleaseMachineHold.
	machineHeld       bool
	machineStopSource string
	// engineMemoryMB is the memory of the VM the engine runs inside, as the
	// engine reported it at the last successful Build (0 on a host whose
	// containers share its RAM, or when the engine did not say); budgetMB is
	// the memory budget that Build gave container work — the configured budget
	// clipped to the VM (runtime.ContainerMemoryBudgetMB, TB-63). engineCPUs
	// and cpuBudget are the CPU twins (the VM's vCPU count and the configured
	// max_cpu_cores clipped to it, runtime.ContainerCPUBudget, TB-75). All
	// are 0 until a runtime has been built.
	engineMemoryMB int
	budgetMB       int
	engineCPUs     int
	cpuBudget      int

	// Seams for tests: engine detection, runtime construction and the engine
	// VM probe (its memory and vCPU count). Production wiring is
	// DetectContainerBackendPreferred, NewContainerRuntimeForBackend plus the
	// configuration start-up applies, and the engine's own /info on a VM
	// platform.
	detect    func(preferred runtime.ContainerBackend) runtime.BackendInfo
	construct func(backend runtime.BackendInfo) (runtime.Runtime, error)
	engineVM  func(rt runtime.Runtime) (memMB, cpus int)
	// ping asks the engine behind a freshly constructed runtime to answer
	// before the runtime is put into service (TB-80): detection on Linux is
	// a socket FILE check, and a socket nobody listens on used to register
	// CONTAINER, advertise it, and fail every Prepare. Production pings the
	// engine's API; the test factory accepts any runtime unless a test
	// installs its own (SetEnginePingForTest).
	ping func(rt runtime.Runtime) error
	now  func() time.Time
}

// NewContainerRuntimeFactory returns the production factory for this config.
func NewContainerRuntimeFactory(cfg *config.Config, logger *slog.Logger) *ContainerRuntimeFactory {
	f := &ContainerRuntimeFactory{cfg: cfg, logger: logger, now: time.Now}
	f.detect = func(preferred runtime.ContainerBackend) runtime.BackendInfo {
		return runtime.DetectContainerBackendPreferred(runtime.BundledPodmanPath(), preferred)
	}
	f.construct = f.constructContainerRuntime
	f.engineVM = probeEngineVM
	f.ping = pingContainerRuntime
	return f
}

// NewContainerRuntimeFactoryForTest returns a factory whose detection and
// construction are the given functions, so a test can make an engine "appear"
// without a container engine on the host. The engine is treated as one whose
// VM size is unknown (no memory or CPU clip); SetEngineMemoryProbeForTest and
// SetEngineVMProbeForTest change that. Exported for the management package's
// tests.
func NewContainerRuntimeFactoryForTest(cfg *config.Config, logger *slog.Logger,
	detect func(preferred runtime.ContainerBackend) runtime.BackendInfo,
	construct func(backend runtime.BackendInfo) (runtime.Runtime, error)) *ContainerRuntimeFactory {
	return &ContainerRuntimeFactory{cfg: cfg, logger: logger, now: time.Now, detect: detect, construct: construct,
		engineVM: func(runtime.Runtime) (int, int) { return 0, 0 },
		ping:     func(runtime.Runtime) error { return nil }}
}

// SetEnginePingForTest replaces the registration ping: the given function
// answers "does the engine behind this runtime answer" for a runtime the
// construction seam returned (nil = yes).
func (f *ContainerRuntimeFactory) SetEnginePingForTest(ping func(rt runtime.Runtime) error) {
	f.ping = ping
}

// enginePingTimeout bounds the registration ping. The engine is local; an
// answer that takes longer than this is not one.
const enginePingTimeout = 5 * time.Second

// pingContainerRuntime is the production registration ping: the engine's
// API must answer before its runtime is registered. A runtime that is not a
// container runtime (nothing to ping) passes. The failure is reported as an
// EngineUnreachableError so the detector's log and the status route name
// the socket the volunteer has to look at.
func pingContainerRuntime(rt runtime.Runtime) error {
	cr, ok := rt.(*runtime.ContainerRuntime)
	if !ok || cr == nil || cr.Client() == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), enginePingTimeout)
	defer cancel()
	if err := cr.Client().Ping(ctx); err != nil {
		return &runtime.EngineUnreachableError{Backend: cr.Backend(), Socket: cr.EngineSocket(), Err: err}
	}
	return nil
}

// SetEngineMemoryProbeForTest replaces the engine VM memory probe: the given
// function answers "how much memory does the VM this engine runs inside have"
// for a runtime the construction seam returned (0 = none / unknown). The VM's
// CPU count stays unknown.
func (f *ContainerRuntimeFactory) SetEngineMemoryProbeForTest(probe func(rt runtime.Runtime) int) {
	f.engineVM = func(rt runtime.Runtime) (int, int) { return probe(rt), 0 }
}

// SetEngineVMProbeForTest replaces the engine VM probe: the given function
// answers "how much memory and how many CPUs does the VM this engine runs
// inside have" (0 = none / unknown for either).
func (f *ContainerRuntimeFactory) SetEngineVMProbeForTest(probe func(rt runtime.Runtime) (memMB, cpus int)) {
	f.engineVM = probe
}

// probeEngineVM is the production engine VM probe: on a platform whose engine
// runs inside a VM (runtime.ContainerEngineRunsInVM) it asks the engine for
// its total memory and CPU count — the VM's, since that is where the engine
// daemon runs — and reports 0 elsewhere, or when the engine does not answer.
// The engine's own figures are used rather than `podman machine inspect`,
// whose numbers are what Podman recorded at init and, on WSL, not what the
// VM has.
func probeEngineVM(rt runtime.Runtime) (memMB, cpus int) {
	if !runtime.ContainerEngineRunsInVM() {
		return 0, 0
	}
	cr, ok := rt.(*runtime.ContainerRuntime)
	if !ok || cr == nil || cr.Client() == nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cr.Client().Info(ctx)
	if err != nil || info == nil {
		return 0, 0
	}
	if info.MemTotalMB > 0 {
		memMB = int(info.MemTotalMB)
	}
	if info.NCPU > 0 {
		cpus = info.NCPU
	}
	return memMB, cpus
}

// Build probes for a container engine once and, when one answers, returns the
// container runtime built for it and the engine it was built against. A nil
// runtime with a nil error means no engine was found; a nil runtime with an
// error means an engine was found but the runtime could not be built or its
// socket did not answer the registration ping (the error says why — usually
// a socket that is not up yet, or one nothing listens on). When a Podman binary
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
			switch {
			case forceMachineSetup:
				// Start-up, or a person's explicit request (the redetect verb;
				// a start or setup through the API): a held machine is
				// released and the bring-up runs even inside its retry interval.
				f.ReleaseMachineHold()
				f.recordMachineSetup(f.setUpMachine(mm))
				// Re-detect backend after machine setup to get updated socket path.
				backend = f.detect(preferred)
			case f.machineHeldStopped():
				// The volunteer stopped the machine (TB-88): leave it stopped.
				// Only look (a fresh inspect, once a minute) whether somebody
				// has started it again — then connect below, and the
				// registration releases the hold.
				mm.InvalidateStatus()
				if st := mm.Status(); st.Status != runtime.MachineRunning {
					_, source := f.MachineHeldStopped()
					err := fmt.Errorf("%w (stopped %s)", ErrPodmanMachineHeldStopped, describeMachineStop(source))
					f.recordResult(backend, false, err.Error())
					return nil, backend, err
				}
			case f.machineSetupDue():
				if f.stoppedSinceBuilt(mm) {
					// A machine this daemon had a runtime on is now stopped and
					// nothing here stopped it: the volunteer did, by hand. Hold
					// it rather than undo their stop (TB-88).
					f.HoldMachineStopped(MachineStopOutside)
					err := fmt.Errorf("%w (stopped %s)", ErrPodmanMachineHeldStopped, describeMachineStop(MachineStopOutside))
					f.recordResult(backend, false, err.Error())
					return nil, backend, err
				}
				f.recordMachineSetup(f.setUpMachine(mm))
				backend = f.detect(preferred)
			default:
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
	// An engine that was found but does not answer is not registered: a
	// socket file with no service behind it advertised CONTAINER to every
	// head and failed each unit at the ping (TB-80). The probe keeps running;
	// the runtime is built again when the socket answers.
	if err := f.ping(rt); err != nil {
		closeRuntimeClient(rt)
		f.recordResult(backend, false, err.Error())
		return nil, backend, err
	}
	f.applyContainerBudgets(rt)
	f.recordResult(backend, true, "")
	return rt, backend, nil
}

// closeRuntimeClient releases the engine connection of a runtime that will
// not be registered.
func closeRuntimeClient(rt runtime.Runtime) {
	if cr, ok := rt.(*runtime.ContainerRuntime); ok && cr != nil && cr.Client() != nil {
		_ = cr.Client().Close()
	}
}

// applyContainerBudgets gives a freshly built container runtime its memory
// ceiling and records the engine VM's size: the memory budget is the
// configured one, clipped to the VM's memory less headroom when the engine
// runs inside a VM whose size the probe reports (runtime.ContainerMemoryBudgetMB,
// TB-63); the CPU budget is max_cpu_cores clipped to the VM's vCPUs
// (runtime.ContainerCPUBudget, TB-75). The figures are recorded on the factory
// so the daemon can advertise the same budgets to heads, book admission
// against them, and name the VM in its diagnostics. Before this the ceilings
// were the configuration alone, so a Mac with a 2 GiB Podman machine
// advertised 8192 MB, was sent 7000 MB units, and had each one killed at model
// load. The runtime's CPU grant — what each container is given — is the
// static whole budget until the daemon wires its live equal split
// (wireRuntimeCPU).
func (f *ContainerRuntimeFactory) applyContainerBudgets(rt runtime.Runtime) {
	configMB := f.cfg.ResourceLimits.MaxMemoryMB
	configCores := f.cfg.ResourceLimits.MaxCPUCores
	engineMB, engineCPUs := f.engineVM(rt)
	budget := runtime.ContainerMemoryBudgetMB(configMB, engineMB)
	cpuBudget := runtime.ContainerCPUBudget(configCores, engineCPUs)
	if cr, ok := rt.(*runtime.ContainerRuntime); ok && cr != nil {
		cr.SetEngineMemoryMB(engineMB)
		cr.SetMemoryCeilingMB(budget)
		cr.SetCPUBudget(cpuBudget)
	}
	f.mu.Lock()
	f.engineMemoryMB = engineMB
	f.budgetMB = budget
	f.engineCPUs = engineCPUs
	f.cpuBudget = cpuBudget
	f.mu.Unlock()
	if engineMB > 0 {
		f.logger.Info("container engine runs inside a VM; container work is budgeted against the VM's memory",
			"engine_vm_memory_mb", engineMB, "headroom_mb", runtime.ContainerVMHeadroomMB,
			"container_memory_budget_mb", budget, "max_memory_mb", configMB)
	}
	if engineCPUs > 0 {
		f.logger.Info("container engine runs inside a VM; the CPU budget is bounded by the VM's CPUs",
			"engine_vm_cpus", engineCPUs, "cpu_budget_cores", cpuBudget, "max_cpu_cores", configCores)
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

// ContainerCPUs reports the CPU budget the last built container runtime was
// given (max_cpu_cores clipped to the VM) and the vCPU count of the VM its
// engine runs inside (0 when the engine shares the host's CPUs or the count
// is unknown). Both are 0 until a runtime has been built (TB-75).
func (f *ContainerRuntimeFactory) ContainerCPUs() (budgetCores, engineCPUs int) {
	if f == nil {
		return 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpuBudget, f.engineCPUs
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

// ClampAdvertisedCPU is ClampAdvertisedMemory's CPU twin (TB-75): it lowers
// the CPU budget in a hardware advertisement to the container CPU budget when
// the engine VM's vCPU count clips the configuration, so a head's dispatch
// gate — which compares a leaf's min_cpu_cores against this figure — stops
// sending this machine units its VM cannot run at their declared size. It
// reports whether the advertisement changed.
func (f *ContainerRuntimeFactory) ClampAdvertisedCPU(hw *lettucev1.HardwareCapabilities) bool {
	if hw == nil {
		return false
	}
	budget, engineCPUs := f.ContainerCPUs()
	if engineCPUs <= 0 || budget <= 0 || int32(budget) >= hw.MaxCpuCores {
		return false
	}
	hw.MaxCpuCores = int32(budget)
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

// stoppedSinceBuilt reports whether a Podman machine this factory once built
// a runtime on — so it was running under this daemon — now reads stopped. A
// fresh `podman machine inspect`, not the manager's cache: the cache may hold
// the state from before the stop. A machine that reports running with a dead
// socket (the applehv stale-socket family) is not stopped, and the bring-up
// still runs for it as before.
func (f *ContainerRuntimeFactory) stoppedSinceBuilt(mm *runtime.PodmanMachineManager) bool {
	f.mu.Lock()
	wasUp := f.built && f.backend.Backend == runtime.BackendPodman
	f.mu.Unlock()
	if !wasUp {
		return false
	}
	mm.InvalidateStatus()
	return mm.Status().Status == runtime.MachineStopped
}

// HoldMachineStopped records that the volunteer stopped the Podman machine
// (source: MachineStopFromApp or MachineStopOutside), so Build leaves it
// stopped until a person asks for it again (TB-88).
func (f *ContainerRuntimeFactory) HoldMachineStopped(source string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machineHeld = true
	f.machineStopSource = source
}

// ReleaseMachineHold lets Build bring the machine up again: the volunteer
// asked for it (the app's Start or Setup, a redetect request) or the machine
// came back by their hand.
func (f *ContainerRuntimeFactory) ReleaseMachineHold() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machineHeld = false
	f.machineStopSource = ""
}

// MachineHeldStopped reports whether the Podman machine is held stopped and
// where the stop came from.
func (f *ContainerRuntimeFactory) MachineHeldStopped() (held bool, source string) {
	if f == nil {
		return false, ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machineHeld, f.machineStopSource
}

func (f *ContainerRuntimeFactory) machineHeldStopped() bool {
	held, _ := f.MachineHeldStopped()
	return held
}

// recordResult notes a Build's outcome: the engine a runtime was built
// against (only on success — a later "nothing found" must not erase it), and
// the reason construction failed, if it did. A successful build releases a
// machine hold: the engine answers, so whoever stopped the machine has
// started it again.
func (f *ContainerRuntimeFactory) recordResult(backend runtime.BackendInfo, built bool, errStr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastErr = errStr
	if built {
		f.backend = backend
		f.built = true
		f.machineHeld = false
		f.machineStopSource = ""
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
	cr.SetCPUBudget(f.cfg.ResourceLimits.MaxCPUCores)
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
// the run context. While no container runtime is registered (and a head is
// trusted for one) it probes every containerRedetectInterval and on request;
// while one is registered it sleeps until woken. It used to exit for good
// once a runtime was registered, leaving an engine that later stopped
// answering to the runtime breaker — which re-probed by fetching a real unit
// and abandoning it, billed, every ten minutes (TB-80). Now an outage
// (NoteContainerEngineUnreachable) unregisters the runtime and wakes the loop,
// which probes with a ping until the engine answers and registers a fresh
// runtime.
func (d *Daemon) runContainerRedetect(ctx context.Context) {
	if d.containerFactory == nil || d.runtimeRegistry == nil {
		return
	}
	if d.containerRedetectActive() {
		d.logger.Info("no container runtime at start; re-checking for a container engine periodically — start Docker or the Podman machine and container work begins without a restart",
			"interval", containerRedetectInterval)
	}
	timer := time.NewTimer(containerRedetectInterval)
	defer timer.Stop()
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	for {
		if !d.containerRedetectActive() {
			// A runtime is registered (or no head trusts one): nothing to
			// probe. Sleep until an outage or a request wakes the loop.
			stopTimer()
			select {
			case <-ctx.Done():
				return
			case <-d.containerRedetectCh:
			}
			if !d.containerRedetectActive() {
				continue
			}
			// Woken with nothing registered — an outage took the runtime out
			// of service, or the volunteer stopped the machine: probe at once,
			// then on the cadence. Forced only for a person's explicit request
			// (containerRedetectForce): a Podman machine bring-up that failed
			// within its retry interval is not re-driven for an outage, and a
			// machine the volunteer stopped is not started (TB-88); the cheap
			// ping still runs every minute.
			if d.RedetectContainerRuntime(ctx, d.containerRedetectForce.Swap(false)) {
				continue
			}
			timer.Reset(containerRedetectInterval)
		}
		force := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-d.containerRedetectCh:
			// A wake. An explicit request (the API verb, a machine start that
			// just succeeded) sets containerRedetectForce: probe now and let a
			// machine bring-up run even inside its retry interval — the person
			// asking has usually just fixed it. A wake without the flag (an
			// outage, a machine stop) probes without the bring-up.
			force = d.containerRedetectForce.Swap(false)
			stopTimer()
		}
		if d.RedetectContainerRuntime(ctx, force) {
			continue
		}
		timer.Reset(containerRedetectInterval)
	}
}

// RequestContainerRedetect asks the loop to probe for an engine now instead of
// at its next tick. It returns an error when a probe is not applicable, so the
// caller can say why rather than wait for nothing. The probe is forced: a
// person is asking, so a machine bring-up runs even inside its retry interval
// and a machine the volunteer had stopped is started again (TB-88).
func (d *Daemon) RequestContainerRedetect() error {
	switch {
	case d.containerFactory == nil:
		return ErrContainerDetectUnavailable
	case d.runtimeRegistry != nil && d.runtimeRegistry.GetRuntime("container") != nil:
		return ErrContainerRuntimeRegistered
	case !d.containerRedetectActive():
		return ErrContainerNotTrusted
	}
	d.containerRedetectForce.Store(true)
	d.wakeContainerRedetect()
	return nil
}

// wakeContainerRedetect wakes the loop for a probe without forcing the
// machine bring-up (an outage, a machine stop).
func (d *Daemon) wakeContainerRedetect() {
	select {
	case d.containerRedetectCh <- struct{}{}:
	default: // a probe is already queued
	}
}

// WakeContainerRedetect brings the loop's next probe forward without forcing
// the machine bring-up. The management API's status route uses it when a
// held machine reads running again — the volunteer started it by hand — so
// the runtime is registered within a second instead of at the next tick,
// and the card does not read "not answering" for up to a minute over a
// machine that is up (TB-88).
func (d *Daemon) WakeContainerRedetect() {
	if d.containerFactory == nil || d.runtimeRegistry == nil {
		return
	}
	d.wakeContainerRedetect()
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
		held := errors.Is(err, ErrPodmanMachineHeldStopped)
		if err != nil {
			outcome = "error: " + err.Error()
		}
		if outcome != d.lastRedetectOutcome {
			switch {
			case held:
				d.noteMachineHeldStopped()
			case err != nil:
				d.logger.Warn("container engine found but its runtime could not be built; will keep re-checking",
					"backend", backend.Backend, "socket", backend.SocketPath, "error", err)
			default:
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
	// The runtime was built with the static whole-budget CPU grant and the
	// start-up memory ceiling; give it the daemon's live equal split (TB-75)
	// and live memory budget (TB-79).
	d.wireRuntimeCPU(rt)
	d.wireRuntimeMemory(rt)
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
	// A machine the volunteer had stopped is up again (by their hand, or by
	// their request through the app): the hold is released by the build, and
	// its notice is resolved here (TB-88).
	d.notices.Resolve("container_machine_stopped", "", "")
	if outage := d.endContainerOutage(); outage != nil {
		// The engine that stopped answering is back (TB-80): the notice the
		// outage raised is resolved, and the heads hear CONTAINER again on
		// their next contact (markRuntimesChanged below).
		d.logger.Info("container engine answering again; container work resumes",
			"backend", backend.Backend, "engine", backend.Engine, "version", backend.Version,
			"socket", backend.SocketPath, "runtimes", runtimes,
			"unreachable_for", time.Since(outage.since).Round(time.Second).String())
		d.notices.Resolve("container_engine_unreachable", "", "")
	} else {
		d.logger.Info("container runtime registered after start: this machine can now run container work",
			"backend", backend.Backend, "engine", backend.Engine, "version", backend.Version,
			"socket", backend.SocketPath, "runtimes", runtimes)
	}

	// The engine's VM may be smaller than the configured budgets (TB-63,
	// TB-75): lower the advertised figures BEFORE the heads are re-told, so
	// the re-registration and every later poll carry what the VM can hold.
	d.applyContainerMemoryBudget()
	d.applyContainerCPUBudget()
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

// --- an engine that was up and stopped answering (TB-80) ---

// containerOutage records a container engine that stopped answering under a
// running daemon: when, which engine, and the transport error it reported.
// It lives from NoteContainerEngineUnreachable until the next successful
// registration.
type containerOutage struct {
	since   time.Time
	backend runtime.BackendInfo
	err     string
}

// NoteContainerEngineUnreachable takes a container runtime whose engine has
// stopped answering out of service. rt is the runtime that reported the
// outage (the fetcher's Prepare, or a slot's Execute); it is unregistered only
// if it is still the registered one, so a stale report from a unit that was
// running when the engine died cannot remove a runtime a later probe built
// for the recovered engine. On the first report of an outage it records the
// outage, raises ONE container_engine_unreachable notice naming the socket
// and the remedy, flags every head for re-registration without CONTAINER,
// re-evaluates the no-runnable-leaf verdict, returns every buffered container
// unit to its head un-run (budget-neutral, TB-35) so none reaches a slot only
// to fail at create, and wakes the re-detection loop, which probes with a
// ping until the engine answers. It reports whether it took a runtime out of
// service. Safe to call from any goroutine.
func (d *Daemon) NoteContainerEngineUnreachable(rt runtime.Runtime, err error) bool {
	if d.runtimeRegistry == nil || rt == nil {
		return false
	}
	if !d.runtimeRegistry.Unregister(rt) {
		return false
	}
	// Name the engine: the detector's record of what it built, else what the
	// error itself says (the runtime stamps its backend and socket on it),
	// else what the runtime knows.
	backend, _ := d.containerFactory.Backend()
	var eu *runtime.EngineUnreachableError
	if backend.Backend == "" && errors.As(err, &eu) {
		backend = runtime.BackendInfo{Backend: eu.Backend, SocketPath: eu.Socket}
	}
	if backend.Backend == "" {
		if cr, ok := rt.(*runtime.ContainerRuntime); ok && cr != nil {
			backend = runtime.BackendInfo{Backend: cr.Backend(), SocketPath: cr.EngineSocket()}
		}
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	where := string(backend.Backend)
	if backend.SocketPath != "" {
		where += " at " + backend.SocketPath
	}
	if where == "" {
		where = "the container engine"
	}
	d.logger.Warn("container engine stopped answering: container work is paused, buffered container units are returned to their heads, and the engine is re-checked every minute until it answers",
		"backend", backend.Backend, "engine", backend.Engine, "socket", backend.SocketPath,
		"error", errText, "interval", containerRedetectInterval,
		"remedy", containerOutageRemedy(backend))
	d.notices.Notify(NoticeWarn, "container_engine_unreachable",
		fmt.Sprintf("The container engine (%s) is not answering: %s. Container work is paused and buffered container units have been returned to their heads un-run; Lettuce re-checks the engine every minute and resumes container work by itself when it answers. %s",
			where, errText, containerOutageRemedy(backend)),
		"", "")
	d.retireContainerRuntime(backend, errText)
	return true
}

// retireContainerRuntime is the part of taking a container runtime out of
// service that an engine outage (NoteContainerEngineUnreachable) and a machine
// the volunteer stopped (NotePodmanMachineStopped) share, once the runtime has
// left the registry: record the outage (the buffer sweep and the status route
// read it), flag every head for re-registration without CONTAINER,
// re-evaluate the no-runnable-leaf verdict, return every buffered container
// unit to its head un-run, and wake the re-detection loop.
func (d *Daemon) retireContainerRuntime(backend runtime.BackendInfo, errText string) {
	d.containerOutageMu.Lock()
	d.containerOutage = &containerOutage{since: time.Now(), backend: backend, err: errText}
	d.containerOutageMu.Unlock()

	d.markRuntimesChanged()
	d.refreshRuntimeBlocked()

	// Every buffered container unit would reach a slot only to fail at
	// create, run-started and billed; give them back now while they are
	// un-run. The sweep is the fetcher's own (unfitBuffered names the
	// reason), safe from any goroutine.
	d.mu.Lock()
	f := d.fetcher
	d.mu.Unlock()
	if f != nil {
		f.sweepBuffer()
	}

	d.wakeContainerRedetect()
}

// --- a Podman machine the volunteer stopped (TB-88) ---

// NotePodmanMachineStopped records that the volunteer is stopping the Podman
// machine through the app (the management API's stop verb, before the stop
// begins): the machine is held stopped so the re-detection loop will not
// start it again, and the container runtime leaves service at once — every
// buffered container unit goes back to its head un-run and the heads are
// re-told this machine's runtimes — rather than at the first unit to fail
// against the dead engine. The loop, woken, notes the hold: one Info line and
// the container_machine_stopped notice, in place of the outage WARN.
func (d *Daemon) NotePodmanMachineStopped(source string) {
	if d.containerFactory == nil {
		return
	}
	d.containerFactory.HoldMachineStopped(source)
	if d.runtimeRegistry == nil {
		return
	}
	rt := d.runtimeRegistry.GetRuntime("container")
	if rt == nil || !d.runtimeRegistry.Unregister(rt) {
		// Nothing in service (the machine was stopped during an outage, or
		// before its runtime came up): the loop's next probe notes the hold.
		return
	}
	backend, _ := d.containerFactory.Backend()
	d.logger.Info("podman machine is being stopped from the app: container work is paused and buffered container units are returned to their heads; Lettuce will not start the machine by itself",
		"backend", backend.Backend, "socket", backend.SocketPath)
	d.retireContainerRuntime(backend, "the Podman machine was stopped "+describeMachineStop(source))
}

// ReleasePodmanMachineHold lets the loop bring the machine up again: the
// volunteer asked for it (the app's Start or Setup button).
func (d *Daemon) ReleasePodmanMachineHold() {
	d.containerFactory.ReleaseMachineHold()
}

// PodmanMachineHeldStopped reports whether the Podman machine is left stopped
// on the volunteer's decision, and where the stop came from
// (MachineStopFromApp, MachineStopOutside). For the management API's status
// route, so the app's runtime card can say "stopped by you; Lettuce will not
// start it" instead of "unreachable, re-checking".
func (d *Daemon) PodmanMachineHeldStopped() (held bool, source string) {
	return d.containerFactory.MachineHeldStopped()
}

// noteMachineHeldStopped is the loop's one-time report of a hold (the outcome
// changed): an Info line — the volunteer's own decision, not a fault — and
// the container_machine_stopped notice, which supersedes the outage notice
// the stop may have raised first. A stop from outside the app is a WARN-level
// notice: nothing on the app's own screens confirmed it.
func (d *Daemon) noteMachineHeldStopped() {
	_, source := d.containerFactory.MachineHeldStopped()
	d.logger.Info("podman machine is stopped by the volunteer; Lettuce will not start it by itself — container work waits until it is started again",
		"stopped", describeMachineStop(source), "start_it", "the app's Start Machine button, 'podman machine start', or a Lettuce restart",
		"check_interval", containerRedetectInterval)
	d.notices.Resolve("container_engine_unreachable", "", "")
	level := NoticeInfo
	if source == MachineStopOutside {
		level = NoticeWarn
	}
	d.notices.Notify(level, "container_machine_stopped",
		fmt.Sprintf("The Podman machine was stopped %s. Lettuce will not start it by itself: container work waits until you start the machine again (Settings, runtime card, Start Machine; or 'podman machine start'), and Lettuce picks it up within a minute. Buffered container units were returned to their heads un-run; WASM and native work continues.",
			describeMachineStop(source)),
		"", "")
}

// containerOutageRemedy is the volunteer-facing hint for an engine that
// stopped answering, by engine kind.
func containerOutageRemedy(backend runtime.BackendInfo) string {
	switch backend.Backend {
	case runtime.BackendPodman:
		return "Check that the Podman machine is running ('podman machine ls', 'podman machine start') or that the Podman socket is enabled; a machine that reports running while its socket is dead is fixed by 'podman machine stop' and then 'podman machine start'."
	case runtime.BackendDocker:
		if backend.Engine == "podman" {
			return "Check that Podman Desktop's machine is running and its Docker compatibility is on."
		}
		return "Check that Docker (Docker Desktop, or the docker service) is running."
	default:
		return "Check that the container engine is running."
	}
}

// endContainerOutage clears the outage record and returns it (nil when none
// was recorded): the engine has answered a probe and its runtime is
// registered again.
func (d *Daemon) endContainerOutage() *containerOutage {
	d.containerOutageMu.Lock()
	defer d.containerOutageMu.Unlock()
	out := d.containerOutage
	d.containerOutage = nil
	return out
}

// ContainerOutage reports whether a container engine that was in service has
// stopped answering and is being re-probed, with the engine and the error
// it last reported. For the management API's status route, so the app's
// runtime card can say "unreachable, re-checking" rather than "running"
// (the machine's own claim) or "not installed".
func (d *Daemon) ContainerOutage() (backend runtime.BackendInfo, errText string, since time.Time, down bool) {
	d.containerOutageMu.Lock()
	defer d.containerOutageMu.Unlock()
	if d.containerOutage == nil {
		return runtime.BackendInfo{}, "", time.Time{}, false
	}
	return d.containerOutage.backend, d.containerOutage.err, d.containerOutage.since, true
}

// containerEngineDown reports whether a container-engine outage is in
// progress (recorded and not yet ended by a successful registration).
func (d *Daemon) containerEngineDown() bool {
	_, _, _, down := d.ContainerOutage()
	return down
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
