//go:build linux

package resource

import (
	"log/slog"
	"syscall"
	"testing"
	"unsafe"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// readRlimit returns a live process's current limit for one resource. It reads
// through prlimit64's old_limit out-parameter — the same kernel interface
// enforceFallback writes through — so the assertions describe what the kernel
// actually holds rather than what the Go wrapper reports.
func readRlimit(t *testing.T, pid, resource int) rlimit64 {
	t.Helper()
	var out rlimit64
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PRLIMIT64,
		uintptr(pid),
		uintptr(resource),
		0, // new_limit: nil, read-only
		uintptr(unsafe.Pointer(&out)),
		0, 0,
	)
	if errno != 0 {
		t.Fatalf("prlimit64 read of resource %d on pid %d: %v", resource, pid, errno)
	}
	return out
}

// TB-11: the fallback path capped RLIMIT_AS — virtual ADDRESS SPACE — at the
// leaf's declared memory. Managed runtimes reserve far more address space than
// they commit, so a Go leaf died inside runtime.mallocinit() before executing a
// line of its own code, on every unprivileged Linux machine (which is to say,
// most volunteers). The fix enforces RLIMIT_DATA — committed memory — with
// headroom.
//
// These assertions read the limits back off a live child with prlimit64, so they
// describe what the kernel was actually told, not what the code appears to say.

// TestEnforceFallbackSetsDataLimitNotAddressSpace is the regression test. It
// fails against the pre-fix code twice over: RLIMIT_DATA is left untouched, and
// RLIMIT_AS is clamped to the declared 128 MiB — the value measured to kill a Go
// process during runtime initialization.
func TestEnforceFallbackSetsDataLimitNotAddressSpace(t *testing.T) {
	// Construct the fallback limiter directly rather than letting
	// detectCgroupsV2 decide. A skip-if-cgroups guard would silently retire this
	// test on any machine that happens to have delegation — including, one day,
	// CI — and the fallback is precisely the path most volunteers land on.
	l := &LinuxLimiter{logger: slog.Default(), useCgroups: false}

	pid := startLimiterTestChild(t)

	// The child inherits our own limits, so compare against what it had rather
	// than against an assumed RLIM_INFINITY.
	asBefore := readRlimit(t, pid, syscall.RLIMIT_AS)

	const declaredMB = 128
	limits := &config.ResourceLimits{MaxCPUCores: 1, MaxMemoryMB: declaredMB}
	cleanup, err := l.Enforce(pid, limits)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	defer cleanup()

	// RLIMIT_DATA carries the headroomed ceiling.
	data := readRlimit(t, pid, syscall.RLIMIT_DATA)
	wantBytes := uint64(fallbackMemoryLimitMB(declaredMB)) * 1024 * 1024
	if data.Cur != wantBytes {
		t.Errorf("RLIMIT_DATA = %d bytes (%d MiB), want %d bytes (%d MiB)",
			data.Cur, data.Cur/1024/1024, wantBytes, wantBytes/1024/1024)
	}

	// ...and RLIMIT_AS is left exactly as it was. Setting it at all is what made
	// the whole NATIVE runtime unusable.
	asAfter := readRlimit(t, pid, syscall.RLIMIT_AS)
	if asAfter != asBefore {
		t.Errorf("RLIMIT_AS was modified (%d -> %d); the address-space cap is what killed native leaves",
			asBefore.Cur, asAfter.Cur)
	}
	if asAfter.Cur == uint64(declaredMB)*1024*1024 {
		t.Error("RLIMIT_AS was clamped to the declared memory — this is exactly the TB-11 failure")
	}
}

// TestEnforceFallbackLeavesMemoryUnlimitedWhenUndeclared: a work unit that
// declares no memory ceiling must not acquire one by accident.
func TestEnforceFallbackLeavesMemoryUnlimitedWhenUndeclared(t *testing.T) {
	// Construct the fallback limiter directly rather than letting
	// detectCgroupsV2 decide. A skip-if-cgroups guard would silently retire this
	// test on any machine that happens to have delegation — including, one day,
	// CI — and the fallback is precisely the path most volunteers land on.
	l := &LinuxLimiter{logger: slog.Default(), useCgroups: false}

	pid := startLimiterTestChild(t)

	before := readRlimit(t, pid, syscall.RLIMIT_DATA)

	cleanup, err := l.Enforce(pid, &config.ResourceLimits{MaxCPUCores: 1, MaxMemoryMB: 0})
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	defer cleanup()

	after := readRlimit(t, pid, syscall.RLIMIT_DATA)
	if after != before {
		t.Errorf("RLIMIT_DATA changed (%d -> %d) for a unit declaring no memory ceiling", before.Cur, after.Cur)
	}
}
