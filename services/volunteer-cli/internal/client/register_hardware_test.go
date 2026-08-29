package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sync/atomic"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	gpudetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Registration advertises the hardware the caller already detected and runs
// no detection of its own. Detection launches vendor tools and reads
// platform registries; when Register re-ran it for every head, a start-up
// probed the machine once per head plus once for the daemon — and on
// Windows each probe could raise its own UAC prompt.
func TestRegister_ReusesDetectedHardware(t *testing.T) {
	withMockHardware(t)

	// Any detection during Register is a failure: every sub-detection seam
	// counts its calls.
	var probes atomic.Int32
	origCPU, origMem, origDisk := detectCPUModel, detectTotalMemoryMB, detectDiskAvailableMB
	origCmd, origAdapters := gpudetect.CommandExecutorCtx, gpudetect.DisplayAdapterSource
	t.Cleanup(func() {
		detectCPUModel, detectTotalMemoryMB, detectDiskAvailableMB = origCPU, origMem, origDisk
		gpudetect.CommandExecutorCtx, gpudetect.DisplayAdapterSource = origCmd, origAdapters
	})
	detectCPUModel = func() string { probes.Add(1); return "probed" }
	detectTotalMemoryMB = func() int32 { probes.Add(1); return 1 }
	detectDiskAvailableMB = func(string) int64 { probes.Add(1); return 1 }
	gpudetect.CommandExecutorCtx = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		probes.Add(1)
		return nil, nil
	}
	gpudetect.DisplayAdapterSource = func() (gpudetect.DisplayAdapterReader, error) {
		probes.Add(1)
		return nil, gpudetect.ErrDisplayAdaptersUnsupported
	}

	mock := &mockVolunteerService{}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()
	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cfg := config.Defaults()
	cached := &lettucev1.HardwareCapabilities{CpuModel: "cached-cpu", CpuCores: 12, MemoryTotalMb: 65536}

	if _, _, _, err := Register(context.Background(), client, pub, nil, "", cfg, filepath.Join(t.TempDir(), "config.yaml"), cached, "NATIVE"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if n := probes.Load(); n != 0 {
		t.Errorf("Register ran hardware detection %d time(s); it must advertise the hardware it was given", n)
	}
	if mock.registerReq == nil {
		t.Fatal("no RegisterVolunteer request reached the head")
	}
	if got := mock.registerReq.GetHardware(); got.GetCpuModel() != "cached-cpu" || got.GetCpuCores() != 12 || got.GetMemoryTotalMb() != 65536 {
		t.Errorf("advertised hardware = %+v, want the cached capabilities", got)
	}
}
