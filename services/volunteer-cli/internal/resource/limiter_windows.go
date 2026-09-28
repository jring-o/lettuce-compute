//go:build windows

package resource

import (
	"fmt"
	"log/slog"
	"os/exec"
	goruntime "runtime"
	"sync"
	"syscall"
	"unsafe"
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

	jobObjectLimitPriorityClass    = 0x00000020
	jobObjectLimitProcessMemory    = 0x00000100
	jobObjectCpuRateControlEnable  = 0x1
	jobObjectCpuRateControlHardCap = 0x4

	idlePriorityClass = 0x00000040 // IDLE_PRIORITY_CLASS
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

	// jobs keeps each enforced process's Job Object handle until its cleanup
	// runs, so the limits a running task is held to can be read back from its
	// job (the limiter's tests do).
	mu   sync.Mutex
	jobs map[int]uintptr
}

func newPlatformLimiter(logger *slog.Logger) Limiter {
	return NewWindowsLimiter(logger)
}

// NewWindowsLimiter creates a limiter for Windows.
func NewWindowsLimiter(logger *slog.Logger) *WindowsLimiter {
	return &WindowsLimiter{logger: logger, jobs: make(map[int]uintptr)}
}

// Apply is a no-op on Windows. Limits are applied post-start via Job Objects
// in Enforce(). Avoiding CREATE_SUSPENDED eliminates the need to enumerate
// and resume threads, which Go's os/exec does not natively support.
func (w *WindowsLimiter) Apply(cmd *exec.Cmd, limits *TaskLimits) error {
	return nil
}

// cpuRateFor converts a task's CPU grant into a Job Object CPU rate: the
// grant as a percentage of the machine's CPUs, in hundredths of a percent
// (50 % = 5000, 100 % = 10000), clamped to the API's 1 %–100 % range. A
// grant of 3 cores on an 8-CPU machine is 37.5 % → 3750.
func cpuRateFor(cores, numCPU int) uint32 {
	if numCPU < 1 {
		numCPU = 1
	}
	rate := uint32(cores * 10000 / numCPU)
	if rate > 10000 {
		rate = 10000
	}
	if rate < 100 {
		rate = 100 // minimum 1%
	}
	return rate
}

// setJobCPURate applies a hard CPU-rate cap to a Job Object, or lifts it when
// cores is 0.
func (w *WindowsLimiter) setJobCPURate(jobHandle uintptr, cores int) error {
	cpuInfo := jobobjectCpuRateControlInfo{}
	if cores > 0 {
		cpuInfo.ControlFlags = jobObjectCpuRateControlEnable | jobObjectCpuRateControlHardCap
		cpuInfo.CpuRate = cpuRateFor(cores, goruntime.NumCPU())
	}
	ret, _, callErr := procSetInformationJobObject.Call(
		jobHandle,
		uintptr(infoClassCpuRateControl),
		uintptr(unsafe.Pointer(&cpuInfo)),
		unsafe.Sizeof(cpuInfo),
	)
	if ret == 0 {
		return fmt.Errorf("SetInformationJobObject (CPU rate): %w", callErr)
	}
	w.logger.Debug("set CPU rate", "rate_per_10000", cpuInfo.CpuRate, "cores", cores, "total_cores", goruntime.NumCPU())
	return nil
}

// setJobLimits applies a Job Object's extended limits. Each call replaces the
// job's whole set of basic limit flags, so every limit the job carries travels
// in the one call.
func setJobLimits(jobHandle uintptr, info *jobobjectExtendedLimitInfo) error {
	ret, _, callErr := procSetInformationJobObject.Call(
		jobHandle,
		uintptr(infoClassExtendedLimit),
		uintptr(unsafe.Pointer(info)),
		unsafe.Sizeof(*info),
	)
	if ret == 0 {
		return callErr
	}
	return nil
}

// Enforce creates a Job Object, configures memory and CPU limits, and assigns
// the process to the job. Returns a cleanup function that closes the handles.
func (w *WindowsLimiter) Enforce(pid int, limits *TaskLimits) (func(), error) {
	// Create anonymous Job Object.
	jobHandle, _, err := procCreateJobObjectW.Call(0, 0)
	if jobHandle == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// Run every process of the job at idle priority, so the owner's own
	// programs are served first whenever the CPUs are contended, and set the
	// memory limit alongside it. The priority is a courtesy: if the job refuses
	// it the task still runs, with its memory limit, at normal priority.
	info := jobobjectExtendedLimitInfo{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitPriorityClass
	info.BasicLimitInformation.PriorityClass = idlePriorityClass
	if limits.MaxMemoryMB > 0 {
		info.BasicLimitInformation.LimitFlags |= jobObjectLimitProcessMemory
		info.ProcessMemoryLimit = uintptr(limits.MaxMemoryMB) * 1024 * 1024
	}
	if err := setJobLimits(jobHandle, &info); err != nil {
		w.logger.Warn("could not lower the task's priority; it runs at normal priority", "pid", pid, "error", err)
		info.BasicLimitInformation.LimitFlags &^= jobObjectLimitPriorityClass
		info.BasicLimitInformation.PriorityClass = 0
		if limits.MaxMemoryMB > 0 {
			if err := setJobLimits(jobHandle, &info); err != nil {
				procCloseHandle.Call(jobHandle)
				return nil, fmt.Errorf("SetInformationJobObject (memory): %w", err)
			}
		}
	} else {
		w.logger.Debug("set job priority", "priority_class", "idle")
	}
	if limits.MaxMemoryMB > 0 {
		w.logger.Debug("set memory limit", "limit_mb", limits.MaxMemoryMB)
	}

	// Set the CPU rate to this task's grant — not, as before, the whole
	// budget for every job, which let N tasks use N times the limit
	// (TB-75).
	if limits.CPU.Cores > 0 {
		if err := w.setJobCPURate(jobHandle, limits.CPU.Cores); err != nil {
			procCloseHandle.Call(jobHandle)
			return nil, err
		}
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

	w.mu.Lock()
	w.jobs[pid] = jobHandle
	w.mu.Unlock()

	cleanup := func() {
		w.mu.Lock()
		delete(w.jobs, pid)
		w.mu.Unlock()
		procCloseHandle.Call(jobHandle)
	}
	return cleanup, nil
}

// CheckDiskSpace verifies that at least requiredMB of disk space is available
// on the volume containing path.
func (w *WindowsLimiter) CheckDiskSpace(path string, requiredMB int) error {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("%w: invalid path %s: %v", ErrDiskSpaceUnknown, path, err)
	}

	var freeBytes, totalBytes, totalFreeBytes uint64
	ret, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytes)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return fmt.Errorf("%w: GetDiskFreeSpaceEx %s: %v", ErrDiskSpaceUnknown, path, callErr)
	}

	availableMB := freeBytes / (1024 * 1024)
	if availableMB < uint64(requiredMB) {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required", availableMB, requiredMB)
	}

	return nil
}

// describeCPUEnforcement reports the Windows CPU posture.
//
// The Job Object applies CPU RATE control — a hard cap on the share of CPU time
// the job may consume — rather than placing work on chosen processors. Like the
// cgroup quota on Linux this is insensitive to which CPUs are permitted, so
// there is no permitted-set count to report.
func describeCPUEnforcement() CPUEnforcement {
	return CPUEnforcement{
		Mechanism:  "Job Object CPU rate control",
		Confinable: true,
	}
}
