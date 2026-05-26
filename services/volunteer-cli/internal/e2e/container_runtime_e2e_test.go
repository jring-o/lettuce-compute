package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// --- Mock DockerClient ---

// mockDockerClient implements runtime.DockerClient for E2E testing.
type mockDockerClient struct {
	pingFn             func(ctx context.Context) error
	imagePullFn        func(ctx context.Context, ref string) error
	imageExistsFn      func(ctx context.Context, ref string) (bool, error)
	containerCreateFn  func(ctx context.Context, cfg *runtime.ContainerConfig) (string, error)
	containerStartFn   func(ctx context.Context, containerID string) error
	containerWaitFn    func(ctx context.Context, containerID string) (int64, error)
	containerLogsFn    func(ctx context.Context, containerID string) (io.ReadCloser, error)
	containerInspectFn func(ctx context.Context, containerID string) (*runtime.ContainerStats, error)
	containerRemoveFn  func(ctx context.Context, containerID string) error

	lastCreateConfig *runtime.ContainerConfig
}

func (m *mockDockerClient) Ping(ctx context.Context) error {
	if m.pingFn != nil {
		return m.pingFn(ctx)
	}
	return nil
}

func (m *mockDockerClient) ImagePull(ctx context.Context, ref string) error {
	if m.imagePullFn != nil {
		return m.imagePullFn(ctx, ref)
	}
	return nil
}

func (m *mockDockerClient) ImageExists(ctx context.Context, ref string) (bool, error) {
	if m.imageExistsFn != nil {
		return m.imageExistsFn(ctx, ref)
	}
	return true, nil
}

func (m *mockDockerClient) ContainerCreate(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
	m.lastCreateConfig = cfg
	if m.containerCreateFn != nil {
		return m.containerCreateFn(ctx, cfg)
	}
	return "mock-container-id", nil
}

func (m *mockDockerClient) ContainerStart(ctx context.Context, containerID string) error {
	if m.containerStartFn != nil {
		return m.containerStartFn(ctx, containerID)
	}
	return nil
}

func (m *mockDockerClient) ContainerWait(ctx context.Context, containerID string) (int64, error) {
	if m.containerWaitFn != nil {
		return m.containerWaitFn(ctx, containerID)
	}
	return 0, nil
}

func (m *mockDockerClient) ContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	if m.containerLogsFn != nil {
		return m.containerLogsFn(ctx, containerID)
	}
	return io.NopCloser(bytes.NewReader([]byte("mock logs"))), nil
}

func (m *mockDockerClient) ContainerInspect(ctx context.Context, containerID string) (*runtime.ContainerStats, error) {
	if m.containerInspectFn != nil {
		return m.containerInspectFn(ctx, containerID)
	}
	return &runtime.ContainerStats{}, nil
}

func (m *mockDockerClient) ContainerRemove(ctx context.Context, containerID string) error {
	if m.containerRemoveFn != nil {
		return m.containerRemoveFn(ctx, containerID)
	}
	return nil
}

func (m *mockDockerClient) ContainerPause(ctx context.Context, containerID string) error {
	return nil
}

func (m *mockDockerClient) ContainerUnpause(ctx context.Context, containerID string) error {
	return nil
}

func (m *mockDockerClient) Close() error { return nil }

// --- Helpers ---

