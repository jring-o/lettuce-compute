package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MockDockerClient implements DockerClient for testing without a real Docker daemon.
type MockDockerClient struct {
	PingFn             func(ctx context.Context) error
	InfoFn             func(ctx context.Context) (*EngineInfo, error)
	ImagePullFn        func(ctx context.Context, ref string) error
	ImageExistsFn      func(ctx context.Context, ref string) (bool, error)
	ContainerCreateFn  func(ctx context.Context, cfg *ContainerConfig) (string, error)
	ContainerStartFn   func(ctx context.Context, containerID string) error
	ContainerWaitFn    func(ctx context.Context, containerID string) (int64, error)
	ContainerLogsFn    func(ctx context.Context, containerID string) (io.ReadCloser, error)
	ContainerInspectFn func(ctx context.Context, containerID string) (*ContainerStats, error)
	ContainerStopFn    func(ctx context.Context, containerID string, timeout time.Duration) error
	ContainerRemoveFn  func(ctx context.Context, containerID string) error
	ImageIDFn          func(ctx context.Context, ref string) (string, error)
	ImageListFn        func(ctx context.Context) ([]ImageSummary, error)
	ImageRemoveFn      func(ctx context.Context, imageID string) error
	ContainerListFn    func(ctx context.Context, labelKey string) ([]ContainerSummary, error)

	// Capture the last ContainerCreate config for assertions.
	LastCreateConfig *ContainerConfig
}

func (m *MockDockerClient) Ping(ctx context.Context) error {
	if m.PingFn != nil {
		return m.PingFn(ctx)
	}
	return nil
}

func (m *MockDockerClient) Info(ctx context.Context) (*EngineInfo, error) {
	if m.InfoFn != nil {
		return m.InfoFn(ctx)
	}
	return &EngineInfo{}, nil
}

func (m *MockDockerClient) ImagePull(ctx context.Context, ref string) error {
	if m.ImagePullFn != nil {
		return m.ImagePullFn(ctx, ref)
	}
	return nil
}

func (m *MockDockerClient) ImageExists(ctx context.Context, ref string) (bool, error) {
	if m.ImageExistsFn != nil {
		return m.ImageExistsFn(ctx, ref)
	}
	return true, nil
}

func (m *MockDockerClient) ImageID(ctx context.Context, ref string) (string, error) {
	if m.ImageIDFn != nil {
		return m.ImageIDFn(ctx, ref)
	}
	return "", nil
}

func (m *MockDockerClient) ImageList(ctx context.Context) ([]ImageSummary, error) {
	if m.ImageListFn != nil {
		return m.ImageListFn(ctx)
	}
	return nil, nil
}

func (m *MockDockerClient) ImageRemove(ctx context.Context, imageID string) error {
	if m.ImageRemoveFn != nil {
		return m.ImageRemoveFn(ctx, imageID)
	}
	return nil
}

func (m *MockDockerClient) ContainerList(ctx context.Context, labelKey string) ([]ContainerSummary, error) {
	if m.ContainerListFn != nil {
		return m.ContainerListFn(ctx, labelKey)
	}
	return nil, nil
}

func (m *MockDockerClient) ContainerCreate(ctx context.Context, cfg *ContainerConfig) (string, error) {
	m.LastCreateConfig = cfg
	if m.ContainerCreateFn != nil {
		return m.ContainerCreateFn(ctx, cfg)
	}
	return "mock-container-id", nil
}

func (m *MockDockerClient) ContainerStart(ctx context.Context, containerID string) error {
	if m.ContainerStartFn != nil {
		return m.ContainerStartFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) ContainerWait(ctx context.Context, containerID string) (int64, error) {
	if m.ContainerWaitFn != nil {
		return m.ContainerWaitFn(ctx, containerID)
	}
	return 0, nil
}

func (m *MockDockerClient) ContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	if m.ContainerLogsFn != nil {
		return m.ContainerLogsFn(ctx, containerID)
	}
	return io.NopCloser(bytes.NewReader([]byte("mock logs"))), nil
}

func (m *MockDockerClient) ContainerInspect(ctx context.Context, containerID string) (*ContainerStats, error) {
	if m.ContainerInspectFn != nil {
		return m.ContainerInspectFn(ctx, containerID)
	}
	return &ContainerStats{}, nil
}

func (m *MockDockerClient) ContainerStop(ctx context.Context, containerID string, timeout time.Duration) error {
	if m.ContainerStopFn != nil {
		return m.ContainerStopFn(ctx, containerID, timeout)
	}
	return nil
}

func (m *MockDockerClient) ContainerRemove(ctx context.Context, containerID string) error {
	if m.ContainerRemoveFn != nil {
		return m.ContainerRemoveFn(ctx, containerID)
	}
	return nil
}

func (m *MockDockerClient) ContainerPause(ctx context.Context, containerID string) error {
	return nil
}

func (m *MockDockerClient) ContainerUnpause(ctx context.Context, containerID string) error {
	return nil
}

func (m *MockDockerClient) Close() error { return nil }

func newTestContainerRuntime(t *testing.T, mock *MockDockerClient) (*ContainerRuntime, string) {
	t.Helper()
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cr := NewContainerRuntimeWithClient(dataDir, logger, mock)
	return cr, dataDir
}

func TestContainerRuntime_Name(t *testing.T) {
	cr, _ := newTestContainerRuntime(t, &MockDockerClient{})
	if cr.Name() != "container" {
		t.Errorf("Name() = %q, want %q", cr.Name(), "container")
	}
}

