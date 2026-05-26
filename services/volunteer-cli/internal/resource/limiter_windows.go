//go:build windows

package resource

import (
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

var (
	kernel32W = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW         = kernel32W.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32W.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32W.NewProc("AssignProcessToJobObject")
	procOpenProcess              = kernel32W.NewProc("OpenProcess")
	procCloseHandle              = kernel32W.NewProc("CloseHandle")
	procGetDiskFreeSpaceExW      = kernel32W.NewProc("GetDiskFreeSpaceExW")
)

const (
	processAllAccess = 0x001FFFFF

	infoClassExtendedLimit  = 9  // JobObjectExtendedLimitInformation
	infoClassCpuRateControl = 15 // JobObjectCpuRateControlInformation

	jobObjectLimitProcessMemory    = 0x00000100
	jobObjectCpuRateControlEnable  = 0x1
	jobObjectCpuRateControlHardCap = 0x4
)

// Windows Job Object structures (64-bit layout).

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobobjectBasicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobobjectExtendedLimitInfo struct {
	BasicLimitInformation jobobjectBasicLimitInfo
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobobjectCpuRateControlInfo struct {
	ControlFlags uint32
	CpuRate      uint32 // union field; we only use CpuRate
}

// WindowsLimiter enforces resource limits using Windows Job Objects.
type WindowsLimiter struct {
	logger *slog.Logger
}

func newPlatformLimiter(logger *slog.Logger) Limiter {
	return NewWindowsLimiter(logger)
}

// NewWindowsLimiter creates a limiter for Windows.
func NewWindowsLimiter(logger *slog.Logger) *WindowsLimiter {
	return &WindowsLimiter{logger: logger}
}

// Apply is a no-op on Windows. Limits are applied post-start via Job Objects
// in Enforce(). Avoiding CREATE_SUSPENDED eliminates the need to enumerate
// and resume threads, which Go's os/exec does not natively support.
func (w *WindowsLimiter) Apply(cmd *exec.Cmd, limits *config.ResourceLimits) error {
	return nil
}

// Enforce creates a Job Object, configures memory and CPU limits, and assigns
// the process to the job. Returns a cleanup function that closes the handles.
func (w *WindowsLimiter) Enforce(pid int, limits *config.ResourceLimits) (func(), error) {
	// Create anonymous Job Object.
	jobHandle, _, err := procCreateJobObjectW.Call(0, 0)
	if jobHandle == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// Set memory limit.
	if limits.MaxMemoryMB > 0 {
		info := jobobjectExtendedLimitInfo{}
		info.BasicLimitInformation.LimitFlags = jobObjectLimitProcessMemory
		info.ProcessMemoryLimit = uintptr(limits.MaxMemoryMB) * 1024 * 1024

		ret, _, callErr := procSetInformationJobObject.Call(
			jobHandle,
			uintptr(infoClassExtendedLimit),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		if ret == 0 {
			procCloseHandle.Call(jobHandle)
			return nil, fmt.Errorf("SetInformationJobObject (memory): %w", callErr)
		}
		w.logger.Debug("set memory limit", "limit_mb", limits.MaxMemoryMB)
	}

	// Set CPU rate control.
	if limits.MaxCPUCores > 0 {
		numCPU := runtime.NumCPU()
		// CpuRate is percentage * 100 (e.g., 50% = 5000, 100% = 10000).
		rate := uint32((limits.MaxCPUCores * 10000) / numCPU)
		if rate > 10000 {
			rate = 10000
		}
		if rate < 100 {
			rate = 100 // minimum 1%
		}

		cpuInfo := jobobjectCpuRateControlInfo{
			ControlFlags: jobObjectCpuRateControlEnable | jobObjectCpuRateControlHardCap,
			CpuRate:      rate,
		}

		ret, _, callErr := procSetInformationJobObject.Call(
			jobHandle,
			uintptr(infoClassCpuRateControl),
			uintptr(unsafe.Pointer(&cpuInfo)),
			unsafe.Sizeof(cpuInfo),
		)
		if ret == 0 {
			procCloseHandle.Call(jobHandle)
			return nil, fmt.Errorf("SetInformationJobObject (CPU rate): %w", callErr)
		}
		w.logger.Debug("set CPU rate", "rate_per_10000", rate, "max_cores", limits.MaxCPUCores, "total_cores", numCPU)
	}

	// Open the process by PID and assign to job.
	procHandle, _, err := procOpenProcess.Call(processAllAccess, 0, uintptr(pid))
	if procHandle == 0 {
		procCloseHandle.Call(jobHandle)
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}

	ret, _, err := procAssignProcessToJobObject.Call(jobHandle, procHandle)
	procCloseHandle.Call(procHandle)
	if ret == 0 {
		procCloseHandle.Call(jobHandle)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	w.logger.Info("process assigned to job object", "pid", pid)

	cleanup := func() {
		procCloseHandle.Call(jobHandle)
	}
	return cleanup, nil
}

// CheckDiskSpace verifies that at least requiredMB of disk space is available
// on the volume containing path.
func (w *WindowsLimiter) CheckDiskSpace(path string, requiredMB int) error {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	var freeBytes, totalBytes, totalFreeBytes uint64
	ret, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytes)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return fmt.Errorf("GetDiskFreeSpaceEx: %w", callErr)
	}

	availableMB := freeBytes / (1024 * 1024)
	if availableMB < uint64(requiredMB) {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required", availableMB, requiredMB)
	}

	return nil
}
