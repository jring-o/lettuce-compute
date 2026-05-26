//go:build linux

package daemon

import "os"

// defaultFreeSystemMemoryMB reads MemAvailable from /proc/meminfo, the kernel's
// best estimate of memory available for starting new applications without
// swapping. Reading a proc file is a plain file read (no external command).
func defaultFreeSystemMemoryMB() (int, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemAvailableMB(string(data))
}
