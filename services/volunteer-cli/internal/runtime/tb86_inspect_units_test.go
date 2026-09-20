package runtime

import (
	"os/exec"
	"testing"
)

// TB-86 regression: `podman machine inspect` reports Resources.Memory in MiB
// and Resources.DiskSize in GiB on Podman 5 (the machine package types them
// strongunits.MiB / GiB; the man page's example reads `"Memory": 6144,
// "DiskSize": 100`). The parser was written against Podman 4, which printed
// bytes, and divided both figures as bytes — so every current Podman's
// machine read "2 CPUs, 0 MiB RAM, 0 GiB disk" on the app's runtime card
// (1366 / 1048576 = 0). Both shapes must parse to the machine's real size.

// TestTB86_InspectUnitsPodman5MiBGiB is the tester's `--memory 1366` machine
// on Podman 5.8.6, inspect output verbatim in shape. Pre-fix: MemoryMB 0,
// DiskGB 0.
func TestTB86_InspectUnitsPodman5MiBGiB(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "podman-machine-default",
				"State": "running",
				"Resources": {"CPUs": 2, "DiskSize": 100, "Memory": 1366, "USBs": []},
				"ConnectionInfo": {"PodmanSocket": {"Path": "/var/folders/82/T/podman/podman-machine-default-api.sock"}}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.machineStatus()
	if info.Status != MachineRunning {
		t.Fatalf("status = %s, want running", info.Status)
	}
	if info.CPUs != 2 || info.MemoryMB != 1366 || info.DiskGB != 100 {
		t.Errorf("parsed CPUs=%d MemoryMB=%d DiskGB=%d, want 2 / 1366 / 100 (Podman 5 prints MiB and GiB; pre-fix divided them as bytes and read 0 / 0)",
			info.CPUs, info.MemoryMB, info.DiskGB)
	}
}

// TestTB86_InspectUnitsPodman4Bytes: the byte-valued output of Podman 4 (the
// shape the parser was first written against) still parses to the same
// machine size — the unit heuristic keys on magnitude, and no machine has a
// million MiB of memory or a million GiB of disk.
func TestTB86_InspectUnitsPodman4Bytes(t *testing.T) {
	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{
				"Name": "podman-machine-default",
				"State": "stopped",
				"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
				"ConnectionInfo": {}
			}]`), nil
		}
		return nil, exec.ErrNotFound
	})

	info := m.machineStatus()
	if info.CPUs != 4 || info.MemoryMB != 8192 || info.DiskGB != 20 {
		t.Errorf("parsed CPUs=%d MemoryMB=%d DiskGB=%d, want 4 / 8192 / 20 for a byte-valued Podman 4 inspect",
			info.CPUs, info.MemoryMB, info.DiskGB)
	}
}

// TestTB86_InspectUnitHelpers pins the boundary: figures up to one million
// are MiB/GiB as printed, anything above is bytes.
func TestTB86_InspectUnitHelpers(t *testing.T) {
	cases := []struct {
		in         int
		wantMemory int
		wantDisk   int
	}{
		{0, 0, 0},
		{1366, 1366, 1366},
		{6144, 6144, 6144},
		{1 << 20, 1 << 20, 1 << 20},   // one million: still MiB/GiB
		{1<<20 + 1, 1, 0},             // just above: bytes (1 MiB and change; not a whole GiB)
		{6442450944, 6144, 6},         // 6 GiB in bytes
		{107374182400, 102400, 100},   // 100 GiB in bytes
	}
	for _, c := range cases {
		if got := inspectMemoryMB(c.in); got != c.wantMemory {
			t.Errorf("inspectMemoryMB(%d) = %d, want %d", c.in, got, c.wantMemory)
		}
		if got := inspectDiskGB(c.in); got != c.wantDisk {
			t.Errorf("inspectDiskGB(%d) = %d, want %d", c.in, got, c.wantDisk)
		}
	}
}
