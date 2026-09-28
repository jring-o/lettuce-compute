package runtime

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// CPUThrottle counts, since a task started, the CPU quota periods in which it
// ran and those in which its quota stopped it. A task that runs no more
// threads than the cores it was granted is rarely stopped; one that runs more
// is stopped in most periods, and each stop stalls the whole task until the
// next period.
type CPUThrottle struct {
	Periods   uint64
	Throttled uint64
}

// ContainerCPUThrottling reads a running container's CPU throttling: from the
// engine's stats where it reports it (Docker), else from the container's own
// cgroup — its cpu.stat, read inside the container through the engine, since
// Podman's stats send zeros. It is an error when neither answers (an image
// with no cat, a cgroup with no cpu.stat): there is then no reading.
func ContainerCPUThrottling(ctx context.Context, dc DockerClient, containerID string) (CPUThrottle, error) {
	if s, err := dc.ContainerUsage(ctx, containerID); err == nil && s.ThrottlePeriods > 0 {
		return CPUThrottle{Periods: s.ThrottlePeriods, Throttled: s.ThrottledPeriods}, nil
	}
	lastErr := fmt.Errorf("no cpu.stat")
	// cgroup v2, then v1's cpu controller.
	for _, path := range []string{"/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/cpu/cpu.stat"} {
		out, err := dc.ContainerExecOutput(ctx, containerID, []string{"cat", path})
		if err != nil {
			lastErr = err
			continue
		}
		if t, ok := ParseCPUStat(out); ok {
			return t, nil
		}
	}
	return CPUThrottle{}, fmt.Errorf("read CPU throttling: %w", lastErr)
}

// ParseCPUStat reads nr_periods and nr_throttled from a cgroup's cpu.stat
// (cgroup v1 and v2 name them alike). False when either is missing.
func ParseCPUStat(b []byte) (CPUThrottle, bool) {
	var t CPUThrottle
	var periods, throttled bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "nr_periods":
			t.Periods, periods = n, true
		case "nr_throttled":
			t.Throttled, throttled = n, true
		}
	}
	return t, periods && throttled
}
