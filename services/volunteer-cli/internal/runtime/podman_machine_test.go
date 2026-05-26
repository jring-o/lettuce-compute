package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *PodmanMachineManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewPodmanMachineManager("/usr/bin/podman", logger)
}

func TestNeedsMachine(t *testing.T) {
	result := needsMachine()
	switch runtime.GOOS {
	case "windows", "darwin":
		if !result {
			t.Error("expected NeedsMachine() = true on", runtime.GOOS)
		}
	case "linux":
		if result {
			t.Error("expected NeedsMachine() = false on linux")
		}
	}
}

func TestPodmanMachineManager_NeedsMachine(t *testing.T) {
	m := newTestManager(t)
	got := m.NeedsMachine()
	expected := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if got != expected {
		t.Errorf("NeedsMachine() = %v, expected %v on %s", got, expected, runtime.GOOS)
	}
}

func TestStatus_LinuxPodmanAvailable(t *testing.T) {
	if needsMachine() {
		t.Skip("test only runs on Linux")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineRunning {
		t.Errorf("expected MachineRunning, got %s", info.Status)
	}
}

func TestStatus_LinuxPodmanNotFound(t *testing.T) {
	if needsMachine() {
		t.Skip("test only runs on Linux")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewPodmanMachineManager("", logger)
	info := m.Status()
	if info.Status != MachineNotInstalled {
		t.Errorf("expected MachineNotInstalled, got %s", info.Status)
	}
}

func TestStatus_MachineRunning(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {
					"PodmanPipe": {"Path": "//./pipe/podman-machine-default"}
				}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineRunning {
		t.Errorf("expected MachineRunning, got %s", info.Status)
	}
	if info.Name != "default" {
		t.Errorf("expected name 'default', got %s", info.Name)
	}
	if info.CPUs != 4 {
		t.Errorf("expected 4 CPUs, got %d", info.CPUs)
	}
	if info.MemoryMB != 8192 {
		t.Errorf("expected 8192 MB, got %d", info.MemoryMB)
	}
	if info.DiskGB != 20 {
		t.Errorf("expected 20 GB, got %d", info.DiskGB)
	}
}

func TestStatus_MachineStopped(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "stopped",
				"Resources": {"CPUs": 2, "Memory": 4294967296, "DiskSize": 10737418240},
				"ConnectionInfo": {
					"PodmanSocket": {"Path": "/tmp/podman.sock"}
				}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineStopped {
		t.Errorf("expected MachineStopped, got %s", info.Status)
	}
	if info.CPUs != 2 {
		t.Errorf("expected 2 CPUs, got %d", info.CPUs)
	}
}

func TestStatus_NotInitialized(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineNotInitialized {
		t.Errorf("expected MachineNotInitialized, got %s", info.Status)
	}
}

func TestStatus_Starting(t *testing.T) {
	m := newTestManager(t)

	// Simulate starting state.
	m.mu.Lock()
	m.starting = true
	m.mu.Unlock()

	info := m.Status()
	if info.Status != MachineStarting {
		t.Errorf("expected MachineStarting, got %s", info.Status)
	}

	m.mu.Lock()
	m.starting = false
	m.mu.Unlock()
}

func TestInit(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	var capturedArgs []string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		capturedArgs = args
		return []byte("Machine init complete\n"), nil
	})

	err := m.Init(4, 8192, 20)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	expected := []string{"machine", "init", "--cpus=4", "--memory=8192", "--disk-size=20"}
	if len(capturedArgs) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, capturedArgs)
	}
	for i, arg := range expected {
		if capturedArgs[i] != arg {
			t.Errorf("arg[%d]: expected %q, got %q", i, arg, capturedArgs[i])
		}
	}
}

func TestInit_LinuxNoOp(t *testing.T) {
	if needsMachine() {
		t.Skip("test only runs on Linux")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		t.Error("Init should not execute commands on Linux")
		return nil, exec.ErrNotFound
	})

	err := m.Init(4, 8192, 20)
	if err != nil {
		t.Fatalf("Init should be no-op on Linux, got: %v", err)
	}
}