func withMockExecutor(t *testing.T, mock func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := runtime.CommandExecutor
	t.Cleanup(func() { runtime.CommandExecutor = orig })
	runtime.CommandExecutor = mock
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestWorkUnit(id string, image string, gpuRequired bool, gpuType string) *runtime.WorkUnit {
	wu := &runtime.WorkUnit{
		ID:     id,
		LeafID: "test-leaf",
		ExecutionSpec: runtime.ExecutionSpec{
			Image:       image,
			GPURequired: gpuRequired,
			GPUType:     gpuType,
			MaxMemoryMB: 512,
		},
		EnvVars: map[string]string{"TEST_VAR": "test_value"},
	}
	return wu
}

// createContainerRuntimeWithMock creates a ContainerRuntime with an injected mock DockerClient.
func createContainerRuntimeWithMock(t *testing.T, mock *mockDockerClient) *runtime.ContainerRuntime {
	t.Helper()
	dataDir := t.TempDir()
	logger := testLogger()
	return runtime.NewContainerRuntimeWithClient(dataDir, logger, mock)
}

// --- Scenario 1: Podman CPU Container Lifecycle ---
// Verifies that the container runtime with a mock client (simulating Podman backend)
// correctly executes the Prepare→Execute→Cleanup lifecycle for a CPU work unit.

func TestScenario1_PodmanCPUContainerLifecycle(t *testing.T) {
	var (
		pullCalled  bool
		createCalled bool
		startCalled  bool
		waitCalled   bool
		removeCalled bool
	)

	mock := &mockDockerClient{
		imageExistsFn: func(ctx context.Context, ref string) (bool, error) {
			return false, nil // force pull
		},
		imagePullFn: func(ctx context.Context, ref string) error {
			pullCalled = true
			if ref != "python:3.12-slim" {
				t.Errorf("expected image python:3.12-slim, got %s", ref)
			}
			return nil
		},
		containerCreateFn: func(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
			createCalled = true
			return "podman-container-1", nil
		},
		containerStartFn: func(ctx context.Context, containerID string) error {
			startCalled = true
			if containerID != "podman-container-1" {
				t.Errorf("expected container ID podman-container-1, got %s", containerID)
			}
			return nil
		},
		containerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			waitCalled = true
			return 0, nil
		},
		containerRemoveFn: func(ctx context.Context, containerID string) error {
			removeCalled = true
			return nil
		},
	}

	cr := createContainerRuntimeWithMock(t, mock)
	wu := newTestWorkUnit("11111111-1111-4111-8111-111111111111", "python:3.12-slim", false, "")

	ctx := context.Background()

	// Prepare: creates work dirs, pulls image.
	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if prep.WorkDir == "" {
		t.Fatal("expected non-empty WorkDir")
	}
	if !pullCalled {
		t.Error("expected image pull to be called")
	}

	// Execute: creates and runs container.
	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !createCalled {
		t.Error("expected container create")
	}
	if !startCalled {
		t.Error("expected container start")
	}
	if !waitCalled {
		t.Error("expected container wait")
	}
	if !removeCalled {
		t.Error("expected container remove")
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}

	// Verify container config.
	cfg := mock.lastCreateConfig
	if cfg == nil {
		t.Fatal("no container config captured")
	}
	if cfg.Image != "python:3.12-slim" {
		t.Errorf("expected image python:3.12-slim, got %s", cfg.Image)
	}
	if cfg.NetworkMode != "none" {
		t.Errorf("expected network mode 'none', got %s", cfg.NetworkMode)
	}
	if cfg.MemoryBytes != 512*1024*1024 {
		t.Errorf("expected memory bytes %d, got %d", 512*1024*1024, cfg.MemoryBytes)
	}

	// Verify env vars include LETTUCE standard vars.
	envMap := envToMap(cfg.Env)
	if envMap["LETTUCE_WORK_UNIT_ID"] != wu.ID {
		t.Errorf("expected LETTUCE_WORK_UNIT_ID=%s, got %s", wu.ID, envMap["LETTUCE_WORK_UNIT_ID"])
	}
	if envMap["TEST_VAR"] != "test_value" {
		t.Errorf("expected TEST_VAR=test_value, got %s", envMap["TEST_VAR"])
	}

	// Verify labels.
	if cfg.Labels["lettuce.work-unit-id"] != wu.ID {
		t.Errorf("expected label %s, got %s", wu.ID, cfg.Labels["lettuce.work-unit-id"])
	}

	// Cleanup: removes work directory.
	if err := cr.Cleanup(prep); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if _, err := os.Stat(prep.WorkDir); !os.IsNotExist(err) {
		t.Error("expected work directory to be removed after cleanup")
	}
}

// --- Scenario 2: Docker Fallback ---
// Verifies that the container runtime produces identical ContainerConfig
// regardless of whether it was created via Podman or Docker backend.
// Since NewContainerRuntimeWithClient is backend-agnostic (just uses DockerClient),
// this test verifies identical behavior by running the same work unit through
// two separate ContainerRuntime instances.

