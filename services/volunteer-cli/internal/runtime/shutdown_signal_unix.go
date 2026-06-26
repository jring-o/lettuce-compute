//go:build !windows

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setGracefulShutdown configures cmd so that when its context is cancelled (a
// graceful stop or a deadline) the process is first asked to terminate with
// SIGTERM — giving a cooperating leaf a window to flush a final checkpoint — and
// is only force-killed if it does not exit within grace. A leaf that does not
// trap SIGTERM is terminated by it immediately, exactly as before.
func setGracefulShutdown(cmd *exec.Cmd, grace time.Duration) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	// After Cancel runs, os/exec waits WaitDelay for the process to exit before
	// sending SIGKILL (and also bounds how long Wait blocks on output streams).
	cmd.WaitDelay = grace
}