func TestStart(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	var capturedCmd string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		capturedCmd = name + " " + strings.Join(args, " ")
		return []byte("Machine started\n"), nil
	})

	err := m.Start()
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !strings.Contains(capturedCmd, "machine start") {
		t.Errorf("expected 'machine start' in command, got %q", capturedCmd)
	}
}

func TestStop(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	var capturedCmd string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		capturedCmd = name + " " + strings.Join(args, " ")
		return []byte("Machine stopped\n"), nil
	})

	err := m.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if !strings.Contains(capturedCmd, "machine stop") {
		t.Errorf("expected 'machine stop' in command, got %q", capturedCmd)
	}
}

func TestSetup_FullFlow(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	var cmds []string
	callCount := 0
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		cmds = append(cmds, cmd)
		callCount++

		// First call: machine inspect -> not initialized
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
		}
		// Init and Start succeed
		if len(args) >= 2 && args[0] == "machine" && (args[1] == "init" || args[1] == "start") {
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	err := m.Setup(4, 8192, 20)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	// Should have called: inspect, init, start
	hasInit := false
	hasStart := false
	for _, c := range cmds {
		if strings.Contains(c, "machine init") {
			hasInit = true
		}
		if strings.Contains(c, "machine start") {
			hasStart = true
		}
	}
	if !hasInit {
		t.Error("expected machine init to be called")
	}
	if !hasStart {
		t.Error("expected machine start to be called")
	}
}

func TestSetup_AlreadyRunning(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	initCalled := false
	startCalled := false
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "init" {
			initCalled = true
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			startCalled = true
		}
		return nil, exec.ErrNotFound
	})

	err := m.Setup(4, 8192, 20)
	if err != nil {
		t.Fatalf("Setup should succeed when already running: %v", err)
	}
	if initCalled {
		t.Error("Init should not be called when already running")
	}
	if startCalled {
		t.Error("Start should not be called when already running")
	}
}

func TestWaitForReady_ImmediateSuccess(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "info" {
			return []byte("true"), nil
		}
		return nil, exec.ErrNotFound
	})

	err := m.WaitForReady(5 * time.Second)
	if err != nil {
		t.Fatalf("WaitForReady failed: %v", err)
	}
}

func TestWaitForReady_Timeout(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("connection refused")
	})

	err := m.WaitForReady(1 * time.Second)
	if err == nil {
		t.Fatal("WaitForReady should have timed out")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("expected 'not ready' in error, got: %v", err)
	}
}

func TestStatus_EmptyBinary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewPodmanMachineManager("", logger)
	info := m.Status()
	if info.Status != MachineNotInstalled {
		t.Errorf("expected MachineNotInstalled with empty binary, got %s", info.Status)
	}
}

func TestStatus_MachineInspectInvalidJSON(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("not valid json at all"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineError {
		t.Errorf("expected MachineError for invalid JSON, got %s", info.Status)
	}
	if !strings.Contains(info.Error, "parsing machine inspect output") {
		t.Errorf("expected parse error message, got %q", info.Error)
	}
}

func TestStatus_MachineInspectEmptyArray(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("[]"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineNotInitialized {
		t.Errorf("expected MachineNotInitialized for empty array, got %s", info.Status)
	}
}

func TestStatus_MachineInspectUnknownState(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "transitioning",
				"Resources": {"CPUs": 2, "Memory": 4294967296, "DiskSize": 10737418240},
				"ConnectionInfo": {}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	// Unknown states default to MachineStopped.
	if info.Status != MachineStopped {
		t.Errorf("expected MachineStopped for unknown state, got %s", info.Status)
	}
}

func TestInit_Failure(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return []byte("machine already exists\n"), fmt.Errorf("exit status 125")
	})

	err := m.Init(4, 8192, 20)
	if err == nil {
		t.Fatal("Init should fail when command returns error")
	}
	if !strings.Contains(err.Error(), "podman machine init failed") {
		t.Errorf("expected 'podman machine init failed' in error, got %q", err.Error())
	}
}

