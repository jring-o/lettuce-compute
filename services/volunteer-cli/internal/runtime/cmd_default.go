//go:build !windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func newCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// interruptCommand asks a command that has run past its timeout to stop:
// SIGTERM, which podman handles by resetting its machine's "starting" flag
// before it exits. A kill skips that clean-up.
func interruptCommand(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
