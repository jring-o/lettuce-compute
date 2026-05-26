//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// IsProcessRunning checks if a process with the given PID is alive.
func IsProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if the process exists without sending a signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// StopProcess sends SIGTERM to the process.
func StopProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
