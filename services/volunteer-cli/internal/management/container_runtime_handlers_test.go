package management

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// setupContainerRuntimeTestEnv creates a test environment with a PodmanMachineManager.
func setupContainerRuntimeTestEnv(t *testing.T, backend string, mm *runtime.PodmanMachineManager) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.ContainerBackend = backend
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config:         cfg,
		MachineManager: mm,
		Logger:         logger,
	})

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	return &testEnv{
		server:  srv,
		bridge:  bridge,
		daemon:  d,
		dataDir: tmpDir,
		cfgPath: cfgPath,
		baseURL: "http://127.0.0.1:" + itoa(srv.Port()),
		token:   srv.Token(),
	}
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func TestGetContainerRuntime_None(t *testing.T) {
	env := setupContainerRuntimeTestEnv(t, "", nil)
	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)
	if result["backend"] != "none" {
		t.Errorf("expected backend 'none', got %v", result["backend"])
	}
	if result["status"] != "not_installed" {
		t.Errorf("expected status 'not_installed', got %v", result["status"])
	}
}

func TestGetContainerRuntime_Podman(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock podman to return version (Linux-like behavior — no machine needed).
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)
	if result["backend"] != "podman" {
		t.Errorf("expected backend 'podman', got %v", result["backend"])
	}
}

func TestGetContainerRuntime_Docker(t *testing.T) {
	env := setupContainerRuntimeTestEnv(t, "docker", nil)
	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)
	if result["backend"] != "docker" {
		t.Errorf("expected backend 'docker', got %v", result["backend"])
	}
}

func TestSetupContainerRuntime_NoManager(t *testing.T) {
	env := setupContainerRuntimeTestEnv(t, "", nil)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/setup", `{"cpus":4,"memory_mb":8192,"disk_gb":20}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no manager, got %d", resp.StatusCode)
	}
}

func TestStartContainerRuntime_NoManager(t *testing.T) {
	env := setupContainerRuntimeTestEnv(t, "", nil)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	// Without a machine manager, status is "not_installed" which is != "running",
	// but bridge.StartContainerRuntime returns error.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no manager, got %d", resp.StatusCode)
	}
}

func TestStopContainerRuntime_NoManager(t *testing.T) {
	env := setupContainerRuntimeTestEnv(t, "", nil)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/stop", "")
	// No manager configured — bridge returns "no container runtime configured".
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no manager, got %d", resp.StatusCode)
	}
}

func TestStartContainerRuntime_Failure(t *testing.T) {
	if !runtime.NeedsMachineForTest() {
		t.Skip("start failure path only exercises on Windows/macOS where machine start shells out")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock machine start to fail with a generic error (not "not initialized" or "already running").
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			return []byte("hypervisor error\n"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("command failed")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when start fails, got %d", resp.StatusCode)
	}
}

func TestSetupContainerRuntime_InvalidJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock to return not initialized so we pass the "already running" check.
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
		}
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/setup", "{invalid json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected error code VALIDATION_ERROR, got %v", errObj["code"])
	}
}

func TestGetContainerRuntime_WithMachineInfo(t *testing.T) {
	if !runtime.NeedsMachineForTest() {
		t.Skip("machine info only populated on Windows/macOS where podman machine inspect runs")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock machine inspect to return full machine info.
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
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
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := decodeJSON(t, resp)
	if result["backend"] != "podman" {
		t.Errorf("expected backend 'podman', got %v", result["backend"])
	}
	if result["status"] != "running" {
		t.Errorf("expected status 'running', got %v", result["status"])
	}
	if result["machine_name"] != "default" {
		t.Errorf("expected machine_name 'default', got %v", result["machine_name"])
	}
	// JSON numbers decode as float64.
	if cpus, ok := result["machine_cpus"].(float64); !ok || cpus != 4 {
		t.Errorf("expected machine_cpus 4, got %v", result["machine_cpus"])
	}
}

func TestSetupContainerRuntime_Success(t *testing.T) {
	if !runtime.NeedsMachineForTest() {
		t.Skip("setup happy path only runs on Windows/macOS where machine lifecycle is exercised")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock: status returns not_initialized, then init and start succeed.
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "init" {
			return []byte("Machine init complete\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			return []byte("Machine started\n"), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/setup", `{"cpus":4,"memory_mb":8192,"disk_gb":20}`)
	if resp.StatusCode != http.StatusOK {
		result := decodeJSON(t, resp)
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, result)
	}

	result := decodeJSON(t, resp)
	if result["status"] != "running" {
		t.Errorf("expected status 'running', got %v", result["status"])
	}
}

func TestStartContainerRuntime_Success(t *testing.T) {
	if !runtime.NeedsMachineForTest() {
		t.Skip("start happy path only runs on Windows/macOS where machine lifecycle is exercised")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock: status returns stopped, start succeeds.
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "stopped",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			return []byte("Machine started\n"), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusOK {
		result := decodeJSON(t, resp)
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, result)
	}

	result := decodeJSON(t, resp)
	if result["status"] != "running" {
		t.Errorf("expected status 'running', got %v", result["status"])
	}
}

func TestStopContainerRuntime_Success(t *testing.T) {
	if !runtime.NeedsMachineForTest() {
		t.Skip("stop happy path only runs on Windows/macOS where machine lifecycle is exercised")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// Mock: status returns running, stop succeeds.
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "stop" {
			return []byte("Machine stopped\n"), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/stop", "")
	if resp.StatusCode != http.StatusOK {
		result := decodeJSON(t, resp)
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, result)
	}

	result := decodeJSON(t, resp)
	if result["status"] != "stopped" {
		t.Errorf("expected status 'stopped', got %v", result["status"])
	}
}

func TestSetupContainerRuntime_BodyTooLarge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)

	// Send a body > 1 MB.
	largeBody := strings.Repeat("x", 1<<20+1)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/setup", largeBody)
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got %d", resp.StatusCode)
	}
}

func TestSetupContainerRuntime_AlreadyRunning_Idempotent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// On this platform (Windows), mock machine inspect to return running.
	// On Linux, podman --version returning success means "running".
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		return nil, fmt.Errorf("not found")
	}

	env := setupContainerRuntimeTestEnv(t, "podman", mm)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/setup", "")
	// Setup is idempotent — already running returns success.
	if resp.StatusCode != http.StatusOK {
		result := decodeJSON(t, resp)
		t.Fatalf("expected 200 (idempotent), got %d: %v", resp.StatusCode, result)
	}
}
