//go:build windows

package runtime

import (
	"os/exec"
	"time"
)

// setGracefulShutdown configures cmd's post-cancellation behavior on Windows.
// Windows has no SIGTERM a child process can portably trap, so cancellation keeps
// the default behavior (the process is killed). WaitDelay still bounds how long
// Wait blocks on the process's output streams after cancellation.
func setGracefulShutdown(cmd *exec.Cmd, grace time.Duration) {
	cmd.WaitDelay = grace
}
