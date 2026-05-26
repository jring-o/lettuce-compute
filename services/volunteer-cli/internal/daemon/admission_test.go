package daemon

import (
	"context"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestCanAccommodateWU_FreeRAM verifies that admission refuses a work unit the
// machine cannot currently fit in real free system RAM, and skips
// the check when free memory can't be read.
func TestCanAccommodateWU_FreeRAM(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.slotManager = NewSlotManager(4, d.logger)
	d.cfg.ResourceLimits.MaxMemoryMB = 0 // isolate the free-RAM check from the budget check

	orig := freeSystemMemoryMB
	defer func() { freeSystemMemoryMB = orig }()

	freeSystemMemoryMB = func() (int, bool) { return 10000, true }

	// 8000 MB WU + 512 headroom = 8512 <= 10000 free → fits.
	if !d.canAccommodateWU(&runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 8000}}) {
		t.Error("should accommodate 8000MB WU with 10000MB free")
	}

	// 10000 MB WU + 512 headroom = 10512 > 10000 free → reject.
	if d.canAccommodateWU(&runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 10000}}) {
		t.Error("should NOT accommodate 10000MB WU when only 10000MB is free (headroom)")
	}

	// When free RAM is unknown, the real-RAM check is skipped.
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	if !d.canAccommodateWU(&runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 999999}}) {
		t.Error("should accommodate when free RAM is unknown and no budget is configured")
	}
}

// TestCanAccommodateWU_GPUExclusivity verifies that admission runs at most one
// GPU work unit per physical GPU, so concurrent units never oversubscribe VRAM.
func TestCanAccommodateWU_GPUExclusivity(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.slotManager = NewSlotManager(4, d.logger)
	d.cfg.ResourceLimits.MaxMemoryMB = 0 // isolate the GPU check
	// One physical GPU.
	d.cachedHW = &lettucev1.HardwareCapabilities{Gpus: []*lettucev1.GpuInfo{{}}}

	gpuWU := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{GPURequired: true}}

	if d.slotManager.ActiveGPUCount() != 0 {
		t.Fatalf("ActiveGPUCount = %d, want 0", d.slotManager.ActiveGPUCount())
	}
	if !d.canAccommodateWU(gpuWU) {
		t.Fatal("should accommodate first GPU WU with 1 GPU and 0 active")
	}

	// Start an active GPU slot that blocks until released.
	blockCh := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-d.slotManager.available
	d.slotManager.StartSlot(ctx, slotID, &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID: "gpu-active", LeafID: "p",
			ExecutionSpec: runtime.ExecutionSpec{GPURequired: true},
		},
		WUResp: &lettucev1.RequestWorkUnitResponse{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/gpu-active"},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			<-blockCh
			return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
		}},
		Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}, d)

	time.Sleep(50 * time.Millisecond)

	if d.slotManager.ActiveGPUCount() != 1 {
		t.Errorf("ActiveGPUCount = %d, want 1", d.slotManager.ActiveGPUCount())
	}

	// The single GPU is busy → a second GPU WU is refused.
	if d.canAccommodateWU(gpuWU) {
		t.Error("should NOT accommodate a second GPU WU when the only GPU is busy")
	}

	// A non-GPU WU is unaffected by GPU exclusivity.
	cpuWU := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{GPURequired: false}}
	if !d.canAccommodateWU(cpuWU) {
		t.Error("should accommodate a non-GPU WU regardless of GPU occupancy")
	}

	close(blockCh)
	cancel()
	// Let the slot goroutine drain.
	time.Sleep(50 * time.Millisecond)
}
