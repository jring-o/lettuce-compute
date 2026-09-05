package runtime

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TB-63 regression tests, runtime half.
//
// On macOS and Windows every container runs inside the engine's virtual
// machine (a Podman machine, Docker Desktop's engine VM), whose memory is the
// real ceiling for container work. The client read the machine's size and used
// it for nothing but a Settings caption, while advertising, booking and
// enforcing max_memory_mb from the configuration alone — so a 16 GB Mac with a
// 2 GiB Podman machine advertised 8192 MB, was sent 7000 MB units, and had each
// one killed by the guest kernel at model load (exit 137, under a minute), four
// times in forty minutes. ContainerMemoryBudgetMB is the one figure all three
// sites now share; EngineInfo carries the VM's memory as the engine reports it.

// TestTB63_ContainerMemoryBudgetMB pins the arithmetic: the configuration is
// clipped to the VM's memory less the headroom, never raised by it, floored at
// the smallest bookable unit, and left alone when there is no VM figure.
func TestTB63_ContainerMemoryBudgetMB(t *testing.T) {
	cases := []struct {
		name               string
		configMB, engineMB int
		want               int
	}{
		{"the tester's Mac: 8192 configured, 2 GiB machine", 8192, 2048, 2048 - ContainerVMHeadroomMB},
		{"a machine big enough to honor the configuration", 8192, 16384, 8192},
		{"a machine exactly at configuration plus headroom", 8192, 8192 + ContainerVMHeadroomMB, 8192},
		{"no VM figure (Linux, or the engine did not say)", 8192, 0, 8192},
		{"a configuration already below the machine", 1024, 2048, 1024},
		{"a machine smaller than the headroom still yields a bookable budget", 8192, 256, MinTaskMemMB},
		{"no configuration at all takes the machine's figure", 0, 2048, 2048 - ContainerVMHeadroomMB},
	}
	for _, tc := range cases {
		if got := ContainerMemoryBudgetMB(tc.configMB, tc.engineMB); got != tc.want {
			t.Errorf("%s: ContainerMemoryBudgetMB(%d, %d) = %d, want %d", tc.name, tc.configMB, tc.engineMB, got, tc.want)
		}
	}
}

// TestTB63_BuildEngineInfoCarriesTheEngineMemory: the engine's reported total
// memory (bytes) reaches EngineInfo in MB, and an engine that does not report
// it leaves the field 0 rather than a negative or garbage figure.
func TestTB63_BuildEngineInfoCarriesTheEngineMemory(t *testing.T) {
	defer withPathExists(func(string) bool { return false })()

	ei := buildEngineInfo("/var/lib/containers/storage", nil, 2048*1024*1024)
	if ei.MemTotalMB != 2048 {
		t.Errorf("MemTotalMB = %d, want 2048 for a 2 GiB engine VM", ei.MemTotalMB)
	}
	for _, bytes := range []int64{0, -1} {
		if ei := buildEngineInfo("/var/lib/docker", nil, bytes); ei.MemTotalMB != 0 {
			t.Errorf("MemTotalMB = %d for a reported %d bytes, want 0 (unknown)", ei.MemTotalMB, bytes)
		}
	}
}

// TestTB63_ContainerRuntimeRecordsTheEngineMemory: the runtime keeps the VM
// figure beside its ceiling so the daemon's diagnostics can name both.
func TestTB63_ContainerRuntimeRecordsTheEngineMemory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cr := NewContainerRuntimeWithClient(t.TempDir(), logger, &MockDockerClient{})
	if cr.EngineMemoryMB() != 0 || cr.MemoryCeilingMB() != 0 {
		t.Fatalf("fresh runtime: engine %d MB, ceiling %d MB, want both 0", cr.EngineMemoryMB(), cr.MemoryCeilingMB())
	}
	cr.SetEngineMemoryMB(2048)
	cr.SetMemoryCeilingMB(ContainerMemoryBudgetMB(8192, 2048))
	if cr.EngineMemoryMB() != 2048 || cr.MemoryCeilingMB() != 1536 {
		t.Errorf("engine %d MB, ceiling %d MB, want 2048 / 1536", cr.EngineMemoryMB(), cr.MemoryCeilingMB())
	}
	// The ceiling is what BookedMemMB clamps a unit to: a 7000 MB declaration is
	// booked at 1536, so a unit that arrives anyway is bounded by its cgroup
	// rather than left to the VM's global out-of-memory killer.
	if got := BookedMemMB(7000, cr.MemoryCeilingMB()); got != 1536 {
		t.Errorf("BookedMemMB(7000, ceiling) = %d, want 1536", got)
	}
}

// TestTB63_ContainerEngineRunsInVMFollowsThePlatform: the default answer is
// the machine platforms (Windows, macOS), and the seam can be overridden.
func TestTB63_ContainerEngineRunsInVMFollowsThePlatform(t *testing.T) {
	if got, want := ContainerEngineRunsInVM(), needsMachine(); got != want {
		t.Errorf("ContainerEngineRunsInVM() = %v, want %v (needsMachine) on this platform", got, want)
	}
	orig := ContainerEngineRunsInVM
	defer func() { ContainerEngineRunsInVM = orig }()
	ContainerEngineRunsInVM = func() bool { return true }
	if !ContainerEngineRunsInVM() {
		t.Error("the seam did not take")
	}
}

// TestTB63_RealEngine_InfoReportsMemTotal runs against a real engine: the
// production Info path must carry a positive MemTotalMB, because the whole fix
// rests on that figure being available from the engine rather than from
// `podman machine inspect` (whose number, on WSL, is not what the VM has —
// 2048 MB recorded against ~48 GB actual on the box this was written on).
func TestTB63_RealEngine_InfoReportsMemTotal(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := cr.Client().Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.MemTotalMB <= 0 {
		t.Fatalf("MemTotalMB = %d from a real engine, want > 0", info.MemTotalMB)
	}
	t.Logf("real engine reports MemTotalMB = %d (ContainerMemoryBudgetMB(8192, that) = %d)",
		info.MemTotalMB, ContainerMemoryBudgetMB(8192, int(info.MemTotalMB)))
}
