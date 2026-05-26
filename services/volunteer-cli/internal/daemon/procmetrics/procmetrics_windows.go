package procmetrics

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessMemoryInfo = modkernel32.NewProc("K32GetProcessMemoryInfo")
	procGetProcessTimes      = modkernel32.NewProc("GetProcessTimes")
)

// processMemoryCounters matches PROCESS_MEMORY_COUNTERS from Windows API.
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

type windowsReader struct{}

func newPlatformReader() Reader {
	return &windowsReader{}
}

func (r *windowsReader) Read(pid int) (*ProcessMetrics, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID: %d", pid)
	}

	const PROCESS_QUERY_INFORMATION = 0x0400
	const PROCESS_VM_READ = 0x0010

	handle, err := syscall.OpenProcess(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(handle)

	metrics := &ProcessMetrics{}

	// Memory: GetProcessMemoryInfo
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ret, _, err := procGetProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&pmc)),
		uintptr(pmc.CB),
	)
	if ret != 0 {
		rss := float64(pmc.WorkingSetSize) / (1024 * 1024)
		virt := float64(pmc.PagefileUsage) / (1024 * 1024)
		metrics.MemoryRSSMB = &rss
		metrics.VirtualMemoryMB = &virt
	}

	// CPU: GetProcessTimes
	var creationTime, exitTime, kernelTime, userTime syscall.Filetime
	ret, _, err = procGetProcessTimes.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&creationTime)),
		uintptr(unsafe.Pointer(&exitTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if ret != 0 {
		// Convert FILETIME (100ns intervals) to seconds.
		kernel := float64(uint64(kernelTime.HighDateTime)<<32|uint64(kernelTime.LowDateTime)) / 1e7
		user := float64(uint64(userTime.HighDateTime)<<32|uint64(userTime.LowDateTime)) / 1e7
		totalCPU := kernel + user
		metrics.CPUUsagePct = &totalCPU
	}

	// Disk I/O not available per-process on Windows without NtQueryInformationProcess.
	return metrics, nil
}
