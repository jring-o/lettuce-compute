package daemon

import "strings"

// freeMemoryHeadroomMB is the slack left above a work unit's declared memory
// requirement when checking real free system RAM. It keeps a small reserve for
// the OS and the daemon itself so admitting a unit never consumes the very last
// of available memory.
const freeMemoryHeadroomMB = 512

// freeSystemMemoryMB reports the currently-available system memory in MB. The
// bool is false when the value can't be determined on this platform, in which
// case callers should skip the real-memory admission check and fall back to the
// configured budget. Overridable in tests.
var freeSystemMemoryMB = defaultFreeSystemMemoryMB

// parseMemAvailableMB extracts MemAvailable (reported in kB) from /proc/meminfo
// content and returns it in MB. Returns ok=false if the field is absent or
// unparseable. Pure function kept untagged so it is testable on any platform.
func parseMemAvailableMB(meminfo string) (int, bool) {
	for _, line := range strings.Split(meminfo, "\n") {
		// Lines look like: "MemAvailable:   30482140 kB".
		rest, ok := strings.CutPrefix(line, "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		var kb int64
		seenDigit := false
		for i := 0; i < len(fields[0]); i++ {
			ch := fields[0][i]
			if ch < '0' || ch > '9' {
				return 0, false
			}
			kb = kb*10 + int64(ch-'0')
			seenDigit = true
		}
		if !seenDigit {
			return 0, false
		}
		return int(kb / 1024), true
	}
	return 0, false
}
