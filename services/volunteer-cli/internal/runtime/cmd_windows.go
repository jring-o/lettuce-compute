//go:build windows

package runtime

import (
	"context"
	"os/exec"
	"syscall"
)

func defaultCommandExecutor(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCommandTimeout)
	defer cancel()
	return defaultCommandExecutorCtx(ctx, name, args...)
}

func defaultCommandExecutorCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd.Output()
}
