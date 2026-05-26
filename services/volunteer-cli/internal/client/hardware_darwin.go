//go:build darwin

package client

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	gpudetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func defaultDetectCPUModel() string {
	// Bound sysctl with DetectionCommandTimeout so a wedged tool cannot block
	// Register; degrade to "unknown" on any failure.
	ctx, cancel := context.WithTimeout(context.Background(), gpudetect.DetectionCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func defaultDetectTotalMemoryMB() int32 {
	ctx, cancel := context.WithTimeout(context.Background(), gpudetect.DetectionCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return int32(bytes / (1024 * 1024))
}

func defaultDetectDiskAvailableMB(path string) int64 {
	if path == "" {
		path = "."
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail * uint64(stat.Bsize) / (1024 * 1024))
}
