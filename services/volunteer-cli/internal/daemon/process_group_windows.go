//go:build windows

package daemon

import (
	"fmt"
	"log/slog"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

// JOBOBJECT_BASIC_LIMIT_INFORMATION matches the Windows API struct layout.
type jobObjectBasicLimitInformation struct {
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

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32Job          = windows.NewLazySystemDLL("kernel32.dll")
	procTerminateJobObj  = kernel32Job.NewProc("TerminateJobObject")
)

// jobObjectGroup implements ProcessGroup using a Windows Job Object.
// When the daemon exits (even abnormally), the Job Object handle is closed
// by the OS, which kills all processes in the job.
type jobObjectGroup struct {
	handle windows.Handle
	logger *slog.Logger
}

// NewProcessGroup creates a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
func NewProcessGroup(logger *slog.Logger) (ProcessGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := jobObjectExtendedLimitInfo{
		BasicLimitInformation: jobObjectBasicLimitInformation{
			LimitFlags: jobObjectLimitKillOnJobClose,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	logger.Info("process group created (Windows Job Object with KILL_ON_JOB_CLOSE)")
	return &jobObjectGroup{handle: job, logger: logger}, nil
}

func (g *jobObjectGroup) ConfigureCommand(cmd *exec.Cmd) {
	// On Windows, child processes are added to the Job Object after Start(),
	// not before. No SysProcAttr changes needed.
}

func (g *jobObjectGroup) Add(pid int) error {
	// Open the process with rights to assign it to a job and terminate it.
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(g.handle, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	g.logger.Debug("added process to job object", "pid", pid)
	return nil
}

func (g *jobObjectGroup) Terminate() {
	ret, _, err := procTerminateJobObj.Call(uintptr(g.handle), 1)
	if ret == 0 {
		g.logger.Warn("TerminateJobObject failed", "error", err)
	} else {
		g.logger.Info("terminated all processes in job object")
	}
}

func (g *jobObjectGroup) ReleaseChildren() {
	// Clear KILL_ON_JOB_CLOSE so frozen child processes survive daemon exit.
	info := jobObjectExtendedLimitInfo{
		BasicLimitInformation: jobObjectBasicLimitInformation{
			LimitFlags: 0,
		},
	}
	_, err := windows.SetInformationJobObject(
		g.handle,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		g.logger.Warn("failed to release children from job object", "error", err)
	} else {
		g.logger.Info("released children from job object (KILL_ON_JOB_CLOSE removed)")
	}
}

func (g *jobObjectGroup) Close() {
	windows.CloseHandle(g.handle)
}
