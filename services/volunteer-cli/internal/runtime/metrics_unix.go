//go:build linux || darwin

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// collectPlatformMetrics populates peak memory from Unix rusage.
func collectPlatformMetrics(cmd *exec.Cmd, metrics *ExecutionMetrics) {
	if cmd.ProcessState == nil {
		return
	}
	rusage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok || rusage == nil {
		return
	}
	// Maxrss is in kilobytes on Linux, bytes on macOS.
	// Convert to MB.
	maxrss := rusage.Maxrss
	if goruntime.GOOS == "darwin" {
		// macOS reports bytes.
		metrics.PeakMemoryMB = int32(maxrss / (1024 * 1024))
	} else {
		// Linux reports kilobytes.
		metrics.PeakMemoryMB = int32(maxrss / 1024)
	}
}

// DiskIOMonitor reads /proc/[pid]/io periodically to capture disk I/O
// metrics before the process exits (since /proc entries disappear on exit).
type DiskIOMonitor struct {
	mu         sync.Mutex
	readBytes  int64
	writeBytes int64
	logger     *slog.Logger
}

// NewDiskIOMonitor creates a monitor that polls /proc/[pid]/io every interval.
// It returns a cleanup function that stops monitoring and returns the final values.
func NewDiskIOMonitor(pid int, interval time.Duration, logger *slog.Logger) (cleanup func(), monitor *DiskIOMonitor) {
	m := &DiskIOMonitor{logger: logger}

	if goruntime.GOOS != "linux" {
		// macOS doesn't expose per-process disk I/O via /proc.
		logger.Debug("disk I/O monitoring not available on this platform")
		return func() {}, m
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		procPath := fmt.Sprintf("/proc/%d/io", pid)
		for {
			select {
			case <-done:
				// Final read before stopping.
				m.readProcIO(procPath)
				return
			case <-ticker.C:
				m.readProcIO(procPath)
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done) }) }, m
}

// readProcIO reads /proc/[pid]/io and updates the stored values.
func (m *DiskIOMonitor) readProcIO(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var readBytes, writeBytes int64
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		val, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		switch parts[0] {
		case "read_bytes":
			readBytes = val
		case "write_bytes":
			writeBytes = val
		}
	}

	m.mu.Lock()
	m.readBytes = readBytes
	m.writeBytes = writeBytes
	m.mu.Unlock()
}

// DiskReadMB returns accumulated disk reads in MB.
func (m *DiskIOMonitor) DiskReadMB() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readBytes / (1024 * 1024)
}

// DiskWriteMB returns accumulated disk writes in MB.
func (m *DiskIOMonitor) DiskWriteMB() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBytes / (1024 * 1024)
}

// startDiskIOMonitor starts a disk I/O monitor for the given pid.
func startDiskIOMonitor(pid int, logger *slog.Logger) (cleanup func(), monitor *DiskIOMonitor) {
	return NewDiskIOMonitor(pid, 500*time.Millisecond, logger)
}

// applyDiskIOMetrics copies disk I/O values from the monitor into metrics.
func applyDiskIOMetrics(monitor *DiskIOMonitor, metrics *ExecutionMetrics) {
	if monitor == nil {
		return
	}
	metrics.DiskReadMB = monitor.DiskReadMB()
	metrics.DiskWriteMB = monitor.DiskWriteMB()
}
