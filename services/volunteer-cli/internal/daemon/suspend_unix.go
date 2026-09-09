//go:build !windows

package daemon

import "syscall"

// nativeProcessHandle suspends/resumes a native process via SIGSTOP/SIGCONT.
type nativeProcessHandle struct {
	pid int
	// setCPU gives the process a new CPU share through the limiter that
	// enforced it (TB-75); nil when no limiter is wired (a handle built for
	// a process this daemon did not enforce, e.g. a resumed orphan).
	setCPU func(pid int, shareCores float64) error
}

// NewNativeProcessHandle returns a handle for pid. setCPU, when non-nil, is
// how the handle rewrites the process's CPU cap (see ProcessHandle.SetCPUShare).
func NewNativeProcessHandle(pid int, setCPU func(pid int, shareCores float64) error) ProcessHandle {
	return &nativeProcessHandle{pid: pid, setCPU: setCPU}
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

func (h *nativeProcessHandle) SetCPUShare(shareCores float64) error {
	if h.setCPU == nil {
		return nil
	}
	return h.setCPU(h.pid, shareCores)
}

// isProcessAlive checks whether a process with the given PID exists.
func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