func TestStart_Failure(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return []byte("cannot start\n"), fmt.Errorf("exit status 1")
	})

	err := m.Start()
	if err == nil {
		t.Fatal("Start should fail when command returns error")
	}
	if !strings.Contains(err.Error(), "podman machine start failed") {
		t.Errorf("expected 'podman machine start failed' in error, got %q", err.Error())
	}
}

func TestStop_Failure(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return []byte("cannot stop\n"), fmt.Errorf("exit status 1")
	})

	err := m.Stop()
	if err == nil {
		t.Fatal("Stop should fail when command returns error")
	}
	if !strings.Contains(err.Error(), "podman machine stop failed") {
		t.Errorf("expected 'podman machine stop failed' in error, got %q", err.Error())
	}
}

func TestSetup_MachineStopped(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	initCalled := false
	startCalled := false
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "stopped",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "init" {
			initCalled = true
			return []byte("OK\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			startCalled = true
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	err := m.Setup(4, 8192, 20)
	if err != nil {
		t.Fatalf("Setup should succeed when machine is stopped: %v", err)
	}
	if initCalled {
		t.Error("Init should not be called when machine is stopped (only start)")
	}
	if !startCalled {
		t.Error("Start should be called when machine is stopped")
	}
}

func TestSetup_MachineError(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			// Return a generic error (not "no VM" / "does not exist")
			return []byte("unexpected error"), fmt.Errorf("exit status 1")
		}
		return nil, exec.ErrNotFound
	})

	err := m.Setup(4, 8192, 20)
	if err == nil {
		t.Fatal("Setup should fail when machine is in error state")
	}
	if !strings.Contains(err.Error(), "podman machine error") {
		t.Errorf("expected 'podman machine error' in error, got %q", err.Error())
	}
}

func TestSetup_NotInstalled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewPodmanMachineManager("", logger)

	err := m.Setup(4, 8192, 20)
	if err == nil {
		t.Fatal("Setup should fail when podman is not installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("expected 'not installed' in error, got %q", err.Error())
	}
}

func TestSetup_Starting(t *testing.T) {
	m := newTestManager(t)
	// Simulate starting state.
	m.mu.Lock()
	m.starting = true
	m.mu.Unlock()

	err := m.Setup(4, 8192, 20)
	if err != nil {
		t.Fatalf("Setup should succeed (no-op) when machine is already starting: %v", err)
	}

	m.mu.Lock()
	m.starting = false
	m.mu.Unlock()
}

func TestWaitForReady_NoVersionFallback(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		// info command fails
		if len(args) >= 1 && args[0] == "info" {
			return nil, fmt.Errorf("connection refused")
		}
		// version command succeeds — but should NOT be tried as fallback
		if len(args) >= 1 && args[0] == "version" {
			t.Error("WaitForReady should not fall back to podman version")
			return []byte("4.9.0"), nil
		}
		return nil, fmt.Errorf("unknown command")
	})

	err := m.WaitForReady(1 * time.Second)
	if err == nil {
		t.Fatal("WaitForReady should time out when info fails (no version fallback)")
	}
}

func TestWaitForReady_DefaultTimeout(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "info" {
			return []byte("true"), nil
		}
		return nil, fmt.Errorf("unknown")
	})

	// Passing 0 should use default 60s timeout, but succeed immediately.
	err := m.WaitForReady(0)
	if err != nil {
		t.Fatalf("WaitForReady with 0 timeout should use default and succeed: %v", err)
	}
}

func TestStart_LinuxNoOp(t *testing.T) {
	if needsMachine() {
		t.Skip("test only runs on Linux")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		t.Error("Start should not execute commands on Linux")
		return nil, exec.ErrNotFound
	})

	err := m.Start()
	if err != nil {
		t.Fatalf("Start should be no-op on Linux, got: %v", err)
	}
}

