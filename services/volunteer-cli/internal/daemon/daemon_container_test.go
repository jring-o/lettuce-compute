package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newContainerTestDaemon creates a daemon with a RuntimeRegistry for container tests.
func newContainerTestDaemon(mc *mockClient, registry *RuntimeRegistry) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false // disable thermal for most tests

	d := NewDaemon(DaemonConfig{
		Config:          cfg,
		PubKey:          pub,
		PrivKey:         priv,
		Client:          mc,
		VolunteerID:     "test-volunteer-id",
		RuntimeRegistry: registry,
		Logger:          logger,
	})
	// Legacy Client path builds a head with no per-head runtime trust; grant it so work
	// flows through the fetcher's per-head trust gate (see grantAllRuntimeTrust).
	grantAllRuntimeTrust(d.multiClient.Servers())
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond
	d.multiClient.SetBackoff(1*time.Millisecond, 16*time.Millisecond)
	return d
}

// --- Scenario 1: Container work unit execution ---

func TestF16_ContainerWorkUnitExecution(t *testing.T) {
	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "1ecc156d-5771-45c6-8e41-718829bd7b45", // was wu-container-1
						LeafId:                   "proj-1",
						Runtime:                  "container",
						InputData:                []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Image:       "test-image:latest",
							GpuRequired: false,
						},
					},
				},
			}, nil
		},
	}

	containerRT := &mockRuntime{
		canHandle: true,
		name:      "container",
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			// Verify the work unit has the right image.
			if wu.ExecutionSpec.Image != "test-image:latest" {
				t.Errorf("expected image test-image:latest, got %s", wu.ExecutionSpec.Image)
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("container-output"),
				OutputChecksum: "abc123",
				ExitCode:       0,
				Metrics: runtime.ExecutionMetrics{
					WallClockSeconds: 15,
					CPUSecondsUser:   12.0,
				},
			}, nil
		},
	}

	registry := NewRuntimeRegistry()
	registry.Register(&mockRuntime{canHandle: true, name: "native"})
	registry.Register(containerRT)

	d := newContainerTestDaemon(mc, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if containerRT.getPrepareCalls() != 1 {
		t.Errorf("container prepare calls = %d, want 1", containerRT.getPrepareCalls())
	}
	if containerRT.getExecuteCalls() != 1 {
		t.Errorf("container execute calls = %d, want 1", containerRT.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 1 {
		t.Errorf("submit calls = %d, want 1", mc.getSubmitCalls())
	}
	if containerRT.getCleanupCalls() != 1 {
		t.Errorf("container cleanup calls = %d, want 1", containerRT.getCleanupCalls())
	}
}

// --- Scenario 2: GPU container work unit ---

func TestF16_GPUContainerWorkUnit(t *testing.T) {
	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "9a9f659e-6b8b-4c91-8f0f-cc53cb72aa26", // was wu-gpu-1
						LeafId:                   "proj-1",
						Runtime:                  "container",
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Image:       "gpu-image:latest",
							GpuRequired: true,
							GpuType:     "nvidia",
						},
					},
				},
			}, nil
		},
	}

	containerRT := &mockRuntime{
		canHandle: true,
		name:      "container",
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			if !wu.ExecutionSpec.GPURequired {
				t.Error("expected GPURequired=true")
			}
			if wu.ExecutionSpec.GPUType != "nvidia" {
				t.Errorf("expected GPUType=nvidia, got %s", wu.ExecutionSpec.GPUType)
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("gpu-output"),
				OutputChecksum: "gpu-hash",
				ExitCode:       0,
				Metrics: runtime.ExecutionMetrics{
					WallClockSeconds: 30,
					CPUSecondsUser:   5.0,
					GPUSeconds:       25.5,
					GPUModel:         "NVIDIA GeForce RTX 3080",
					GPUVRAMUsedMB:    4096,
				},
			}, nil
		},
	}

	registry := NewRuntimeRegistry()
	registry.Register(&mockRuntime{canHandle: true, name: "native"})
	registry.Register(containerRT)

	d := newContainerTestDaemon(mc, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// Verify GPU metrics were included in submitted result.
	mc.mu.Lock()
	lastReq := mc.lastSubmitReq
	mc.mu.Unlock()

	if lastReq == nil {
		t.Fatal("no submit request captured")
	}
	meta := lastReq.Metadata
	if meta == nil {
		t.Fatal("no metadata in submit request")
	}
	if meta.GpuSeconds != 25.5 {
		t.Errorf("GpuSeconds = %f, want 25.5", meta.GpuSeconds)
	}
	if meta.GpuModel != "NVIDIA GeForce RTX 3080" {
		t.Errorf("GpuModel = %q, want %q", meta.GpuModel, "NVIDIA GeForce RTX 3080")
	}
	if meta.GpuVramUsedMb != 4096 {
		t.Errorf("GpuVramUsedMb = %d, want 4096", meta.GpuVramUsedMb)
	}
}

