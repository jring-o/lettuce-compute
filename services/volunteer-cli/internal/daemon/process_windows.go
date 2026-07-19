//go:build windows

package daemon

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000
	processTerminate               = 0x0001
	stillActive                    = 259
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess      = kernel32.NewProc("OpenProcess")
	procGetExitCodeProc  = kernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess = kernel32.NewProc("TerminateProcess")
)

// IsProcessRunning checks if a process with the given PID is alive.
func IsProcessRunning(pid int) bool {
	handle, _, _ := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation),
		0, // bInheritHandle = false
		uintptr(pid),
	)
	if handle == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	var exitCode uint32
	ret, _, _ := procGetExitCodeProc.Call(handle, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 {
		return false
	}
	return exitCode == stillActive
}

// StopProcess forcibly terminates the process on Windows (TerminateProcess:
// instant, no grace). It is the hard-terminate primitive for compute children
// (killDroppedOrphan) and `stop --force`; the CLI's graceful stop path is
// RequestGracefulStop, which signals the daemon's named stop event instead.
func StopProcess(pid int) error {
	handle, _, err := procOpenProcess.Call(
		uintptr(processTerminate),
		0,
		uintptr(pid),
	)
	if handle == 0 {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	ret, _, err := procTerminateProcess.Call(handle, 1)
	if ret == 0 {
		return fmt.Errorf("TerminateProcess(%d): %w", pid, err)
	}
	return nil
}