func TestContainerRuntime_CanHandle(t *testing.T) {
	cr, _ := newTestContainerRuntime(t, &MockDockerClient{})

	tests := []struct {
		name string
		spec *ExecutionSpec
		want bool
	}{
		{"image set", &ExecutionSpec{Image: "alpine:latest"}, true},
		{"image empty", &ExecutionSpec{Image: ""}, false},
		{"nil spec", nil, false},
		{"binaries only", &ExecutionSpec{Binaries: map[string]string{"linux_amd64": "url"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cr.CanHandle(tt.spec)
			if got != tt.want {
				t.Errorf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainerRuntime_PrepareHappyPath(t *testing.T) {
	mock := &MockDockerClient{
		ImageExistsFn: func(ctx context.Context, ref string) (bool, error) {
			return true, nil // image cached locally
		},
	}
	cr, dataDir := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:             "dc5ff9da-f084-4dd7-86b8-e829669814f8", // was wu-1
		LeafID:         "proj-1",
		InputData:      []byte("test input"),
		ParametersJSON: `{"seed": 42}`,
		ExecutionSpec:  ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Verify work directory.
	expectedWorkDir := filepath.Join(dataDir, "container-work", wu.ID)
	if prep.WorkDir != expectedWorkDir {
		t.Errorf("WorkDir = %q, want %q", prep.WorkDir, expectedWorkDir)
	}

	// Verify directories exist.
	for _, sub := range []string{"input", "output"} {
		dir := filepath.Join(prep.WorkDir, sub)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory %s should exist: %v", sub, err)
		}
	}

	// Verify input file.
	inputData, err := os.ReadFile(filepath.Join(prep.WorkDir, "input", "input.dat"))
	if err != nil {
		t.Fatalf("read input.dat: %v", err)
	}
	if string(inputData) != "test input" {
		t.Errorf("input.dat = %q, want %q", inputData, "test input")
	}

	// Verify parameters file.
	paramsData, err := os.ReadFile(filepath.Join(prep.WorkDir, "input", "parameters.json"))
	if err != nil {
		t.Fatalf("read parameters.json: %v", err)
	}
	if string(paramsData) != `{"seed": 42}` {
		t.Errorf("parameters.json = %q, want %q", paramsData, `{"seed": 42}`)
	}
}

func TestContainerRuntime_PrepareWithPull(t *testing.T) {
	pullCalled := false
	mock := &MockDockerClient{
		ImageExistsFn: func(ctx context.Context, ref string) (bool, error) {
			return false, nil // not cached
		},
		ImagePullFn: func(ctx context.Context, ref string) error {
			pullCalled = true
			if ref != "myimage:v1" {
				t.Errorf("pulled wrong image: %s", ref)
			}
			return nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "c67da0e2-15c7-49c4-8a0f-93d9acb43f1d", // was wu-pull
		ExecutionSpec: ExecutionSpec{Image: "myimage:v1"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	if !pullCalled {
		t.Error("ImagePull was not called for uncached image")
	}
}

func TestContainerRuntime_PrepareDockerUnavailable(t *testing.T) {
	mock := &MockDockerClient{
		PingFn: func(ctx context.Context) error {
			return fmt.Errorf("connection refused")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "4c2f23e0-e46e-40ab-80c0-3101e11b3467", // was wu-noDocker
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error when Docker is unavailable")
	}
	if !strings.Contains(err.Error(), "docker is not available") {
		t.Errorf("error = %q, want to contain 'docker is not available'", err)
	}
}

func TestContainerRuntime_PreparePullFailure(t *testing.T) {
	mock := &MockDockerClient{
		ImageExistsFn: func(ctx context.Context, ref string) (bool, error) {
			return false, nil
		},
		ImagePullFn: func(ctx context.Context, ref string) error {
			return fmt.Errorf("pull timeout")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "e3da5662-a334-4026-81e4-dc07c64d7144", // was wu-pullfail
		ExecutionSpec: ExecutionSpec{Image: "bad-image:latest"},
	}

	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error from pull failure")
	}
	if !strings.Contains(err.Error(), "pull timeout") {
		t.Errorf("error = %q, want to contain 'pull timeout'", err)
	}
}

func TestContainerRuntime_PrepareDiskExhaustion(t *testing.T) {
	mock := &MockDockerClient{
		ImageExistsFn: func(ctx context.Context, ref string) (bool, error) { return false, nil },
		ImagePullFn: func(ctx context.Context, ref string) error {
			return fmt.Errorf("failed to register layer: write /var/lib/.../diff: no space left on device")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "67f34972-c6c9-4a9c-8171-bf6e57c39762", // was wu-nospace
		ExecutionSpec: ExecutionSpec{Image: "ghcr.io/example/huge:1.0"},
	}

	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error from disk exhaustion")
	}
	msg := err.Error()
	// Actionable: names the image, says it's a disk-space problem, mentions free space.
	for _, want := range []string{"ghcr.io/example/huge:1.0", "disk space", "free up space"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want to contain %q", msg, want)
		}
	}
	// Original cause preserved for logs/wrapping.
	if !strings.Contains(msg, "no space left on device") {
		t.Errorf("error = %q, want to preserve underlying cause", msg)
	}
}

func TestInterpretPullError(t *testing.T) {
	if interpretPullError(BackendDocker, "img", nil) != nil {
		t.Error("nil error should stay nil")
	}

	tests := []struct {
		name      string
		backend   ContainerBackend
		err       error
		wantDisk  bool
		wantInMsg string
	}{
		{"docker nospace", BackendDocker, fmt.Errorf("write: no space left on device"), true, "Docker's image store"},
		{"podman enospc", BackendPodman, fmt.Errorf("ENOSPC: disk full"), true, "Podman machine"},
		{"generic", BackendDocker, fmt.Errorf("manifest unknown"), false, ""},
		{"timeout not disk", BackendPodman, fmt.Errorf("context deadline exceeded"), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interpretPullError(tt.backend, "myimg", tt.err)
			if got == nil {
				t.Fatal("expected non-nil error")
			}
			isDisk := strings.Contains(got.Error(), "out of disk space")
			if isDisk != tt.wantDisk {
				t.Errorf("disk-detection = %v, want %v (msg=%q)", isDisk, tt.wantDisk, got)
			}
			if tt.wantInMsg != "" && !strings.Contains(got.Error(), tt.wantInMsg) {
				t.Errorf("msg = %q, want to contain %q", got, tt.wantInMsg)
			}
			// Underlying error is always preserved via %w.
			if !strings.Contains(got.Error(), tt.err.Error()) {
				t.Errorf("msg = %q, want to wrap %q", got, tt.err)
			}
		})
	}
}

func TestContainerRuntime_ExecuteHappyPath(t *testing.T) {
	mock := &MockDockerClient{
		ContainerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			return 0, nil
		},
		ContainerInspectFn: func(ctx context.Context, containerID string) (*ContainerStats, error) {
			return &ContainerStats{
				CPUUsageUser:   2_000_000_000, // 2 seconds
				CPUUsageKernel: 500_000_000,   // 0.5 seconds
				MemoryPeak:     256 * 1024 * 1024,
			}, nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:              "f42b7a90-69c3-43bb-8a5b-0a4b3c29d4eb", // was exec-1
		LeafID:          "proj-1",
		DeadlineSeconds: 60,
		ExecutionSpec:   ExecutionSpec{Image: "alpine:latest", MaxMemoryMB: 512},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Write output.dat to simulate container output.
	outputDir := filepath.Join(prep.WorkDir, "output")
	if err := os.WriteFile(filepath.Join(outputDir, "output.dat"), []byte("result data"), 0o644); err != nil {
		t.Fatalf("write output: %v", err)
	}

	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if string(result.OutputData) != "result data" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "result data")
	}
	if result.OutputChecksum != checksumSHA256([]byte("result data")) {
		t.Error("checksum mismatch")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Metrics.CPUSecondsUser != 2.0 {
		t.Errorf("CPUSecondsUser = %f, want 2.0", result.Metrics.CPUSecondsUser)
	}
	if result.Metrics.CPUSecondsSystem != 0.5 {
		t.Errorf("CPUSecondsSystem = %f, want 0.5", result.Metrics.CPUSecondsSystem)
	}
	if result.Metrics.PeakMemoryMB != 256 {
		t.Errorf("PeakMemoryMB = %d, want 256", result.Metrics.PeakMemoryMB)
	}
	if result.Metrics.CPUCoresUsed < 1 {
		t.Errorf("CPUCoresUsed = %d, want >= 1", result.Metrics.CPUCoresUsed)
	}
}

func TestContainerRuntime_ExecuteWithDeadline(t *testing.T) {
	mock := &MockDockerClient{
		ContainerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			// Simulate a container that takes too long.
			<-ctx.Done()
			return -1, ctx.Err()
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:              "3a3eabf3-03d8-4d99-8e7e-a8b9c67f7e58", // was timeout-1
		DeadlineSeconds: 1,
		ExecutionSpec:   ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from deadline exceeded")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error = %q, want to contain 'deadline exceeded'", err)
	}
}

func TestContainerRuntime_ExecuteNonZeroExit(t *testing.T) {
	mock := &MockDockerClient{
		ContainerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			return 1, nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "a9ed1f2d-3170-4a7c-8b28-4d2c636e1111", // was exit-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestContainerRuntime_ExecuteNetworkIsolation(t *testing.T) {
	tests := []struct {
		name          string
		id            string
		networkAccess bool
		wantMode      string
	}{
		{"no network", "aaaaaaaa-1111-4111-8111-000000000001", false, "none"},
		{"with network", "aaaaaaaa-1111-4111-8111-000000000002", true, "bridge"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDockerClient{}
			cr, _ := newTestContainerRuntime(t, mock)

			wu := &WorkUnit{
				ID:            tt.id,
				ExecutionSpec: ExecutionSpec{Image: "alpine:latest", NetworkAccess: tt.networkAccess},
			}

			prep, err := cr.Prepare(context.Background(), wu)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			defer cr.Cleanup(prep)

			// Write output to avoid read errors.
			os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

			_, err = cr.Execute(context.Background(), wu, prep)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if mock.LastCreateConfig.NetworkMode != tt.wantMode {
				t.Errorf("NetworkMode = %q, want %q", mock.LastCreateConfig.NetworkMode, tt.wantMode)
			}
		})
	}
}

func TestContainerRuntime_ExecuteWithEnvVars(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "61ddabbf-f1eb-44c7-8498-5b3ff77ff0b7", // was env-1
		EnvVars:       map[string]string{"MY_VAR": "my_value", "OTHER": "123"},
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	envSet := make(map[string]bool)
	for _, e := range mock.LastCreateConfig.Env {
		envSet[e] = true
	}

	// Check user env vars.
	if !envSet["MY_VAR=my_value"] {
		t.Error("missing env var MY_VAR=my_value")
	}
	if !envSet["OTHER=123"] {
		t.Error("missing env var OTHER=123")
	}
	// Check LETTUCE env vars.
	if !envSet["LETTUCE_WORK_UNIT_ID="+wu.ID] {
		t.Error("missing LETTUCE_WORK_UNIT_ID")
	}
	if !envSet["LETTUCE_INPUT_DIR=/work/input"] {
		t.Error("missing LETTUCE_INPUT_DIR")
	}
	if !envSet["LETTUCE_OUTPUT_DIR=/work/output"] {
		t.Error("missing LETTUCE_OUTPUT_DIR")
	}
	if !envSet["LETTUCE_PARAMETERS_FILE=/work/input/parameters.json"] {
		t.Error("missing LETTUCE_PARAMETERS_FILE")
	}
}

func TestContainerRuntime_ExecuteMemoryLimit(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "11ad2331-ce16-4da6-8213-d75602aa08a3", // was mem-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest", MaxMemoryMB: 4096},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	expectedBytes := int64(4096) * 1024 * 1024
	if mock.LastCreateConfig.MemoryBytes != expectedBytes {
		t.Errorf("MemoryBytes = %d, want %d", mock.LastCreateConfig.MemoryBytes, expectedBytes)
	}
}

func TestContainerRuntime_ExecuteCPULimit(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	cr.SetMaxCPUCores(2)

	wu := &WorkUnit{
		ID:            "505885bb-386c-478a-8204-36832e02431a", // was cpu-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if mock.LastCreateConfig.CPUPeriod != 100000 {
		t.Errorf("CPUPeriod = %d, want 100000", mock.LastCreateConfig.CPUPeriod)
	}
	if mock.LastCreateConfig.CPUQuota != 200000 {
		t.Errorf("CPUQuota = %d, want 200000 (2 cores)", mock.LastCreateConfig.CPUQuota)
	}
}

func TestContainerRuntime_ExecuteLabels(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "f40389aa-e6a2-4145-8f5d-3181bf34a056", // was label-1
		LeafID:        "proj-abc",
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	labels := mock.LastCreateConfig.Labels
	if labels["lettuce.work-unit-id"] != wu.ID {
		t.Errorf("work-unit-id label = %q, want %q", labels["lettuce.work-unit-id"], wu.ID)
	}
	if labels["lettuce.leaf-id"] != "proj-abc" {
		t.Errorf("leaf-id label = %q, want %q", labels["lettuce.leaf-id"], "proj-abc")
	}
}

func TestContainerRuntime_ExecuteVolumeMounts(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "f88e9355-85c9-49e2-8795-ed44b56989dc", // was bind-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	binds := mock.LastCreateConfig.Binds
	if len(binds) != 3 {
		t.Fatalf("expected 3 binds, got %d", len(binds))
	}

	// Input is read-only; output and checkpoint are read-write.
	foundInputRO := false
	foundOutputRW := false
	foundCheckpointRW := false
	for _, bind := range binds {
		if strings.HasSuffix(bind, ":/work/input:ro") {
			foundInputRO = true
		}
		if strings.HasSuffix(bind, ":/work/output") {
			foundOutputRW = true
		}
		if strings.HasSuffix(bind, ":/work/checkpoint") {
			foundCheckpointRW = true
		}
	}
	if !foundInputRO {
		t.Errorf("expected input bind with :ro, got %v", binds)
	}
	if !foundOutputRW {
		t.Errorf("expected output bind without :ro, got %v", binds)
	}
	if !foundCheckpointRW {
		t.Errorf("expected checkpoint bind without :ro, got %v", binds)
	}
}

func TestContainerRuntime_GracefulStopOnCancel(t *testing.T) {
	stopTimeout := make(chan time.Duration, 1)
	mock := &MockDockerClient{
		ContainerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			<-ctx.Done() // block until the work is cancelled
			return 0, ctx.Err()
		},
		ContainerStopFn: func(ctx context.Context, containerID string, timeout time.Duration) error {
			stopTimeout <- timeout
			return nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "a1b2c3d4-1111-2222-3333-444455556666",
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}
	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = cr.Execute(ctx, wu, prep)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "cancel") {
		t.Errorf("error = %q, want it to mention cancellation", err.Error())
	}

	// On cancellation the container must be stopped with a grace period (so its
	// entrypoint can flush a final checkpoint) rather than only force-removed.
	select {
	case got := <-stopTimeout:
		if got != gracefulShutdownGrace {
			t.Errorf("ContainerStop grace = %v, want %v", got, gracefulShutdownGrace)
		}
	default:
		t.Error("ContainerStop was not called on cancellation")
	}
}

func TestContainerRuntime_Cleanup(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "7fd59306-cb10-459b-83f8-f2c502f7be41", // was cleanup-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Verify work dir exists.
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("work dir should exist: %v", err)
	}

	if err := cr.Cleanup(prep); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Work dir should be gone.
	if _, err := os.Stat(prep.WorkDir); !os.IsNotExist(err) {
		t.Error("work dir should be removed after cleanup")
	}
}

func TestContainerRuntime_CleanupNil(t *testing.T) {
	cr, _ := newTestContainerRuntime(t, &MockDockerClient{})
	if err := cr.Cleanup(nil); err != nil {
		t.Errorf("Cleanup(nil) should not error: %v", err)
	}
	if err := cr.Cleanup(&PrepareResult{}); err != nil {
		t.Errorf("Cleanup(empty) should not error: %v", err)
	}
}

func TestContainerRuntime_ExecuteContainerLogsCaptured(t *testing.T) {
	logContent := "container stdout line 1\ncontainer stderr line 2\n"
	mock := &MockDockerClient{
		ContainerLogsFn: func(ctx context.Context, containerID string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte(logContent))), nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "d9395083-51c0-4fc8-8d45-4131f68eff28", // was logs-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify execution.log was written.
	logData, err := os.ReadFile(filepath.Join(prep.WorkDir, "execution.log"))
	if err != nil {
		t.Fatalf("read execution.log: %v", err)
	}
	if string(logData) != logContent {
		t.Errorf("execution.log = %q, want %q", logData, logContent)
	}
}

func TestContainerRuntime_ExecuteOutputFallback(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "243b1dca-dc95-4fb0-8581-495db3740c59", // was fallback-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Write a file that is NOT output.dat to test fallback.
	os.WriteFile(filepath.Join(prep.WorkDir, "output", "result.txt"), []byte("fallback data"), 0o644)

	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if string(result.OutputData) != "fallback data" {
		t.Errorf("OutputData = %q, want %q (fallback to first file)", result.OutputData, "fallback data")
	}
}

func TestContainerRuntime_ExecuteLogCap(t *testing.T) {
	// Generate logs larger than 10 MB to verify the cap.
	bigLog := make([]byte, 11*1024*1024) // 11 MB
	for i := range bigLog {
		bigLog[i] = 'A'
	}
	mock := &MockDockerClient{
		ContainerLogsFn: func(ctx context.Context, containerID string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bigLog)), nil
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "65da51c4-55d0-4d4a-8d2d-ed24a29cb55a", // was logcap-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	logData, err := os.ReadFile(filepath.Join(prep.WorkDir, "execution.log"))
	if err != nil {
		t.Fatalf("read execution.log: %v", err)
	}
	maxLogSize := 10 * 1024 * 1024
	if len(logData) > maxLogSize {
		t.Errorf("execution.log size = %d, want <= %d (10 MB cap)", len(logData), maxLogSize)
	}
	if len(logData) != maxLogSize {
		t.Errorf("execution.log size = %d, want exactly %d (should fill up to cap)", len(logData), maxLogSize)
	}
}

func TestContainerRuntime_ExecuteEmptyOutput(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "dfeea60f-9cb6-4af3-88e9-6d226d2d103f", // was empty-out-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Do NOT write any output files â€” output dir is empty.
	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.OutputData != nil {
		t.Errorf("OutputData = %v, want nil for empty output dir", result.OutputData)
	}
}

func TestContainerRuntime_ExecuteWorkDir(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "72cb9510-f979-417f-8ef1-b8b5e2549c00", // was workdir-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if mock.LastCreateConfig.WorkDir != "/work" {
		t.Errorf("container WorkDir = %q, want %q", mock.LastCreateConfig.WorkDir, "/work")
	}
}

func TestIsDockerAvailable(t *testing.T) {
	// IsDockerAvailable connects to the real Docker daemon, so we can only
	// verify it returns a bool without panicking. On CI or machines without
	// Docker this will return false, which is fine.
	result := IsDockerAvailable()
	t.Logf("IsDockerAvailable() = %v", result)
	// No assertion on value â€” just verifying no panic / clean execution.
}

func TestContainerRuntime_ExecuteContainerRemoveFails(t *testing.T) {
	mock := &MockDockerClient{
		ContainerRemoveFn: func(ctx context.Context, containerID string) error {
			return fmt.Errorf("remove failed")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "23a341f4-7ee7-4e5c-8683-95f7ac9fa9ec", // was rmfail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	// Execute should succeed even if container removal fails (best-effort).
	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute should not fail on container remove error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestContainerRuntime_ExecuteCreateFailure(t *testing.T) {
	mock := &MockDockerClient{
		ContainerCreateFn: func(ctx context.Context, cfg *ContainerConfig) (string, error) {
			return "", fmt.Errorf("image not found")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "a3fc5311-e0da-443b-818c-231afffecbaa", // was create-fail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from container create failure")
	}
	if !strings.Contains(err.Error(), "create container") {
		t.Errorf("error = %q, want to contain 'create container'", err)
	}
}

func TestContainerRuntime_ExecuteStartFailure(t *testing.T) {
	mock := &MockDockerClient{
		ContainerStartFn: func(ctx context.Context, containerID string) error {
			return fmt.Errorf("insufficient resources")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "07569fd0-c5ce-40fd-8f7c-114ad25d71f7", // was start-fail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from container start failure")
	}
	if !strings.Contains(err.Error(), "start container") {
		t.Errorf("error = %q, want to contain 'start container'", err)
	}
}

func TestContainerRuntime_ExecuteWaitErrorWithoutDeadline(t *testing.T) {
	mock := &MockDockerClient{
		ContainerWaitFn: func(ctx context.Context, containerID string) (int64, error) {
			return -1, fmt.Errorf("connection lost")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "0ddcbeeb-bfc2-42f9-874a-489708457360", // was wait-err-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from container wait failure")
	}
	if !strings.Contains(err.Error(), "container wait") {
		t.Errorf("error = %q, want to contain 'container wait'", err)
	}
	// Confirm it does NOT say "deadline exceeded" â€” this is a Docker error, not a timeout.
	if strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error = %q, should not mention deadline", err)
	}
}

func TestContainerRuntime_ExecuteInspectFailure(t *testing.T) {
	mock := &MockDockerClient{
		ContainerInspectFn: func(ctx context.Context, containerID string) (*ContainerStats, error) {
			return nil, fmt.Errorf("container gone")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "f903b256-4c4a-48f0-8e3c-d3033fa594df", // was inspect-fail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	// Execute should succeed even if inspect fails (graceful degradation).
	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute should not fail on inspect error: %v", err)
	}

	// Metrics should still have wall clock but zero CPU/memory.
	if result.Metrics.WallClockSeconds < 0 {
		t.Errorf("WallClockSeconds = %d, want >= 0", result.Metrics.WallClockSeconds)
	}
	if result.Metrics.CPUSecondsUser != 0 {
		t.Errorf("CPUSecondsUser = %f, want 0 (inspect failed)", result.Metrics.CPUSecondsUser)
	}
	if result.Metrics.PeakMemoryMB != 0 {
		t.Errorf("PeakMemoryMB = %d, want 0 (inspect failed)", result.Metrics.PeakMemoryMB)
	}
}

func TestContainerRuntime_ExecuteLogsFailure(t *testing.T) {
	mock := &MockDockerClient{
		ContainerLogsFn: func(ctx context.Context, containerID string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("logs unavailable")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "627bbf05-bc12-4b22-83eb-aaa35f8af888", // was logfail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	// Execute should succeed even if logs cannot be retrieved.
	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute should not fail on logs error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}

	// execution.log should NOT exist (logs retrieval failed).
	logPath := filepath.Join(prep.WorkDir, "execution.log")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("execution.log should not exist when log retrieval fails")
	}
}

func TestContainerRuntime_PrepareImageExistsError(t *testing.T) {
	mock := &MockDockerClient{
		ImageExistsFn: func(ctx context.Context, ref string) (bool, error) {
			return false, fmt.Errorf("daemon unreachable")
		},
	}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "19aebff7-398f-45e0-812e-91d6dae9a83d", // was imgcheck-fail-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error from image exists check failure")
	}
	if !strings.Contains(err.Error(), "check image") {
		t.Errorf("error = %q, want to contain 'check image'", err)
	}
}

func TestContainerRuntime_BuildMetricsNilStats(t *testing.T) {
	cr, _ := newTestContainerRuntime(t, &MockDockerClient{})
	metrics := cr.buildMetrics(nil, 5*time.Second)

	if metrics.WallClockSeconds != 5 {
		t.Errorf("WallClockSeconds = %d, want 5", metrics.WallClockSeconds)
	}
	if metrics.CPUSecondsUser != 0 {
		t.Errorf("CPUSecondsUser = %f, want 0", metrics.CPUSecondsUser)
	}
	if metrics.CPUSecondsSystem != 0 {
		t.Errorf("CPUSecondsSystem = %f, want 0", metrics.CPUSecondsSystem)
	}
	if metrics.PeakMemoryMB != 0 {
		t.Errorf("PeakMemoryMB = %d, want 0", metrics.PeakMemoryMB)
	}
	if metrics.CPUCoresUsed != 0 {
		t.Errorf("CPUCoresUsed = %d, want 0 (nil stats)", metrics.CPUCoresUsed)
	}
}

func TestContainerRuntime_BuildMetricsZeroCPU(t *testing.T) {
	cr, _ := newTestContainerRuntime(t, &MockDockerClient{})
	stats := &ContainerStats{
		CPUUsageUser:   0,
		CPUUsageKernel: 0,
		MemoryPeak:     128 * 1024 * 1024,
	}
	metrics := cr.buildMetrics(stats, 3*time.Second)

	if metrics.WallClockSeconds != 3 {
		t.Errorf("WallClockSeconds = %d, want 3", metrics.WallClockSeconds)
	}
	// With zero CPU usage, CPUCoresUsed should be clamped to 1.
	if metrics.CPUCoresUsed != 1 {
		t.Errorf("CPUCoresUsed = %d, want 1 (minimum clamp)", metrics.CPUCoresUsed)
	}
	if metrics.PeakMemoryMB != 128 {
		t.Errorf("PeakMemoryMB = %d, want 128", metrics.PeakMemoryMB)
	}
}

func TestContainerRuntime_PrepareNoInputNoParams(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "37fb2089-7629-4fee-8288-42693f6353fd", // was no-input-1
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
		// InputData and ParametersJSON intentionally empty.
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Verify input.dat was NOT written.
	inputPath := filepath.Join(prep.WorkDir, "input", "input.dat")
	if _, err := os.Stat(inputPath); !os.IsNotExist(err) {
		t.Errorf("input.dat should not exist when InputData is empty")
	}

	// Verify parameters.json was NOT written.
	paramsPath := filepath.Join(prep.WorkDir, "input", "parameters.json")
	if _, err := os.Stat(paramsPath); !os.IsNotExist(err) {
		t.Errorf("parameters.json should not exist when ParametersJSON is empty")
	}
}

func TestContainerRuntime_PrepareExternalInputURL(t *testing.T) {
	// Serve external data via httptest server.
	payload := []byte("external input from URL")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "e6788bae-8467-46b1-81d0-d7aacbebc860", // was ext-input-1
		InputDataURL:  srv.URL + "/data.csv",
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Verify input.dat was written from downloaded data.
	got, err := os.ReadFile(filepath.Join(prep.WorkDir, "input", "input.dat"))
	if err != nil {
		t.Fatalf("read input.dat: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("input.dat = %q, want %q", got, payload)
	}
}

func TestContainerRuntime_PrepareExternalInputURL_Failure(t *testing.T) {
	// Serve 404 to trigger download error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "ed670f18-2d5b-41f1-8ea3-e3388ff21a68", // was ext-input-fail
		InputDataURL:  srv.URL + "/missing.csv",
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for failed external download")
	}
	if !strings.Contains(err.Error(), "download input data") {
		t.Errorf("error = %q, want to contain 'download input data'", err)
	}
}

func TestContainerRuntime_PrepareInlineOverExternalURL(t *testing.T) {
	// When both InputData and InputDataURL are set, inline takes precedence.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when inline data is present")
		w.Write([]byte("should not be used"))
	}))
	defer srv.Close()

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "600f80fe-95c1-4ecc-89fb-8df4ee80e36c", // was both-inputs
		InputData:     []byte("inline wins"),
		InputDataURL:  srv.URL + "/data.csv",
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	got, err := os.ReadFile(filepath.Join(prep.WorkDir, "input", "input.dat"))
	if err != nil {
		t.Fatalf("read input.dat: %v", err)
	}
	if string(got) != "inline wins" {
		t.Errorf("input.dat = %q, want %q", got, "inline wins")
	}
}

func TestContainerRuntime_ExecuteNoMemoryLimit(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{
		ID:            "48b8a0af-008a-4ded-8114-ba19622fa2c0", // was no-mem-limit
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest", MaxMemoryMB: 0},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if mock.LastCreateConfig.MemoryBytes != 0 {
		t.Errorf("MemoryBytes = %d, want 0 (no limit)", mock.LastCreateConfig.MemoryBytes)
	}
}

func TestContainerRuntime_ExecuteGPUNVIDIA(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			for _, a := range args {
				if strings.Contains(a, "name") {
					return []byte("NVIDIA RTX 3080\n"), nil
				}
			}
			return []byte("65, 80, 4096, 10240, 250.00\n"), nil
		}
		return nil, fmt.Errorf("not found")
	})

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	cr.SetGPUs([]*GpuDetectionResult{
		{Model: "NVIDIA RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
	})
	cr.SetMaxGPUVRAMPct(50)

	wu := &WorkUnit{
		ID:              "aa316d61-b1ba-4541-88f5-94589bc137df", // was gpu-nvidia-1
		LeafID:          "proj-gpu",
		DeadlineSeconds: 5,
		ExecutionSpec:   ExecutionSpec{Image: "cuda:latest", GPURequired: true},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("gpu result"), 0o644)

	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify GPU DeviceRequest was set.
	if len(mock.LastCreateConfig.GPUDeviceIDs) != 1 || mock.LastCreateConfig.GPUDeviceIDs[0] != "0" {
		t.Errorf("GPUDeviceIDs = %v, want [\"0\"]", mock.LastCreateConfig.GPUDeviceIDs)
	}

	// Verify GPU env vars.
	envSet := make(map[string]bool)
	for _, e := range mock.LastCreateConfig.Env {
		envSet[e] = true
	}
	if !envSet["LETTUCE_GPU_ENABLED=true"] {
		t.Error("missing LETTUCE_GPU_ENABLED=true")
	}
	if !envSet["LETTUCE_GPU_VENDOR=nvidia"] {
		t.Error("missing LETTUCE_GPU_VENDOR=nvidia")
	}

	// Verify NVIDIA_VISIBLE_DEVICES is set for VRAM isolation (S58).
	if !envSet["NVIDIA_VISIBLE_DEVICES=0"] {
		t.Error("missing NVIDIA_VISIBLE_DEVICES=0")
	}

	// Verify VRAM limit env var: 50% of 10240 MB = 5120 MB (S58).
	if !envSet["LETTUCE_GPU_VRAM_LIMIT_MB=5120"] {
		t.Error("missing LETTUCE_GPU_VRAM_LIMIT_MB=5120")
	}

	// Verify output.
	if string(result.OutputData) != "gpu result" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "gpu result")
	}
}

func TestContainerRuntime_ExecuteGPUNVIDIA_Podman(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			for _, a := range args {
				if strings.Contains(a, "name") {
					return []byte("NVIDIA RTX 3080\n"), nil
				}
			}
			return []byte("65, 80, 4096, 10240, 250.00\n"), nil
		}
		return nil, fmt.Errorf("not found")
	})

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	cr.backend = BackendPodman // Simulate Podman backend
	cr.SetGPUs([]*GpuDetectionResult{
		{Model: "NVIDIA RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
	})

	wu := &WorkUnit{
		ID:              "bf54a808-4fff-4767-8567-f27da863288d", // was gpu-podman-1
		LeafID:          "proj-gpu",
		DeadlineSeconds: 5,
		ExecutionSpec:   ExecutionSpec{Image: "cuda:latest", GPURequired: true},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("gpu result"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg := mock.LastCreateConfig

	// Verify backend is set on the config.
	if cfg.Backend != BackendPodman {
		t.Errorf("Backend = %v, want BackendPodman", cfg.Backend)
	}

	// Verify GPUDeviceIDs are still set (used for env vars).
	if len(cfg.GPUDeviceIDs) != 1 || cfg.GPUDeviceIDs[0] != "0" {
		t.Errorf("GPUDeviceIDs = %v, want [\"0\"]", cfg.GPUDeviceIDs)
	}

	// Verify GPU env vars are present.
	envSet := make(map[string]bool)
	for _, e := range cfg.Env {
		envSet[e] = true
	}
	if !envSet["LETTUCE_GPU_ENABLED=true"] {
		t.Error("missing LETTUCE_GPU_ENABLED=true")
	}
	if !envSet["NVIDIA_VISIBLE_DEVICES=0"] {
		t.Error("missing NVIDIA_VISIBLE_DEVICES=0")
	}
}

func TestContainerRuntime_ExecuteGPUAMD(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("not found")
	})

	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	cr.SetGPUs([]*GpuDetectionResult{
		{Model: "AMD RX 7900", Vendor: "amd", VRAMMB: 24576},
	})

	wu := &WorkUnit{
		ID:            "8a11d30a-420a-463d-8210-6fd09489fdf0", // was gpu-amd-1
		ExecutionSpec: ExecutionSpec{Image: "rocm:latest", GPURequired: true, GPUType: "amd"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify AMD device mappings.
	if len(mock.LastCreateConfig.DeviceMappings) != 2 {
		t.Fatalf("DeviceMappings = %d, want 2", len(mock.LastCreateConfig.DeviceMappings))
	}
	if mock.LastCreateConfig.DeviceMappings[0].PathOnHost != "/dev/dri/renderD128" {
		t.Errorf("first device = %q, want /dev/dri/renderD128", mock.LastCreateConfig.DeviceMappings[0].PathOnHost)
	}
	if mock.LastCreateConfig.DeviceMappings[1].PathOnHost != "/dev/kfd" {
		t.Errorf("second device = %q, want /dev/kfd", mock.LastCreateConfig.DeviceMappings[1].PathOnHost)
	}

	// Verify GPU env vars.
	envSet := make(map[string]bool)
	for _, e := range mock.LastCreateConfig.Env {
		envSet[e] = true
	}
	if !envSet["LETTUCE_GPU_VENDOR=amd"] {
		t.Error("missing LETTUCE_GPU_VENDOR=amd")
	}
}

func TestContainerRuntime_ExecuteGPUTypeFiltering(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("not found")
	})

	tests := []struct {
		name       string
		gpuType    string
		gpus       []*GpuDetectionResult
		wantVendor string
		wantErr    bool
	}{
		{
			name:    "nvidia-only selects nvidia",
			gpuType: "nvidia",
			gpus: []*GpuDetectionResult{
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
				{Model: "RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
			},
			wantVendor: "nvidia",
		},
		{
			name:    "amd-only selects amd",
			gpuType: "amd",
			gpus: []*GpuDetectionResult{
				{Model: "RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
			},
			wantVendor: "amd",
		},
		{
			name:    "any selects first",
			gpuType: "",
			gpus: []*GpuDetectionResult{
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
			},
			wantVendor: "amd",
		},
		{
			name:    "no matching GPU",
			gpuType: "nvidia",
			gpus: []*GpuDetectionResult{
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
			},
			wantErr: true,
		},
		{
			name:    "uppercase NVIDIA matches nvidia GPU",
			gpuType: "NVIDIA",
			gpus: []*GpuDetectionResult{
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
				{Model: "RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
			},
			wantVendor: "nvidia",
		},
		{
			name:    "uppercase AMD matches amd GPU",
			gpuType: "AMD",
			gpus: []*GpuDetectionResult{
				{Model: "RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
				{Model: "AMD RX", Vendor: "amd", VRAMMB: 8192},
			},
			wantVendor: "amd",
		},
		{
			name:    "ANY selects first GPU",
			gpuType: "ANY",
			gpus: []*GpuDetectionResult{
				{Model: "RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
			},
			wantVendor: "nvidia",
		},
		{
			name:    "no GPUs at all",
			gpuType: "",
			gpus:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDockerClient{}
			cr, _ := newTestContainerRuntime(t, mock)
			cr.SetGPUs(tt.gpus)

			wu := &WorkUnit{
				ID:            testUUID("filter-" + tt.name),
				ExecutionSpec: ExecutionSpec{Image: "test:latest", GPURequired: true, GPUType: tt.gpuType},
			}

			prep, err := cr.Prepare(context.Background(), wu)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			defer cr.Cleanup(prep)

			os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

			_, err = cr.Execute(context.Background(), wu, prep)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error for no matching GPU")
				}
				if !strings.Contains(err.Error(), "no matching GPU found") {
					t.Errorf("error = %q, want to contain 'no matching GPU found'", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			envSet := make(map[string]bool)
			for _, e := range mock.LastCreateConfig.Env {
				envSet[e] = true
			}
			if !envSet["LETTUCE_GPU_VENDOR="+tt.wantVendor] {
				t.Errorf("expected LETTUCE_GPU_VENDOR=%s in env", tt.wantVendor)
			}
		})
	}
}

func TestContainerRuntime_ExecuteNoCPULimit(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	// maxCPUCores defaults to 0 (no limit).

	wu := &WorkUnit{
		ID:            "182e9723-0e8a-4278-89f5-0a247a7789bf", // was no-cpu-limit
		ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)

	_, err = cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if mock.LastCreateConfig.CPUQuota != 0 {
		t.Errorf("CPUQuota = %d, want 0 (no limit)", mock.LastCreateConfig.CPUQuota)
	}
	if mock.LastCreateConfig.CPUPeriod != 0 {
		t.Errorf("CPUPeriod = %d, want 0 (no limit)", mock.LastCreateConfig.CPUPeriod)
	}
}

// TestContainerRuntime_VRAMLimitWarning verifies that when a work unit's MinVRAMMB
// exceeds the volunteer's allowed VRAM budget, execution still proceeds (the VRAM
// check is a safety-net warning, not a hard block). The container should still run
// and produce output. (S58)
func TestContainerRuntime_VRAMLimitWarning(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			for _, a := range args {
				if strings.Contains(a, "name") {
					return []byte("NVIDIA RTX 3080\n"), nil
				}
			}
			return []byte("65, 80, 4096, 10240, 250.00\n"), nil
		}
		return nil, fmt.Errorf("not found")
	})

	// Capture log output to verify the warning is emitted.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mock := &MockDockerClient{}
	dataDir := t.TempDir()
	cr := NewContainerRuntimeWithClient(dataDir, logger, mock)
	cr.SetGPUs([]*GpuDetectionResult{
		{Model: "NVIDIA RTX 3080", Vendor: "nvidia", VRAMMB: 10240},
	})
	cr.SetMaxGPUVRAMPct(50) // allowedVRAMMB = 5120

	wu := &WorkUnit{
		ID:              "6660c272-e3a9-4874-8080-0ec7b5dcbf20", // was gpu-vram-warn-1
		LeafID:          "proj-gpu",
		DeadlineSeconds: 5,
		ExecutionSpec: ExecutionSpec{
			Image:       "cuda:latest",
			GPURequired: true,
			MinVRAMMB:   8000, // Exceeds allowed 5120 MB
		},
	}

	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("vram-warn-result"), 0o644)

	// Execute should succeed â€” the VRAM check is a warning, not a hard block.
	result, err := cr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute should succeed despite VRAM warning: %v", err)
	}

	// Verify output was produced normally.
	if string(result.OutputData) != "vram-warn-result" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "vram-warn-result")
	}

	// Verify the warning was logged.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "work unit VRAM requirement exceeds volunteer limit") {
		t.Errorf("expected VRAM warning in log output, got: %s", logOutput)
	}

	// Verify the VRAM limit env var is still set (uses allowed, not required).
	envSet := make(map[string]bool)
	for _, e := range mock.LastCreateConfig.Env {
		envSet[e] = true
	}
	if !envSet["LETTUCE_GPU_VRAM_LIMIT_MB=5120"] {
		t.Error("missing LETTUCE_GPU_VRAM_LIMIT_MB=5120 â€” should be set to allowed limit, not required")
	}
}

