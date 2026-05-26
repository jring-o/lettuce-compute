//go:build !windows

package daemon

import "syscall"

// nativeProcessHandle suspends/resumes a native process via SIGSTOP/SIGCONT.
type nativeProcessHandle struct {
	pid int
}

func NewNativeProcessHandle(pid int) ProcessHandle {
	return &nativeProcessHandle{pid: pid}
}

func (h *nativeProcessHandle) Suspend() error {
	return syscall.Kill(h.pid, syscall.SIGSTOP)
}

func (h *nativeProcessHandle) Resume() error {
	return syscall.Kill(h.pid, syscall.SIGCONT)
}

func (h *nativeProcessHandle) PID() int {
	return h.pid
}

// isProcessAlive checks whether a process with the given PID exists.
func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