func TestScenario2_DockerFallbackIdenticalBehavior(t *testing.T) {
	podmanMock := &mockDockerClient{}
	dockerMock := &mockDockerClient{}

	podmanCR := createContainerRuntimeWithMock(t, podmanMock)
	dockerCR := createContainerRuntimeWithMock(t, dockerMock)

	wu := newTestWorkUnit("22222222-2222-4222-8222-222222222222", "ubuntu:22.04", false, "")
	ctx := context.Background()

	// Execute through "Podman" backend.
	podmanPrep, err := podmanCR.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Podman Prepare failed: %v", err)
	}
	_, err = podmanCR.Execute(ctx, wu, podmanPrep)
	if err != nil {
		t.Fatalf("Podman Execute failed: %v", err)
	}

	// Execute through "Docker" backend.
	dockerPrep, err := dockerCR.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Docker Prepare failed: %v", err)
	}
	_, err = dockerCR.Execute(ctx, wu, dockerPrep)
	if err != nil {
		t.Fatalf("Docker Execute failed: %v", err)
	}

	// Compare ContainerConfigs — they should be identical (same Image, Env, Binds pattern, etc).
	pCfg := podmanMock.lastCreateConfig
	dCfg := dockerMock.lastCreateConfig
	if pCfg == nil || dCfg == nil {
		t.Fatal("expected both configs captured")
	}

	if pCfg.Image != dCfg.Image {
		t.Errorf("Image mismatch: podman=%s docker=%s", pCfg.Image, dCfg.Image)
	}
	if pCfg.NetworkMode != dCfg.NetworkMode {
		t.Errorf("NetworkMode mismatch: podman=%s docker=%s", pCfg.NetworkMode, dCfg.NetworkMode)
	}
	if pCfg.MemoryBytes != dCfg.MemoryBytes {
		t.Errorf("MemoryBytes mismatch: podman=%d docker=%d", pCfg.MemoryBytes, dCfg.MemoryBytes)
	}
	if pCfg.CPUQuota != dCfg.CPUQuota {
		t.Errorf("CPUQuota mismatch: podman=%d docker=%d", pCfg.CPUQuota, dCfg.CPUQuota)
	}
	if pCfg.WorkDir != dCfg.WorkDir {
		t.Errorf("WorkDir mismatch: podman=%s docker=%s", pCfg.WorkDir, dCfg.WorkDir)
	}

	// Env vars should match (excluding paths which differ by dataDir).
	pEnv := envToMap(pCfg.Env)
	dEnv := envToMap(dCfg.Env)
	for _, key := range []string{"LETTUCE_WORK_UNIT_ID", "LETTUCE_INPUT_DIR", "LETTUCE_OUTPUT_DIR", "TEST_VAR"} {
		if pEnv[key] != dEnv[key] {
			t.Errorf("Env %s mismatch: podman=%s docker=%s", key, pEnv[key], dEnv[key])
		}
	}

	// GPU config should both be empty.
	if len(pCfg.GPUDeviceIDs) != 0 || len(dCfg.GPUDeviceIDs) != 0 {
		t.Error("expected no GPU device IDs for CPU work unit")
	}

	podmanCR.Cleanup(podmanPrep)
	dockerCR.Cleanup(dockerPrep)
}

// --- Scenario 3: Native-Only Mode ---
// Verifies that container runtime creation fails with BackendNone,
// and native runtime still works independently.

func TestScenario3_NativeOnlyMode(t *testing.T) {
	logger := testLogger()

	// Container runtime creation should fail with BackendNone.
	_, err := runtime.NewContainerRuntimeForBackend(t.TempDir(), logger, runtime.BackendInfo{
		Backend: runtime.BackendNone,
	})
	if err == nil {
		t.Fatal("expected error creating container runtime with BackendNone")
	}
	if !strings.Contains(err.Error(), "no container runtime") {
		t.Errorf("expected 'no container runtime' error, got: %v", err)
	}

	// Native runtime should still work fine.
	nr := runtime.NewNativeRuntime(t.TempDir(), logger)
	if nr.Name() != "native" {
		t.Errorf("expected name 'native', got %s", nr.Name())
	}

	// With a binary spec, native should be able to handle it.
	nativeSpec := &runtime.ExecutionSpec{
		Binaries: map[string]string{
			"windows_amd64": "http://example.com/bin.exe",
			"linux_amd64":   "http://example.com/bin",
			"darwin_amd64":  "http://example.com/bin-mac",
		},
	}
	if !nr.CanHandle(nativeSpec) {
		t.Error("expected NativeRuntime to handle spec with binaries")
	}

	// Without a binary spec, native should not handle it.
	containerSpec := &runtime.ExecutionSpec{
		Image: "python:3.12",
	}
	if nr.CanHandle(containerSpec) {
		t.Error("NativeRuntime should not handle container-only spec")
	}

	// Registry with only native should report "native" only.
	registry := daemon.NewRuntimeRegistry()
	registry.Register(nr)
	runtimes := registry.AvailableRuntimes()
	if len(runtimes) != 1 || runtimes[0] != "native" {
		t.Errorf("expected [native], got %v", runtimes)
	}
}

