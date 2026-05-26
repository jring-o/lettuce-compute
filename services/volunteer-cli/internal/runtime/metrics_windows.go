//go:build windows

package runtime

import (
	"log/slog"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                  = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemInfo  = psapi.NewProc("GetProcessMemoryInfo")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessIoCounts = kernel32.NewProc("GetProcessIoCounters")
)

// collectPlatformMetrics populates peak memory and disk I/O on Windows
// using GetProcessMemoryInfo and GetProcessIoCounters.
func collectPlatformMetrics(cmd *exec.Cmd, metrics *ExecutionMetrics) {
	if cmd.ProcessState == nil {
		return
	}

	pid := cmd.ProcessState.Pid()
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)

	// Peak memory via PROCESS_MEMORY_COUNTERS.
	type processMemoryCounters struct {
		CB                         uint32
		PageFaultCount             uint32
		PeakWorkingSetSize         uintptr
		WorkingSetSize             uintptr
		QuotaPeakPagedPoolUsage    uintptr
		QuotaPagedPoolUsage        uintptr
		QuotaPeakNonPagedPoolUsage uintptr
		QuotaNonPagedPoolUsage     uintptr
		PagefileUsage              uintptr
		PeakPagefileUsage          uintptr
	}

	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ret, _, _ := procGetProcessMemInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&pmc)),
		uintptr(pmc.CB),
	)
	if ret != 0 {
		metrics.PeakMemoryMB = int32(pmc.PeakWorkingSetSize / (1024 * 1024))
	}

	// Disk I/O via GetProcessIoCounters.
	type ioCounters struct {
		ReadOperationCount  uint64
		WriteOperationCount uint64
		OtherOperationCount uint64
		ReadTransferCount   uint64
		WriteTransferCount  uint64
		OtherTransferCount  uint64
	}

	var ioc ioCounters
	ret, _, _ = procGetProcessIoCounts.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&ioc)),
	)
	if ret != 0 {
		metrics.DiskReadMB = int64(ioc.ReadTransferCount / (1024 * 1024))
		metrics.DiskWriteMB = int64(ioc.WriteTransferCount / (1024 * 1024))
	}
}

// DiskIOMonitor is a no-op on Windows since disk I/O is collected
// via GetProcessIoCounters in collectPlatformMetrics after process exit.
type DiskIOMonitor struct{}

// DiskReadMB returns 0 on Windows (I/O collected via collectPlatformMetrics).
func (m *DiskIOMonitor) DiskReadMB() int64 { return 0 }

// DiskWriteMB returns 0 on Windows (I/O collected via collectPlatformMetrics).
func (m *DiskIOMonitor) DiskWriteMB() int64 { return 0 }

// startDiskIOMonitor is a no-op on Windows.
func startDiskIOMonitor(_ int, _ *slog.Logger) (cleanup func(), monitor *DiskIOMonitor) {
	return func() {}, &DiskIOMonitor{}
}

// applyDiskIOMetrics is a no-op on Windows (disk I/O already collected via Windows APIs).
func applyDiskIOMetrics(_ *DiskIOMonitor, _ *ExecutionMetrics) {}
