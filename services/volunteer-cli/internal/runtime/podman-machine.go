package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
)

// Sentinel errors for lifecycle operations.
var (
	ErrAlreadyRunning = errors.New("already running")
	ErrNotRunning     = errors.New("not running")
	ErrNotInitialized = errors.New("not initialized")
	ErrNotInstalled   = errors.New("not installed")
	// ErrMachineBusy: a start, stop or setup is already in progress and the
	// requested operation would only queue behind it (TB-87).
	ErrMachineBusy = errors.New("a machine operation is already in progress")
)

// MachineStatus represents the state of the Podman machine.
type MachineStatus string

const (
	MachineRunning        MachineStatus = "running"
	MachineStopped        MachineStatus = "stopped"
	MachineNotInitialized MachineStatus = "not_initialized"
	MachineNotInstalled   MachineStatus = "not_installed"
	MachineStarting       MachineStatus = "starting"
	// MachineStopping: a stop is in progress. Reported apart from starting so
	// the app's runtime card does not read "Starting..." while the machine is
	// on its way down (TB-87).
	MachineStopping MachineStatus = "stopping"
	MachineError    MachineStatus = "error"
)

// MachineInfo holds the current state of the Podman machine.
type MachineInfo struct {
	Status     MachineStatus
	Name       string // machine name (e.g., "default")
	CPUs       int
	MemoryMB   int
	DiskGB     int
	SocketPath string
	// Error is the machine's own error (an inspect that failed) or, when the
	// machine itself is fine, why the last start, stop or setup this manager
	// ran failed — so a failure of an asynchronous operation (TB-87) reaches
	// the caller that polls Status. Empty when there is nothing to report.
	Error string
}

// PodmanMachineManager manages the Podman machine lifecycle on Windows/macOS.
// On Linux, all operations are no-ops (rootless Podman doesn't need a machine).
type PodmanMachineManager struct {
	podmanBinary string
	logger       *slog.Logger
	mu           sync.Mutex // protects flags and cache
	opMu         sync.Mutex // serializes lifecycle operations (Init, Start, Stop, Setup)
	starting     bool
	initializing bool
	stopping     bool
	// pendingOp names an asynchronous operation (StartAsync, StopAsync,
	// SetupAsync) that has been accepted but whose goroutine has not yet taken
	// opMu — or is still running — so Status reports it as in progress from
	// the moment the caller was told "accepted", not from the moment the
	// command starts (TB-87). "start", "stop" or "setup"; empty otherwise.
	pendingOp string
	// lastOpErr is why the most recent init, start or stop failed, kept until
	// the next one succeeds or the machine is found in the state that
	// operation was for (settleLastOpErr). Status reports it as
	// MachineInfo.Error when the machine itself has no error to report, so a
	// caller that only polls Status (the app's runtime card after an
	// asynchronous verb) learns of the failure.
	lastOpErr string
	// lastFailedOp is the operation lastOpErr belongs to: "init", "start",
	// "setup" or "stop".
	lastFailedOp string

	// startedByThisProcess records whether THIS process actually issued the
	// successful `podman machine start` that brought the machine up (set on
	// startLocked success, cleared on stopLocked success). The machine is a
	// host-wide singleton shared with every other container on the box, so the
	// daemon's shutdown hook may stop it ONLY when the daemon itself started it
	// (PB-27) — Setup() no-ops idempotently on an already-running machine, and
	// "setup succeeded" must never be read as "we own the machine".
	startedByThisProcess bool

	// Status cache
	cachedInfo *MachineInfo
	cachedAt   time.Time
	cacheTTL   time.Duration // default 5s
	fetching   bool          // prevents cache stampede
}

// NewPodmanMachineManager creates a manager for the given Podman binary path.
func NewPodmanMachineManager(podmanBinary string, logger *slog.Logger) *PodmanMachineManager {
	return &PodmanMachineManager{
		podmanBinary: podmanBinary,
		logger:       logger,
		cacheTTL:     5 * time.Second,
	}
}

// NeedsMachine returns true if the current platform requires a Podman machine (VM).
// Returns true on Windows and macOS, false on Linux.
func (m *PodmanMachineManager) NeedsMachine() bool {
	return needsMachine()
}

// needsMachine is the platform check. A variable so a test on a Linux CI
// runner can put the manager on the machine path (SetNeedsMachineForTest) and
// drive `podman machine` through the mocked CommandExecutor.
var needsMachine = func() bool {
	return goruntime.GOOS == "windows" || goruntime.GOOS == "darwin"
}

