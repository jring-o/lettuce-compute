//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// ListenForStopRequests is a no-op on Unix: `lettuce-volunteer stop` delivers
// SIGTERM, which the daemon's signal handler already treats as a graceful stop.
// The returned nil channel blocks forever in a select.
func ListenForStopRequests() (<-chan struct{}, error) {
	return nil, nil
}

// RequestGracefulStop asks the daemon with the given PID to finish its current
// work unit and exit. On Unix that is SIGTERM.
func RequestGracefulStop(pid int) error {
	return StopProcess(pid)
}

// KillProcess forcibly terminates the process without any grace (SIGKILL): all
// in-flight compute is lost. Used by `stop --force` for an unresponsive daemon.
func KillProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGKILL)
}
