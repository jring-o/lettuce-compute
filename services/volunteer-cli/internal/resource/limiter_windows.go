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

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
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

	// jobs keeps each enforced process's Job Object handle until its cleanup
	// runs, so the job's CPU rate can be rewritten while the process runs
	// (SetCPU, TB-75). Before this the handle was held only by the cleanup
	// closure and the rate was fixed for the process's life.
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

// cpuRateFor converts a task's CPU share into a Job Object CPU rate: the
// share as a percentage of the machine's CPUs, in hundredths of a percent
// (50 % = 5000, 100 % = 10000), clamped to the API's 1 %–100 % range. A
// share of 1.5 cores on an 8-CPU machine is 18.75 % → 1875.
func cpuRateFor(shareCores float64, numCPU int) uint32 {
	if numCPU < 1 {
		numCPU = 1
	}
	rate := uint32(shareCores * 10000 / float64(numCPU))
	if rate > 10000 {
		rate = 10000
	}
	if rate < 100 {
		rate = 100 // minimum 1%
	}
	return rate
}

// setJobCPURate applies a hard CPU-rate cap to a Job Object, or lifts it when
// shareCores is 0.
func (w *WindowsLimiter) setJobCPURate(jobHandle uintptr, shareCores float64) error {
	cpuInfo := jobobjectCpuRateControlInfo{}
	if shareCores > 0 {
		cpuInfo.ControlFlags = jobObjectCpuRateControlEnable | jobObjectCpuRateControlHardCap
		cpuInfo.CpuRate = cpuRateFor(shareCores, goruntime.NumCPU())
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
	w.logger.Debug("set CPU rate", "rate_per_10000", cpuInfo.CpuRate, "cores", runtime.FormatCores(shareCores), "total_cores", goruntime.NumCPU())
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

	// Set the CPU rate to this task's SHARE of the budget — not, as before,
	// the whole budget for every job, which let N tasks use N times the
	// limit (TB-75).
	if limits.CPU.ShareCores > 0 {
		if err := w.setJobCPURate(jobHandle, limits.CPU.ShareCores); err != nil {
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

// SetCPU rewrites the CPU rate of the Job Object a running task was assigned
// to (TB-75). A pid that Enforce never saw, or whose cleanup already ran, is
// an error the caller logs and moves past.
func (w *WindowsLimiter) SetCPU(pid int, cpu runtime.CPUGrant) error {
	w.mu.Lock()
	jobHandle, ok := w.jobs[pid]
	w.mu.Unlock()
	if !ok {
		return fmt.Errorf("no job object for pid %d", pid)
	}
	return w.setJobCPURate(jobHandle, cpu.ShareCores)
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
