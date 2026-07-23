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
)

// MachineStatus represents the state of the Podman machine.
type MachineStatus string

const (
	MachineRunning        MachineStatus = "running"
	MachineStopped        MachineStatus = "stopped"
	MachineNotInitialized MachineStatus = "not_initialized"
	MachineNotInstalled   MachineStatus = "not_installed"
	MachineStarting       MachineStatus = "starting"
	MachineError          MachineStatus = "error"
)

// MachineInfo holds the current state of the Podman machine.
type MachineInfo struct {
	Status     MachineStatus
	Name       string // machine name (e.g., "default")
	CPUs       int
	MemoryMB   int
	DiskGB     int
	SocketPath string
	Error      string // last error, empty if no error
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

// needsMachine is the platform check, extracted for testability.
func needsMachine() bool {
	return goruntime.GOOS == "windows" || goruntime.GOOS == "darwin"
}

// NeedsMachineForTest exposes needsMachine for use in tests in other packages.
func NeedsMachineForTest() bool {
	return needsMachine()
}

// Status checks the current Podman machine state.
// On Linux: returns Running if Podman binary exists, NotInstalled otherwise.
// On Windows/macOS: runs `podman machine inspect` and parses the output.
func (m *PodmanMachineManager) Status() MachineInfo {
	m.mu.Lock()

	// Return transitional status if any operation is in progress.
	if m.starting || m.initializing || m.stopping {
		m.mu.Unlock()
		return MachineInfo{Status: MachineStarting}
	}

	// Check cache.
	if m.cachedInfo != nil && time.Since(m.cachedAt) < m.cacheTTL {
		info := *m.cachedInfo
		m.mu.Unlock()
		return info
	}

	// Prevent stampede: if another goroutine is fetching, return stale cache or starting.
	if m.fetching {
		if m.cachedInfo != nil {
			info := *m.cachedInfo
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
	m.mu.Unlock()

	return info
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
		CPUs     int `json:"CPUs"`
		Memory   int `json:"Memory"`   // bytes
		DiskSize int `json:"DiskSize"` // bytes
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
		MemoryMB: r.Resources.Memory / (1024 * 1024),
		DiskGB:   r.Resources.DiskSize / (1024 * 1024 * 1024),
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
	default:
		info.Status = MachineStopped
	}

	return info
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
		return fmt.Errorf("podman machine init failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	m.logger.Info("podman machine initialized")
	return nil
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
		return fmt.Errorf("podman machine start failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	m.mu.Lock()
	m.startedByThisProcess = true
	m.mu.Unlock()

	m.logger.Info("podman machine started")
	return nil
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
		return fmt.Errorf("podman machine stop failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// The machine is down; this process no longer owns a start it should undo.
	m.mu.Lock()
	m.startedByThisProcess = false
	m.mu.Unlock()

	m.logger.Info("podman machine stopped")
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

	status := m.Status()

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
	case MachineStarting:
		m.logger.Info("podman machine is already starting")
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
