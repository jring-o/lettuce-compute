//go:build windows

package daemon

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// nativeProcessHandle suspends/resumes a native process via NtSuspendProcess/NtResumeProcess.
type nativeProcessHandle struct {
	pid int
}

var (
	ntdll              = windows.NewLazySystemDLL("ntdll.dll")
	ntSuspendProcess   = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess    = ntdll.NewProc("NtResumeProcess")
)

func NewNativeProcessHandle(pid int) ProcessHandle {
	return &nativeProcessHandle{pid: pid}
}

func (h *nativeProcessHandle) Suspend() error {
	return ntProcessCall(ntSuspendProcess, h.pid)
}

func (h *nativeProcessHandle) Resume() error {
	return ntProcessCall(ntResumeProcess, h.pid)
}

func (h *nativeProcessHandle) PID() int {
	return h.pid
}

// isProcessAlive checks whether a process with the given PID exists.
func isProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(handle)
	return true
}

func ntProcessCall(proc *windows.LazyProc, pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// windows.Handle is itself a uintptr alias, so pass it directly to LazyProc.Call.
	// Wrapping via unsafe.Pointer (uintptr -> unsafe.Pointer) violates the unsafe.Pointer
	// conversion rules and trips `go vet` (possible misuse of unsafe.Pointer).
	ret, _, _ := proc.Call(uintptr(handle))
	if ret != 0 {
		return fmt.Errorf("%s failed for PID %d: NTSTATUS 0x%x", proc.Name, pid, ret)
	}
	return nil
}
