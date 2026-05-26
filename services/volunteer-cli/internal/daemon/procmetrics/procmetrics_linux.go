package procmetrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type linuxReader struct{}

func newPlatformReader() Reader {
	return &linuxReader{}
}

func (r *linuxReader) Read(pid int) (*ProcessMetrics, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID: %d", pid)
	}

	metrics := &ProcessMetrics{}
	prefix := "/proc/" + strconv.Itoa(pid)

	// Memory from /proc/{pid}/status
	if data, err := os.ReadFile(prefix + "/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				if kb := parseKB(line); kb >= 0 {
					mb := float64(kb) / 1024
					metrics.MemoryRSSMB = &mb
				}
			} else if strings.HasPrefix(line, "VmSize:") {
				if kb := parseKB(line); kb >= 0 {
					mb := float64(kb) / 1024
					metrics.VirtualMemoryMB = &mb
				}
			}
		}
	}

	// CPU from /proc/{pid}/stat (fields 14=utime, 15=stime in clock ticks)
	if data, err := os.ReadFile(prefix + "/stat"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 15 {
			utime, _ := strconv.ParseFloat(fields[13], 64)
			stime, _ := strconv.ParseFloat(fields[14], 64)
			// Report as total CPU seconds (caller can diff for %).
			totalSec := (utime + stime) / 100.0 // assuming SC_CLK_TCK=100
			metrics.CPUUsagePct = &totalSec
		}
	}

	// Disk I/O from /proc/{pid}/io
	if data, err := os.ReadFile(prefix + "/io"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "read_bytes:") {
				if bytes := parseBytes(line); bytes >= 0 {
					mb := float64(bytes) / (1024 * 1024)
					metrics.DiskReadMB = &mb
				}
			} else if strings.HasPrefix(line, "write_bytes:") {
				if bytes := parseBytes(line); bytes >= 0 {
					mb := float64(bytes) / (1024 * 1024)
					metrics.DiskWrittenMB = &mb
				}
			}
		}
	}

	return metrics, nil
}

// parseKB extracts the kB value from a /proc/status line like "VmRSS:    12345 kB".
func parseKB(line string) int64 {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			return v
		}
	}
	return -1
}

// parseBytes extracts the byte count from a /proc/io line like "read_bytes: 12345".
func parseBytes(line string) int64 {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			return v
		}
	}
	return -1
}
