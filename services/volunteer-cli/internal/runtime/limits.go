package runtime

// Resource booking shared by admission (the daemon) and enforcement (the
// native/container/wasm runtimes). The whole point of BG-16 is that the number
// admission RESERVES for a unit and the number a runtime ENFORCES on it are the
// same object: an estimate is not an enforcement. Both sides call these functions,
// so the booked denominator and the enforced ceiling can never drift.

const (
	// DefaultPerTaskMemMB is the memory ceiling (MB) applied to a unit that
	// declares no memory (MaxMemoryMB == 0). It matches the historical admission
	// estimate, but is now also ENFORCED, so a declared-0 unit is bounded rather
	// than unlimited (the container/wasm OOM in BG-16).
	DefaultPerTaskMemMB = 512
	// MinTaskMemMB is a small floor so a tiny declared value still leaves room for
	// runtime overhead.
	MinTaskMemMB = 16

	// DefaultPerTaskDiskMB is the /work disk ceiling (MB) for a unit that declares
	// no disk (MaxDiskMB == 0); it matches the head's own 0->10240 default.
	DefaultPerTaskDiskMB = 10 * 1024
	// MinTaskDiskMB is a small /work floor.
	MinTaskDiskMB = 16
)

// BookedMemMB returns the memory ceiling (MB) that is BOTH booked at admission and
// enforced at runtime for a unit declaring declaredMB (0 = unspecified). The value
// is clamped into [MinTaskMemMB, ceilingMB], where ceilingMB is the volunteer's
// configured whole-machine budget (config.ResourceLimits.MaxMemoryMB). A
// non-positive ceilingMB means "no configured ceiling" and is not applied.
//
// This is the load-bearing BG-16 fix: a leaf that declares 0 (hoping for unlimited)
// is clamped to DefaultPerTaskMemMB and that number is enforced; a leaf that
// declares a huge value is clamped down to the volunteer's own budget.
func BookedMemMB(declaredMB, ceilingMB int) int {
	v := declaredMB
	if v <= 0 {
		v = DefaultPerTaskMemMB
	}
	if v < MinTaskMemMB {
		v = MinTaskMemMB
	}
	if ceilingMB > 0 && v > ceilingMB {
		v = ceilingMB
	}
	return v
}

// BookedDiskMB returns the /work disk ceiling (MB) both reserved and enforced for a
// unit declaring declaredMB (0 = unspecified), clamped into [MinTaskDiskMB,
// ceilingMB] where ceilingMB is the volunteer's configured disk budget
// (config.ResourceLimits.MaxDiskGB * 1024).
//
// The disk watchdog is driven by THIS value, never by the attacker-declared
// MaxDiskMB — which the head accepts with no upper clamp, so a leaf can declare
// ~50 TB and a watchdog keyed on it would never fire before the real disk fills
// (BG-16c / design finding #2). Clamping to the config ceiling terminates the unit
// at the volunteer's own limit instead.
func BookedDiskMB(declaredMB, ceilingMB int) int {
	v := declaredMB
	if v <= 0 {
		v = DefaultPerTaskDiskMB
	}
	if v < MinTaskDiskMB {
		v = MinTaskDiskMB
	}
	if ceilingMB > 0 && v > ceilingMB {
		v = ceilingMB
	}
	return v
}
