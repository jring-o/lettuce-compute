//go:build !windows

package runtime

import (
	"fmt"
	"syscall"
	"time"
)

// SelfCPUSeconds is the CPU time this process has used so far (user +
// system), from getrusage. It is the daemon's own share of the machine's
// load: the coordinator, the fetcher, the management API and every WASM task,
// which runs in-process.
func SelfCPUSeconds() (float64, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, fmt.Errorf("getrusage: %w", err)
	}
	return timevalSeconds(ru.Utime) + timevalSeconds(ru.Stime), nil
}

func timevalSeconds(tv syscall.Timeval) float64 {
	return time.Duration(tv.Nano()).Seconds()
}