// NeedsMachineForTest exposes needsMachine for use in tests in other packages.
func NeedsMachineForTest() bool {
	return needsMachine()
}

// SetNeedsMachineForTest overrides the platform check for tests in any
// package and returns the function that restores it.
func SetNeedsMachineForTest(v bool) (restore func()) {
	orig := needsMachine
	needsMachine = func() bool { return v }
	return func() { needsMachine = orig }
}

// ContainerEngineRunsInVM reports whether this platform's container engine
// runs inside a virtual machine whose memory, not the host's, bounds every
// container: Windows and macOS, where both Podman (its machine) and Docker
// Desktop (its engine VM) do. On Linux containers share the host's RAM and the
// configured budget stands as it is. A variable so tests can put a Linux CI
// runner on the VM path and vice versa (TB-63).
var ContainerEngineRunsInVM = func() bool {
	return needsMachine()
}

// Status checks the current Podman machine state.
// On Linux: returns Running if Podman binary exists, NotInstalled otherwise.
// On Windows/macOS: runs `podman machine inspect` and parses the output.
func (m *PodmanMachineManager) Status() MachineInfo {
	return m.status(true)
}

// status is Status with a choice about the asynchronous accept marker:
// callers outside the manager see an accepted operation as in progress
// (withPending); the operation itself (Setup, deciding what the machine
// needs) must see the machine's real state, not its own marker.
func (m *PodmanMachineManager) status(withPending bool) MachineInfo {
	m.mu.Lock()

	// Return transitional status if any operation is in progress — accepted
	// (pendingOp) or running. A stop reports as stopping; everything else
	// (init, start, setup) as starting.
	if m.stopping || (withPending && m.pendingOp == "stop") {
		m.mu.Unlock()
		return MachineInfo{Status: MachineStopping}
	}
	if m.starting || m.initializing || (withPending && m.pendingOp != "") {
		m.mu.Unlock()
		return MachineInfo{Status: MachineStarting}
	}

	// Check cache.
	if m.cachedInfo != nil && time.Since(m.cachedAt) < m.cacheTTL {
		info := m.withLastOpError(*m.cachedInfo)
		m.mu.Unlock()
		return info
	}

	// Prevent stampede: if another goroutine is fetching, return stale cache or starting.
	if m.fetching {
		if m.cachedInfo != nil {
			info := m.withLastOpError(*m.cachedInfo)
			m.mu.Unlock()
			return info
		}
		m.mu.Unlock()
		return MachineInfo{Status: MachineStarting}
	}
	m.fetching = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.fetching = false
		m.mu.Unlock()
	}()

	// Check if podman binary exists.
	if m.podmanBinary == "" {
		return MachineInfo{Status: MachineNotInstalled}
	}

	var info MachineInfo
	// On Linux, no machine needed — just check if the binary works.
	if !needsMachine() {
		info = m.linuxStatus()
	} else {
		info = m.machineStatus()
	}

	// Cache the result.
	m.mu.Lock()
	m.cachedInfo = &info
	m.cachedAt = time.Now()
	m.settleLastOpErr(info.Status)
	info = m.withLastOpError(info)
	m.mu.Unlock()

	return info
}

// withLastOpError fills in the last failed operation's error when the machine
// itself reported none. Called with mu held.
func (m *PodmanMachineManager) withLastOpError(info MachineInfo) MachineInfo {
	if info.Error == "" && m.lastOpErr != "" {
		info.Error = m.lastOpErr
	}
	return info
}

// settleLastOpErr drops the last failed operation's error once the machine is
// found in the state that operation was for: running after a failed init,
// start or setup, stopped after a failed stop. A start podman gave up on, or
// that Lettuce stopped waiting for, can still bring the machine up, and a
// machine can be started or stopped by hand; either way the error no longer
// describes the machine, and shown beside a running engine it read as a
// fault. An error beside a machine still in the other state stands. Called
// with mu held.
func (m *PodmanMachineManager) settleLastOpErr(status MachineStatus) {
	if m.lastOpErr == "" {
		return
	}
	stop := m.lastFailedOp == "stop"
	if (status == MachineRunning && !stop) || (status == MachineStopped && stop) {
		m.lastOpErr = ""
		m.lastFailedOp = ""
	}
}

// InvalidateStatus drops the cached machine state so the next Status runs a
// fresh `podman machine inspect`. The daemon's re-detection uses it before
// deciding whether a machine that was running under it has been stopped
// (TB-88): the cache may still hold the state from before the stop.
func (m *PodmanMachineManager) InvalidateStatus() {
	m.mu.Lock()
	m.cachedInfo = nil
	m.mu.Unlock()
}

