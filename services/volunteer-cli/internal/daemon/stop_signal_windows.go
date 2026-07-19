//go:build windows

package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Windows has no cross-process SIGTERM: another process cannot deliver a signal
// the way `kill` does on Unix, and TerminateProcess kills instantly — taking the
// running compute child with it via the slot job object, losing all in-flight
// work. To keep the CLI's "finish current work unit, then stop" promise, the
// daemon creates a session-local named kernel event at startup and treats a
// signal on it exactly like SIGTERM; `lettuce-volunteer stop` opens the event by
// PID and sets it.

// stopEventName returns the name of the per-daemon stop event. Keyed by PID so
// several daemons (distinct data dirs) can coexist in one session, and scoped
// `Local\` so it lives in the caller's session namespace.
func stopEventName(pid int) string {
	return fmt.Sprintf(`Local\lettuce-volunteer-stop-%d`, pid)
}

// ListenForStopRequests creates this process's named stop event and returns a
// channel that receives one value when a graceful stop is requested. The
// listener lives for the process lifetime; the event handle is released when
// the wait completes or the process exits.
func ListenForStopRequests() (<-chan struct{}, error) {
	name, err := windows.UTF16PtrFromString(stopEventName(os.Getpid()))
	if err != nil {
		return nil, fmt.Errorf("encoding stop event name: %w", err)
	}
	handle, err := windows.CreateEvent(nil, 1 /* manual reset */, 0 /* non-signaled */, name)
	if err != nil {
		return nil, fmt.Errorf("creating stop event: %w", err)
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer windows.CloseHandle(handle)
		event, waitErr := windows.WaitForSingleObject(handle, windows.INFINITE)
		if waitErr == nil && event == windows.WAIT_OBJECT_0 {
			ch <- struct{}{}
		}
	}()
	return ch, nil
}

// RequestGracefulStop asks the daemon with the given PID to finish its current
// work unit and exit — the Windows counterpart of SIGTERM. It fails, rather
// than falling back to a hard kill, when the daemon does not expose the stop
// event; `stop --force` is the explicit hard-kill path.
func RequestGracefulStop(pid int) error {
	name, err := windows.UTF16PtrFromString(stopEventName(pid))
	if err != nil {
		return fmt.Errorf("encoding stop event name: %w", err)
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("opening stop event for PID %d (daemon not accepting stop requests — is it running, and started by this user?): %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetEvent(handle); err != nil {
		return fmt.Errorf("signaling stop event for PID %d: %w", pid, err)
	}
	return nil
}

// KillProcess forcibly terminates the process without any grace: all in-flight
// compute is lost. Used by `stop --force` for an unresponsive daemon.
func KillProcess(pid int) error {
	return StopProcess(pid)
}