// --- Scenario 4: Linux GPU Container via Podman ---
// Verifies that GPU container config (DeviceRequests, VRAM limit env var)
// is identical through Podman and Docker backends.

func TestScenario4_GPUContainerViaPodman(t *testing.T) {
	nvidiaGPU := &runtime.GpuDetectionResult{
		Model:             "NVIDIA GeForce RTX 3080",
		Vendor:            "nvidia",
		VRAMMB:            10240,
		ComputeCapability: "8.6",
	}

	// Create two container runtimes with mock clients.
	podmanMock := &mockDockerClient{}
	dockerMock := &mockDockerClient{}

	podmanCR := createContainerRuntimeWithMock(t, podmanMock)
	podmanCR.SetBackend(runtime.BackendPodman)
	podmanCR.SetGPUs([]*runtime.GpuDetectionResult{nvidiaGPU})
	podmanCR.SetMaxGPUVRAMPct(80)

	dockerCR := createContainerRuntimeWithMock(t, dockerMock)
	dockerCR.SetBackend(runtime.BackendDocker)
	dockerCR.SetGPUs([]*runtime.GpuDetectionResult{nvidiaGPU})
	dockerCR.SetMaxGPUVRAMPct(80)

	wu := newTestWorkUnit("33333333-3333-4333-8333-333333333333", "cuda-workload:latest", true, "nvidia")
	wu.ExecutionSpec.MinVRAMMB = 4096
	ctx := context.Background()

	// Execute through Podman backend.
	pPrep, err := podmanCR.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Podman Prepare failed: %v", err)
	}
	_, err = podmanCR.Execute(ctx, wu, pPrep)
	if err != nil {
		t.Fatalf("Podman Execute failed: %v", err)
	}

	// Execute through Docker backend.
	dPrep, err := dockerCR.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Docker Prepare failed: %v", err)
	}
	_, err = dockerCR.Execute(ctx, wu, dPrep)
	if err != nil {
		t.Fatalf("Docker Execute failed: %v", err)
	}

	// Compare GPU configuration.
	pCfg := podmanMock.lastCreateConfig
	dCfg := dockerMock.lastCreateConfig
	if pCfg == nil || dCfg == nil {
		t.Fatal("expected both configs captured")
	}

	// Backends must differ.
	if pCfg.Backend != runtime.BackendPodman {
		t.Errorf("Podman config Backend = %v, want BackendPodman", pCfg.Backend)
	}
	if dCfg.Backend != runtime.BackendDocker {
		t.Errorf("Docker config Backend = %v, want BackendDocker", dCfg.Backend)
	}

	// Both should have GPUDeviceIDs set (used for env vars).
	if len(pCfg.GPUDeviceIDs) == 0 {
		t.Fatal("expected Podman config to have GPUDeviceIDs for NVIDIA GPU")
	}
	if len(dCfg.GPUDeviceIDs) == 0 {
		t.Fatal("expected Docker config to have GPUDeviceIDs for NVIDIA GPU")
	}
	if pCfg.GPUDeviceIDs[0] != dCfg.GPUDeviceIDs[0] {
		t.Errorf("GPUDeviceIDs mismatch: podman=%v docker=%v", pCfg.GPUDeviceIDs, dCfg.GPUDeviceIDs)
	}

	// Verify VRAM limit env var.
	// 80% of 10240 = 8192 MB
	pEnv := envToMap(pCfg.Env)
	dEnv := envToMap(dCfg.Env)

	expectedVRAM := "8192"
	if pEnv["LETTUCE_GPU_VRAM_LIMIT_MB"] != expectedVRAM {
		t.Errorf("Podman VRAM limit: expected %s, got %s", expectedVRAM, pEnv["LETTUCE_GPU_VRAM_LIMIT_MB"])
	}
	if dEnv["LETTUCE_GPU_VRAM_LIMIT_MB"] != expectedVRAM {
		t.Errorf("Docker VRAM limit: expected %s, got %s", expectedVRAM, dEnv["LETTUCE_GPU_VRAM_LIMIT_MB"])
	}

	// Verify GPU-related env vars match between backends.
	for _, key := range []string{
		"LETTUCE_GPU_ENABLED",
		"LETTUCE_GPU_VENDOR",
		"LETTUCE_GPU_VRAM_LIMIT_MB",
		"NVIDIA_VISIBLE_DEVICES",
	} {
		if pEnv[key] != dEnv[key] {
			t.Errorf("GPU env %s mismatch: podman=%s docker=%s", key, pEnv[key], dEnv[key])
		}
	}

	if pEnv["LETTUCE_GPU_ENABLED"] != "true" {
		t.Error("expected LETTUCE_GPU_ENABLED=true")
	}
	if pEnv["LETTUCE_GPU_VENDOR"] != "nvidia" {
		t.Errorf("expected LETTUCE_GPU_VENDOR=nvidia, got %s", pEnv["LETTUCE_GPU_VENDOR"])
	}

	podmanCR.Cleanup(pPrep)
	dockerCR.Cleanup(dPrep)
}

