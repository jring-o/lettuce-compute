package runtime

import "testing"

// TestBookedMemMB is the BG-16 memory-clamp contract: declared-0 is bounded to the
// per-task default (not unlimited), a huge declaration is clamped to the config
// ceiling, and a tiny one is floored. The same function feeds both admission and
// enforcement, so these are exactly the numbers each runtime caps at.
func TestBookedMemMB(t *testing.T) {
	const ceiling = 2048
	cases := []struct {
		name     string
		declared int
		ceiling  int
		want     int
	}{
		{"declared-zero-uses-default", 0, ceiling, DefaultPerTaskMemMB},
		{"negative-uses-default", -5, ceiling, DefaultPerTaskMemMB},
		{"tiny-is-floored", 1, ceiling, MinTaskMemMB},
		{"in-range-passes-through", 1024, ceiling, 1024},
		{"huge-is-clamped-to-ceiling", 50_000_000, ceiling, ceiling},
		{"exactly-ceiling", ceiling, ceiling, ceiling},
		{"no-ceiling-keeps-declared", 100_000, 0, 100_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BookedMemMB(c.declared, c.ceiling); got != c.want {
				t.Errorf("BookedMemMB(%d, %d) = %d, want %d", c.declared, c.ceiling, got, c.want)
			}
		})
	}
}

// TestBookedDiskMB is the BG-16c disk-clamp contract (design finding #2): a unit
// that over-declares MaxDiskMB far above the config budget is clamped to the config
// ceiling, so the watchdog terminates at the volunteer's limit rather than never
// firing.
func TestBookedDiskMB(t *testing.T) {
	const ceiling = 10 * 1024 // 10 GB in MB
	cases := []struct {
		name     string
		declared int
		ceiling  int
		want     int
	}{
		{"declared-zero-uses-default", 0, ceiling, DefaultPerTaskDiskMB},
		{"tiny-is-floored", 1, ceiling, MinTaskDiskMB},
		{"in-range-passes-through", 4096, ceiling, 4096},
		{"over-declared-50TB-clamped-to-ceiling", 50_000_000, ceiling, ceiling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BookedDiskMB(c.declared, c.ceiling); got != c.want {
				t.Errorf("BookedDiskMB(%d, %d) = %d, want %d", c.declared, c.ceiling, got, c.want)
			}
		})
	}
}