// --- Scenario 3: Runtime mismatch handling ---

func TestF16_RuntimeMismatch(t *testing.T) {
	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "bad94ec5-4093-4f4d-8d9d-eb38e1d438a4", // was wu-mismatch
						LeafId:                   "proj-1",
						Runtime:                  "container", // requests container runtime
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Image: "some-image:latest",
						},
					},
				},
			}, nil
		},
	}

	// Only native runtime registered â€” no container runtime.
	nativeRT := &mockRuntime{canHandle: true, name: "native"}
	registry := NewRuntimeRegistry()
	registry.Register(nativeRT)

	d := newContainerTestDaemon(mc, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// Daemon should NOT have executed the work unit (no container runtime).
	if nativeRT.getExecuteCalls() != 0 {
		t.Errorf("native execute calls = %d, want 0 (work unit required container)", nativeRT.getExecuteCalls())
	}
	// Should NOT have submitted a result.
	if mc.getSubmitCalls() != 0 {
		t.Errorf("submit calls = %d, want 0", mc.getSubmitCalls())
	}
}

// --- Scenario 4: Thermal pause/resume ---

func TestF16_ThermalPauseResume(t *testing.T) {
	var workRequests int
	var mu sync.Mutex

	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			mu.Lock()
			workRequests++
			mu.Unlock()
			return nil, status.Error(codes.NotFound, "no work")
		},
	}

	nativeRT := &mockRuntime{canHandle: true, name: "native"}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRuntimeRegistry()
	registry.Register(nativeRT)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false // thermal monitor goroutine stays off; we test via thermalPauseCh directly

	d := NewDaemon(DaemonConfig{
		Config:          cfg,
		PubKey:          pub,
		PrivKey:         priv,
		Client:          mc,
		VolunteerID:     "test-volunteer-id",
		RuntimeRegistry: registry,
		Logger:          logger,
	})
	// Legacy Client path builds a head with no per-head runtime trust; grant it so work
	// flows through the fetcher's per-head trust gate (see grantAllRuntimeTrust).
	grantAllRuntimeTrust(d.multiClient.Servers())
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond
	d.multiClient.SetBackoff(1*time.Millisecond, 16*time.Millisecond)

	// Simulate thermal throttle via the thermal pause channel directly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Let daemon run briefly, then pause via thermal channel.
		time.Sleep(50 * time.Millisecond)
		d.thermalPauseCh <- true // thermal pause

		// Wait, then resume.
		time.Sleep(100 * time.Millisecond)
		d.thermalPauseCh <- false // thermal resume

		// Let it run a bit more, then stop.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	// Verify daemon was paused (should have some work requests before pause,
	// then a gap during pause, then more after resume).
	mu.Lock()
	finalRequests := workRequests
	mu.Unlock()

	if finalRequests == 0 {
		t.Error("expected at least some work requests")
	}

	// Verify daemon's paused state was toggled correctly.
	// After the test, daemon should be stopped (not paused).
	d.mu.Lock()
	paused := d.paused
	d.mu.Unlock()
	if paused {
		t.Error("daemon should not be paused after resume signal")
	}
}

// --- Scenario 5: Native work unit still works with registry ---

func TestF16_NativeWorkUnitRegression(t *testing.T) {
	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               "1932032b-5d60-438f-8b64-1244ba787ff9", // was wu-native-1
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Binaries: map[string]string{"linux_amd64": "http://example.com/bin"},
						},
					},
				},
			}, nil
		},
	}

	nativeRT := &mockRuntime{canHandle: true, name: "native"}
	containerRT := &mockRuntime{canHandle: true, name: "container"}

	registry := NewRuntimeRegistry()
	registry.Register(nativeRT)
	registry.Register(containerRT)

	d := newContainerTestDaemon(mc, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// Native runtime should have been used, NOT container.
	if nativeRT.getPrepareCalls() != 1 {
		t.Errorf("native prepare calls = %d, want 1", nativeRT.getPrepareCalls())
	}
	if nativeRT.getExecuteCalls() != 1 {
		t.Errorf("native execute calls = %d, want 1", nativeRT.getExecuteCalls())
	}
	if containerRT.getExecuteCalls() != 0 {
		t.Errorf("container execute calls = %d, want 0", containerRT.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 1 {
		t.Errorf("submit calls = %d, want 1", mc.getSubmitCalls())
	}
}