// TestScenario4_GPUNoMatch verifies error when no matching GPU is available.
func TestScenario4_GPUNoMatch(t *testing.T) {
	mock := &mockDockerClient{}
	cr := createContainerRuntimeWithMock(t, mock)
	// No GPUs set — GPU work unit should fail.

	wu := newTestWorkUnit("44444444-4444-4444-8444-444444444444", "cuda:latest", true, "nvidia")
	ctx := context.Background()

	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	_, err = cr.Execute(ctx, wu, prep)
	if err == nil {
		t.Fatal("expected error when no GPU available for GPU-required work unit")
	}
	if !strings.Contains(err.Error(), "no matching GPU") {
		t.Errorf("expected 'no matching GPU' error, got: %v", err)
	}

	cr.Cleanup(prep)
}

// --- Scenario 5: Podman Machine Lifecycle ---
// Verifies that PodmanMachineManager executes correct commands for
// Init, Start, Stop, and Status operations.

func TestScenario5_PodmanMachineLifecycle(t *testing.T) {
	logger := testLogger()
	var executedCommands []string

	currentState := "not_initialized"

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		executedCommands = append(executedCommands, cmd)

		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}

		if len(args) >= 2 && args[0] == "machine" {
			switch args[1] {
			case "inspect":
				switch currentState {
				case "not_initialized":
					return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
				case "running":
					return []byte(`[{
						"Name": "default",
						"State": "running",
						"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
						"ConnectionInfo": {"PodmanSocket": {"Path": "/run/user/1000/podman/podman.sock"}}
					}]`), nil
				case "stopped":
					return []byte(`[{
						"Name": "default",
						"State": "stopped",
						"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
						"ConnectionInfo": {}
					}]`), nil
				}
			case "init":
				currentState = "stopped"
				return []byte("Machine init complete\n"), nil
			case "start":
				currentState = "running"
				return []byte("Machine started\n"), nil
			case "stop":
				currentState = "stopped"
				return []byte("Machine stopped\n"), nil
			}
		}

		return nil, fmt.Errorf("unknown command: %s", cmd)
	})

	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	// On Windows (our test platform), NeedsMachine() returns true.
	if !mm.NeedsMachine() {
		// On Linux, machine operations are no-ops. The test still exercises the API.
		t.Log("NeedsMachine=false (Linux); machine operations will be no-ops")
	}

	// Step 1: Status should be not_initialized.
	info := mm.Status()
	if mm.NeedsMachine() {
		if info.Status != runtime.MachineNotInitialized {
			t.Errorf("expected not_initialized, got %s", info.Status)
		}
	}

	// Step 2: Setup (init + start).
	if err := mm.Setup(4, 8192, 20); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if mm.NeedsMachine() {
		// Verify init command was called with correct args.
		foundInit := false
		for _, cmd := range executedCommands {
			if strings.Contains(cmd, "machine init") {
				foundInit = true
				if !strings.Contains(cmd, "--cpus=4") {
					t.Errorf("init missing --cpus=4: %s", cmd)
				}
				if !strings.Contains(cmd, "--memory=8192") {
					t.Errorf("init missing --memory=8192: %s", cmd)
				}
				if !strings.Contains(cmd, "--disk-size=20") {
					t.Errorf("init missing --disk-size=20: %s", cmd)
				}
			}
		}
		if !foundInit {
			t.Error("expected 'machine init' command")
		}
	}

	// Step 3: Status should now be running.
	info = mm.Status()
	if mm.NeedsMachine() {
		if info.Status != runtime.MachineRunning {
			t.Errorf("expected running after setup, got %s", info.Status)
		}
	}

	// Step 4: Stop.
	if err := mm.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	info = mm.Status()
	if mm.NeedsMachine() {
		if info.Status != runtime.MachineStopped {
			t.Errorf("expected stopped, got %s", info.Status)
		}
	}

	// Step 5: Start.
	if err := mm.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	info = mm.Status()
	if mm.NeedsMachine() {
		if info.Status != runtime.MachineRunning {
			t.Errorf("expected running after start, got %s", info.Status)
		}

		// Verify machine start command was issued.
		foundStart := false
		for _, cmd := range executedCommands {
			if strings.Contains(cmd, "machine start") {
				foundStart = true
			}
		}
		if !foundStart {
			t.Error("expected 'machine start' command")
		}
	}
}