// linuxStatus checks Podman availability on Linux (no VM required).
func (m *PodmanMachineManager) linuxStatus() MachineInfo {
	_, err := CommandExecutor(m.podmanBinary, "--version")
	if err != nil {
		return MachineInfo{Status: MachineNotInstalled}
	}

	return MachineInfo{
		Status:     MachineRunning,
		SocketPath: podmanSocketPath(m.podmanBinary),
	}
}

// podmanMachineInspectResult represents the relevant fields from `podman machine inspect`.
type podmanMachineInspectResult struct {
	Name      string `json:"Name"`
	State     string `json:"State"`
	Resources struct {
		CPUs int `json:"CPUs"`
		// Memory and DiskSize are printed in MiB and GiB by Podman 5 (the
		// machine package types them strongunits.MiB / GiB) and in bytes by
		// Podman 4, which this parser was first written against; see
		// inspectMemoryMB / inspectDiskGB (TB-86).
		Memory   int `json:"Memory"`
		DiskSize int `json:"DiskSize"`
	} `json:"Resources"`
	ConnectionInfo struct {
		PodmanSocket *struct {
			Path string `json:"Path"`
		} `json:"PodmanSocket"`
		PodmanPipe *struct {
			Path string `json:"Path"`
		} `json:"PodmanPipe"`
	} `json:"ConnectionInfo"`
}

// machineStatus probes `podman machine inspect` on Windows/macOS.
func (m *PodmanMachineManager) machineStatus() MachineInfo {
	out, err := CommandExecutor(m.podmanBinary, "machine", "inspect")
	if err != nil {
		errStr := string(out) + err.Error()
		if strings.Contains(strings.ToLower(errStr), "no vm") ||
			strings.Contains(strings.ToLower(errStr), "does not exist") ||
			strings.Contains(strings.ToLower(errStr), "no machine") {
			return MachineInfo{Status: MachineNotInitialized}
		}
		return MachineInfo{
			Status: MachineError,
			Error:  fmt.Sprintf("podman machine inspect failed: %v", err),
		}
	}

	// podman machine inspect returns a JSON array.
	var results []podmanMachineInspectResult
	if err := json.Unmarshal(out, &results); err != nil {
		return MachineInfo{
			Status: MachineError,
			Error:  fmt.Sprintf("parsing machine inspect output: %v", err),
		}
	}

	if len(results) == 0 {
		return MachineInfo{Status: MachineNotInitialized}
	}

	r := results[0]
	info := MachineInfo{
		Name:     r.Name,
		CPUs:     r.Resources.CPUs,
		MemoryMB: inspectMemoryMB(r.Resources.Memory),
		DiskGB:   inspectDiskGB(r.Resources.DiskSize),
	}

	// Resolve socket path.
	if r.ConnectionInfo.PodmanSocket != nil {
		info.SocketPath = r.ConnectionInfo.PodmanSocket.Path
	} else if r.ConnectionInfo.PodmanPipe != nil {
		info.SocketPath = r.ConnectionInfo.PodmanPipe.Path
	}

	switch strings.ToLower(r.State) {
	case "running":
		info.Status = MachineRunning
	case "stopped":
		info.Status = MachineStopped
	case "starting":
		// A machine that is still booting. Not stopped: the daemon holds a
		// machine it finds stopped as one the volunteer stopped, and must
		// wait for a booting one instead.
		info.Status = MachineStarting
	default:
		info.Status = MachineStopped
	}

	return info
}

// inspectUnitsBytesAbove is the value above which an inspect figure is read
// as bytes rather than MiB/GiB: no machine has a million MiB of memory or a
// million GiB of disk, and a byte count below one MiB is not a machine size
// either. Podman 5 prints `"Memory": 6144, "DiskSize": 100` (MiB, GiB);
// Podman 4 printed the same machine as 6442450944 and 107374182400 (bytes).
// Dividing the Podman 5 figures as bytes gave every current Podman a card
// reading "0 MiB RAM, 0 GiB disk" (TB-86).
const inspectUnitsBytesAbove = 1 << 20

// inspectMemoryMB converts `podman machine inspect`'s Resources.Memory to
// MiB: bytes when the figure is too large to be a MiB count, else MiB as is.
func inspectMemoryMB(v int) int {
	if v > inspectUnitsBytesAbove {
		return v / (1024 * 1024)
	}
	return v
}

