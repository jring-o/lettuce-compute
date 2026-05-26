//go:build linux

package daemon

import (
	"os/exec"
	"syscall"
)

// setPdeathsig sets Pdeathsig on the command so the child is killed
// if the parent (daemon) dies unexpectedly. Linux-only.
func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