// --- Scenario 6: Runtime Status Reporting via Management API ---
// Verifies that the management API endpoints correctly report container
// runtime status and handle lifecycle operations.

func TestScenario6_ManagementAPIContainerRuntime(t *testing.T) {
	logger := testLogger()

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "default",
				"State": "running",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {"PodmanPipe": {"Path": "//./pipe/podman-machine-default"}}
			}]`), nil
		}
		return nil, fmt.Errorf("not found")
	})

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.ContainerBackend = "podman"
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	mm := runtime.NewPodmanMachineManager("/usr/bin/podman", logger)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config:         cfg,
		MachineManager: mm,
		Logger:         logger,
	})

	bridge := management.NewDaemonBridge(d, cfgPath)
	srv := management.NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	token := srv.Token()

	// GET /api/v1/container-runtime — verify correct status.
	resp := doRequest(t, baseURL, token, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["backend"] != "podman" {
		t.Errorf("expected backend 'podman', got %v", body["backend"])
	}
	if body["status"] != "running" {
		t.Errorf("expected status 'running', got %v", body["status"])
	}

	// POST /api/v1/container-runtime/setup — idempotent, returns 200.
	resp = doRequest(t, baseURL, token, "POST", "/api/v1/container-runtime/setup", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for idempotent setup when running, got %d", resp.StatusCode)
	}

	// POST /api/v1/container-runtime/start — bridge pre-checks status.
	// Returns 409 ALREADY_RUNNING when machine is already running.
	resp = doRequest(t, baseURL, token, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusOK {
		t.Errorf("expected 409 or 200 for start when running, got %d", resp.StatusCode)
	}
}

func TestScenario6_ManagementAPIStopNotRunning(t *testing.T) {
	logger := testLogger()

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("not found")
	})

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "test-server"},
	}
	cfg.Save(cfgPath)

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	bridge := management.NewDaemonBridge(d, cfgPath)
	srv := management.NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	token := srv.Token()

	// POST /api/v1/container-runtime/stop — no manager configured, returns 500.
	resp := doRequest(t, baseURL, token, "POST", "/api/v1/container-runtime/stop", "")
	if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 500 or 409 for stop when no manager/not running, got %d", resp.StatusCode)
	}
}

// --- HTTP test helpers ---

func doRequest(t *testing.T, baseURL, token, method, path, body string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	return result
}

// envToMap converts a []string of "KEY=value" pairs to a map.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}
