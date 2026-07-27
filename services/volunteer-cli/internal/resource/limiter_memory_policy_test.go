package resource

import "testing"

// TB-11 regression coverage for the memory ceiling the non-cgroups Linux path
// enforces. The mechanism half (which rlimit is actually set) is asserted in
// limiter_linux_test.go; this file pins the policy half, which is plain
// arithmetic and therefore checkable on every platform.
//
// The numbers are not invented — each row corresponds to a measurement taken
// against a real Go leaf binary under podman on Linux 6.6. They are the reason
// the declared value cannot be enforced verbatim.

func TestFallbackMemoryLimitMB(t *testing.T) {
	cases := []struct {
		name       string
		declaredMB int
		wantMB     int
	}{
		{"unspecified stays unlimited", 0, 0},
		{"negative stays unlimited", -1, 0},
		{"tiny declaration is floored so a runtime can start", 8, 128},
		{"16 MiB is floored", 16, 128},
		{"32 MiB lands exactly on the floor", 32, 128},
		{"64 MiB gets headroom above the floor", 64, 192},
		{"128 MiB — the beyblade-arena-native declaration", 128, 320},
		{"512 MiB scales proportionally", 512, 1088},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fallbackMemoryLimitMB(tc.declaredMB); got != tc.wantMB {
				t.Errorf("fallbackMemoryLimitMB(%d) = %d, want %d", tc.declaredMB, got, tc.wantMB)
			}
		})
	}
}

// TestFallbackMemoryLimitClearsRuntimeStartupFloor is the property that TB-11
// was a violation of: whatever a leaf declares, the enforced ceiling must leave
// a managed runtime enough room to reach main(). Measured floor is one 64 MiB
// Go heap-arena granule; anything at or below it kills the process during
// runtime init, before any leaf code runs.
func TestFallbackMemoryLimitClearsRuntimeStartupFloor(t *testing.T) {
	for declared := 1; declared <= 2048; declared++ {
		got := fallbackMemoryLimitMB(declared)
		if got <= goHeapArenaGranuleMB {
			t.Fatalf("declared %d MiB yields a %d MiB ceiling, at or below the %d MiB runtime startup floor — a leaf would die in runtime init",
				declared, got, goHeapArenaGranuleMB)
		}
	}
}

// TestFallbackMemoryLimitAdmitsDeclaredUsage guards the other edge. A leaf that
// uses exactly what it declared must fit, with room for the runtime's mapping
// granularity on top — measured, a 100 MiB working set already occupies 128 MiB
// of mapped Go arena. A ceiling that merely equalled the declaration would kill
// leaves behaving exactly as declared.
func TestFallbackMemoryLimitAdmitsDeclaredUsage(t *testing.T) {
	for _, declared := range []int{32, 64, 128, 256, 512, 1024} {
		got := fallbackMemoryLimitMB(declared)
		if got < declared+goHeapArenaGranuleMB {
			t.Errorf("declared %d MiB yields %d MiB: leaves less than one %d MiB mapping granule of headroom",
				declared, got, goHeapArenaGranuleMB)
		}
	}
}
