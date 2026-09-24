//go:build windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func newCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}

// interruptCommand stops a command that has run past its timeout. A process
// started without a console cannot be sent a console interrupt, so on
// Windows it is killed.
func interruptCommand(p *os.Process) error {
	return p.Kill()
}