// inspectDiskGB converts Resources.DiskSize to GiB the same way.
func inspectDiskGB(v int) int {
	if v > inspectUnitsBytesAbove {
		return v / (1024 * 1024 * 1024)
	}
	return v
}

// Init initializes a new Podman machine with the given resources.
// No-op on Linux.
func (m *PodmanMachineManager) Init(cpus, memoryMB, diskGB int) error {
	if !needsMachine() {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.initLocked(cpus, memoryMB, diskGB)
}

func (m *PodmanMachineManager) initLocked(cpus, memoryMB, diskGB int) error {
	m.mu.Lock()
	m.initializing = true
	m.cachedInfo = nil
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.initializing = false
		m.mu.Unlock()
	}()

	m.logger.Info("initializing podman machine", "cpus", cpus, "memory_mb", memoryMB, "disk_gb", diskGB)

	args := []string{
		"machine", "init",
		fmt.Sprintf("--cpus=%d", cpus),
		fmt.Sprintf("--memory=%d", memoryMB),
		fmt.Sprintf("--disk-size=%d", diskGB),
	}

	out, err := CommandExecutor(m.podmanBinary, args...)
	if err != nil {
		return m.recordOp("init", fmt.Errorf("podman machine init failed: %s: %w", strings.TrimSpace(string(out)), err))
	}

	m.logger.Info("podman machine initialized")
	return m.recordOp("init", nil)
}

// recordOp notes an operation's outcome for Status (lastOpErr, and the
// operation it belongs to) and returns the error unchanged.
func (m *PodmanMachineManager) recordOp(op string, err error) error {
	m.mu.Lock()
	if err != nil {
		m.lastOpErr = err.Error()
		m.lastFailedOp = op
	} else {
		m.lastOpErr = ""
		m.lastFailedOp = ""
	}
	m.mu.Unlock()
	return err
}

// Start starts the Podman machine.
// On Linux: verifies the Podman socket is accessible.
func (m *PodmanMachineManager) Start() error {
	if !needsMachine() {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.startLocked()
}

func (m *PodmanMachineManager) startLocked() error {
	m.mu.Lock()
	m.starting = true
	m.cachedInfo = nil
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.starting = false
		m.mu.Unlock()
	}()

	m.logger.Info("starting podman machine")

	out, err := CommandExecutor(m.podmanBinary, "machine", "start")
	if err != nil {
		startErr := fmt.Errorf("podman machine start failed: %s: %w", strings.TrimSpace(string(out)), err)
		// A start can end in an error — podman's own, or its bound — after the
		// machine has come up. A running machine is what the start was for.
		if m.machineStatus().Status != MachineRunning {
			return m.recordOp("start", startErr)
		}
		// Ownership only from a start this process stopped at its bound:
		// podman was then part-way through bringing the machine up. A start
		// podman ended by itself may have been its refusal of a machine
		// somebody else already had running, and a machine left running at
		// shutdown is the safe side of that doubt.
		owned := errors.Is(err, errMachineVerbTimedOut)
		m.logger.Warn("podman machine start reported a failure, but the machine is running; counting it as started",
			"error", startErr, "started_by_this_process", owned)
		if owned {
			m.mu.Lock()
			m.startedByThisProcess = true
			m.mu.Unlock()
		}
		return m.recordOp("start", nil)
	}

	m.mu.Lock()
	m.startedByThisProcess = true
	m.mu.Unlock()

	m.logger.Info("podman machine started")
	return m.recordOp("start", nil)
}

// Stop stops the Podman machine.
// On Linux: no-op.
func (m *PodmanMachineManager) Stop() error {
	if !needsMachine() {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stopLocked()
}

func (m *PodmanMachineManager) stopLocked() error {
	m.mu.Lock()
	m.stopping = true
	m.cachedInfo = nil
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.stopping = false
		m.mu.Unlock()
	}()

	m.logger.Info("stopping podman machine")

	out, err := CommandExecutor(m.podmanBinary, "machine", "stop")
	if err != nil {
		stopErr := fmt.Errorf("podman machine stop failed: %s: %w", strings.TrimSpace(string(out)), err)
		// As for a start: a stop that ends in an error after the machine went
		// down did what it was for. Reported as a failure, the app's stop hands
		// the machine back to the re-detection loop, which starts it again.
		if m.machineStatus().Status != MachineStopped {
			return m.recordOp("stop", stopErr)
		}
		m.logger.Warn("podman machine stop reported a failure, but the machine is stopped; counting it as stopped", "error", stopErr)
	} else {
		m.logger.Info("podman machine stopped")
	}

	// The machine is down; this process no longer owns a start it should undo.
	m.mu.Lock()
	m.startedByThisProcess = false
	m.mu.Unlock()

	return m.recordOp("stop", nil)
}

