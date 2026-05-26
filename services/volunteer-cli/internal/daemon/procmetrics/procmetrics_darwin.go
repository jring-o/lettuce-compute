package procmetrics

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type darwinReader struct{}

func newPlatformReader() Reader {
	return &darwinReader{}
}

// Read uses `ps` to get RSS and CPU time for the given PID.
// Disk I/O is not available per-process on macOS without DTrace.
func (r *darwinReader) Read(pid int) (*ProcessMetrics, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID: %d", pid)
	}

	metrics := &ProcessMetrics{}

	// ps -o rss=,vsz= -p PID returns RSS and VSZ in KB.
	out, err := exec.Command("ps", "-o", "rss=,vsz=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return metrics, nil // process may have exited
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) >= 2 {
		if rssKB, err := strconv.ParseFloat(fields[0], 64); err == nil {
			rss := rssKB / 1024
			metrics.MemoryRSSMB = &rss
		}
		if vszKB, err := strconv.ParseFloat(fields[1], 64); err == nil {
			vsz := vszKB / 1024
			metrics.VirtualMemoryMB = &vsz
		}
	}

	// ps -o cputime= -p PID returns CPU time as HH:MM:SS or M:SS.
	out, err = exec.Command("ps", "-o", "cputime=", "-p", strconv.Itoa(pid)).Output()
	if err == nil {
		if secs := parseCPUTime(strings.TrimSpace(string(out))); secs >= 0 {
			metrics.CPUUsagePct = &secs
		}
	}

	return metrics, nil
}

// parseCPUTime parses "HH:MM:SS" or "M:SS" format to total seconds.
func parseCPUTime(s string) float64 {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 3:
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		sec, _ := strconv.ParseFloat(parts[2], 64)
		return h*3600 + m*60 + sec
	case 2:
		m, _ := strconv.ParseFloat(parts[0], 64)
		sec, _ := strconv.ParseFloat(parts[1], 64)
		return m*60 + sec
	}
	return -1
}