func TestNewContainerRuntimeForBackend_Podman(t *testing.T) {
	// NewContainerRuntimeForBackend with Podman backend creates a runtime via host socket.
	// The Docker SDK client creation itself won't fail even with a fake socket path
	// â€” connection errors only occur when you try to use the client.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dataDir := t.TempDir()

	info := BackendInfo{
		Backend:    BackendPodman,
		SocketPath: "/tmp/fake-podman.sock",
		Version:    "4.9.0",
		BinaryPath: "/usr/bin/podman",
	}

	cr, err := NewContainerRuntimeForBackend(dataDir, logger, info)
	if err != nil {
		t.Fatalf("NewContainerRuntimeForBackend(podman): %v", err)
	}
	if cr == nil {
		t.Fatal("expected non-nil ContainerRuntime")
	}
	if cr.Name() != "container" {
		t.Errorf("Name() = %s, want container", cr.Name())
	}
}

func TestNewContainerRuntimeForBackend_Docker(t *testing.T) {
	// Docker backend uses default env-based connection. Client creation succeeds
	// even without a running daemon.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dataDir := t.TempDir()

	info := BackendInfo{
		Backend: BackendDocker,
	}

	cr, err := NewContainerRuntimeForBackend(dataDir, logger, info)
	if err != nil {
		t.Fatalf("NewContainerRuntimeForBackend(docker): %v", err)
	}
	if cr == nil {
		t.Fatal("expected non-nil ContainerRuntime")
	}
}

func TestNewContainerRuntimeForBackend_None(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dataDir := t.TempDir()

	info := BackendInfo{
		Backend: BackendNone,
	}

	_, err := NewContainerRuntimeForBackend(dataDir, logger, info)
	if err == nil {
		t.Fatal("expected error for BackendNone")
	}
	if !strings.Contains(err.Error(), "no container runtime available") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewContainerRuntimeForBackend_MockClientWorks(t *testing.T) {
	// Verify that a ContainerRuntime created via NewContainerRuntimeForBackend
	// works identically to one created with NewContainerRuntimeWithClient.
	// We can't inject a mock through the backend path directly (it creates a real
	// SDK client), but we verify the struct is properly initialized.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dataDir := t.TempDir()

	info := BackendInfo{
		Backend:    BackendPodman,
		SocketPath: "/tmp/test.sock",
		Version:    "5.0.0",
	}

	cr, err := NewContainerRuntimeForBackend(dataDir, logger, info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it can handle container specs.
	if !cr.CanHandle(&ExecutionSpec{Image: "alpine:latest"}) {
		t.Error("CanHandle should return true for container spec")
	}
	if cr.CanHandle(&ExecutionSpec{}) {
		t.Error("CanHandle should return false for empty spec")
	}
}