// --- asynchronous verbs (TB-87) ---
//
// `podman machine start` and `stop` take 30-120 s on an ordinary Intel Mac
// (see machineStartTimeout) and can hang outright (podman #25121, #29074).
// The management API used to run them to completion inside one request, and
// the app's 15 s client gave up with "daemon unreachable" while the machine
// was in fact starting. Each verb now returns as soon as the operation is
// accepted; Status reports starting / stopping meanwhile and, on failure,
// carries the error until the next operation succeeds or the machine reaches
// the state the failed one was for (settleLastOpErr). A caller learns the
// outcome by polling Status (the app's runtime card) or through the done
// callback (the daemon bridge, which registers the runtime after a
// successful start).

// StartAsync runs Start on its own goroutine. ErrMachineBusy when another
// asynchronous operation is still in flight. done, if not nil, is called with
// Start's result when it finishes.
func (m *PodmanMachineManager) StartAsync(done func(error)) error {
	return m.runAsync("start", done, m.Start)
}

// StopAsync is StartAsync for Stop.
func (m *PodmanMachineManager) StopAsync(done func(error)) error {
	return m.runAsync("stop", done, m.Stop)
}

// SetupAsync is StartAsync for Setup.
func (m *PodmanMachineManager) SetupAsync(cpus, memoryMB, diskGB int, done func(error)) error {
	return m.runAsync("setup", done, func() error { return m.Setup(cpus, memoryMB, diskGB) })
}

func (m *PodmanMachineManager) runAsync(op string, done func(error), run func() error) error {
	m.mu.Lock()
	if m.pendingOp != "" {
		m.mu.Unlock()
		return ErrMachineBusy
	}
	m.pendingOp = op
	m.cachedInfo = nil
	m.mu.Unlock()
	go func() {
		err := run()
		// Setup's own refusals (not installed, an inspect error) do not pass
		// through recordOp; record them here so Status carries them too.
		m.recordOp(op, err)
		if err != nil {
			m.logger.Warn("podman machine operation failed", "operation", op, "error", err)
		}
		m.mu.Lock()
		m.pendingOp = ""
		m.mu.Unlock()
		if done != nil {
			done(err)
		}
	}()
	return nil
}

// StartedByThisProcess reports whether this process issued the successful
// `podman machine start` that brought the machine up (and has not stopped it
// since). The daemon's shutdown hook consults this so it never stops a machine
// somebody else was already running (PB-27).
func (m *PodmanMachineManager) StartedByThisProcess() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startedByThisProcess
}

// Setup is the full initialization flow: Init (if not initialized) + Start.
// Idempotent — no-op if already running (in which case this process does NOT
// become the machine's owner; see StartedByThisProcess).
func (m *PodmanMachineManager) Setup(cpus, memoryMB, diskGB int) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	status := m.status(false)

	switch status.Status {
	case MachineRunning:
		m.logger.Info("podman machine already running, skipping setup")
		return nil
	case MachineNotInstalled:
		return fmt.Errorf("podman is not installed")
	case MachineError:
		return fmt.Errorf("podman machine error: %s", status.Error)
	case MachineNotInitialized:
		if err := m.initLocked(cpus, memoryMB, diskGB); err != nil {
			return err
		}
		return m.startLocked()
	case MachineStopped:
		return m.startLocked()
	case MachineStarting, MachineStopping:
		m.logger.Info("podman machine has an operation in progress; not starting it again", "status", status.Status)
		return nil
	default:
		return m.startLocked()
	}
}

// WaitForReady polls the Podman socket until it responds or timeout is reached.
func (m *PodmanMachineManager) WaitForReady(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	deadline := time.Now().Add(timeout)
	interval := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		out, err := CommandExecutor(m.podmanBinary, "info", "--format", "{{.Host.RemoteSocket.Exists}}")
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			return nil
		}

		time.Sleep(interval)
	}

	return fmt.Errorf("podman not ready after %s", timeout)
}

// parseVersionOutput extracts the version from "podman version X.Y.Z" output.
func parseVersionOutput(output string) string {
	s := strings.TrimSpace(output)
	if idx := strings.LastIndex(s, " "); idx >= 0 {
		return strings.TrimSpace(s[idx+1:])
	}
	return s
}