func TestStop_LinuxNoOp(t *testing.T) {
	if needsMachine() {
		t.Skip("test only runs on Linux")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		t.Error("Stop should not execute commands on Linux")
		return nil, exec.ErrNotFound
	})

	err := m.Stop()
	if err != nil {
		t.Fatalf("Stop should be no-op on Linux, got: %v", err)
	}
}

func TestStatus_MachineDoesNotExist(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: does not exist"), fmt.Errorf("exit status 125")
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineNotInitialized {
		t.Errorf("expected MachineNotInitialized for 'does not exist', got %s", info.Status)
	}
}

func TestStatus_NoMachineError(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: no machine found"), fmt.Errorf("exit status 125")
		}
		return nil, exec.ErrNotFound
	})

	info := m.Status()
	if info.Status != MachineNotInitialized {
		t.Errorf("expected MachineNotInitialized for 'no machine', got %s", info.Status)
	}
}

func TestParseVersionOutput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"podman version 4.9.0\n", "4.9.0"},
		{"podman version 5.0.0-rc1", "5.0.0-rc1"},
		{"4.9.0", "4.9.0"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseVersionOutput(tt.input)
		if got != tt.expected {
			t.Errorf("parseVersionOutput(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestStatus_Cache(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	callCount := 0
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			callCount++
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	// First call — should shell out.
	info1 := m.Status()
	if info1.Status != MachineRunning {
		t.Fatalf("expected MachineRunning, got %s", info1.Status)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 shell-out, got %d", callCount)
	}

	// Second call within TTL — should use cache.
	info2 := m.Status()
	if info2.Status != MachineRunning {
		t.Fatalf("expected MachineRunning from cache, got %s", info2.Status)
	}
	if callCount != 1 {
		t.Errorf("expected cache hit (still 1 shell-out), got %d", callCount)
	}
}

func TestStatus_CacheInvalidatedByStart(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	inspectCount := 0
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			inspectCount++
			return []byte(`[{
				"Name": "default",
				"State": "stopped",
				"Resources": {"CPUs": 2, "Memory": 4294967296, "DiskSize": 10737418240},
				"ConnectionInfo": {}
			}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	// Populate cache.
	m.Status()
	if inspectCount != 1 {
		t.Fatalf("expected 1 inspect, got %d", inspectCount)
	}

	// Start invalidates cache.
	m.Start()

	// Next Status should re-probe.
	m.Status()
	if inspectCount != 2 {
		t.Errorf("expected cache invalidated after Start (2 inspects), got %d", inspectCount)
	}
}

func TestInit_NonBlocking(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	initStarted := make(chan struct{})
	initContinue := make(chan struct{})

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "init" {
			close(initStarted)
			<-initContinue
			return []byte("OK\n"), nil
		}
		// Status call during init — should NOT block.
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{"Name":"default","State":"stopped","Resources":{"CPUs":2,"Memory":4294967296,"DiskSize":10737418240},"ConnectionInfo":{}}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	go func() {
		m.Init(4, 8192, 20)
	}()

	<-initStarted

	// Status should return MachineStarting (initializing flag set) without blocking.
	done := make(chan MachineInfo, 1)
	go func() {
		done <- m.Status()
	}()

	select {
	case info := <-done:
		if info.Status != MachineStarting {
			t.Errorf("expected MachineStarting during Init, got %s", info.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status() blocked while Init() was running")
	}

	close(initContinue)
}

func TestStop_NonBlocking(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	stopStarted := make(chan struct{})
	stopContinue := make(chan struct{})

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "stop" {
			close(stopStarted)
			<-stopContinue
			return []byte("OK\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{"Name":"default","State":"running","Resources":{"CPUs":2,"Memory":4294967296,"DiskSize":10737418240},"ConnectionInfo":{}}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	go func() {
		m.Stop()
	}()

	<-stopStarted

	// Status should return MachineStarting (stopping flag set) without blocking.
	done := make(chan MachineInfo, 1)
	go func() {
		done <- m.Status()
	}()

	select {
	case info := <-done:
		if info.Status != MachineStarting {
			t.Errorf("expected MachineStarting during Stop, got %s", info.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status() blocked while Stop() was running")
	}

	close(stopContinue)
}
