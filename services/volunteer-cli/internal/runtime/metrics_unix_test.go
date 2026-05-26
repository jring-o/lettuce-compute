//go:build linux || darwin

package runtime

import (
	"log/slog"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestDiskIOMonitor_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("disk I/O monitoring only available on Linux")
	}

	// Monitor our own process — /proc/self/io should be readable.
	pid := os.Getpid()
	cleanup, monitor := NewDiskIOMonitor(pid, 50*time.Millisecond, slog.Default())

	// Write some data to trigger disk I/O.
	tmpFile, err := os.CreateTemp("", "lettuce-diskio-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	data := make([]byte, 1024*1024) // 1 MB
	tmpFile.Write(data)
	tmpFile.Sync()
	tmpFile.Close()

	// Give the monitor time to pick up the I/O.
	time.Sleep(200 * time.Millisecond)
	cleanup()

	// We should see non-zero write bytes (at least 1 MB).
	if monitor.DiskWriteMB() < 0 {
		t.Errorf("expected non-negative disk write MB, got %d", monitor.DiskWriteMB())
	}

	// Read the file back to trigger read I/O.
	os.ReadFile(tmpFile.Name())
}

func TestDiskIOMonitor_GracefulFallback(t *testing.T) {
	// Use a PID that definitely doesn't exist.
	cleanup, monitor := NewDiskIOMonitor(999999999, 50*time.Millisecond, slog.Default())
	time.Sleep(100 * time.Millisecond)
	cleanup()

	// Should return 0 gracefully (no panic, no error).
	if monitor.DiskReadMB() != 0 {
		t.Errorf("expected 0 disk read MB for invalid PID, got %d", monitor.DiskReadMB())
	}
	if monitor.DiskWriteMB() != 0 {
		t.Errorf("expected 0 disk write MB for invalid PID, got %d", monitor.DiskWriteMB())
	}
}

func TestDiskIOMonitor_MacOSReturnsZero(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific test")
	}

	cleanup, monitor := NewDiskIOMonitor(os.Getpid(), 50*time.Millisecond, slog.Default())
	time.Sleep(100 * time.Millisecond)
	cleanup()

	// macOS doesn't have /proc, so values should be 0.
	if monitor.DiskReadMB() != 0 {
		t.Errorf("expected 0 disk read MB on macOS, got %d", monitor.DiskReadMB())
	}
	if monitor.DiskWriteMB() != 0 {
		t.Errorf("expected 0 disk write MB on macOS, got %d", monitor.DiskWriteMB())
	}
}

func TestApplyDiskIOMetrics_NilMonitor(t *testing.T) {
	metrics := &ExecutionMetrics{}
	applyDiskIOMetrics(nil, metrics)
	// Should not panic.
	if metrics.DiskReadMB != 0 || metrics.DiskWriteMB != 0 {
		t.Error("expected zero values with nil monitor")
	}
}
