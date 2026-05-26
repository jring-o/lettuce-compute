//go:build !windows

package runtime

import (
	"context"
	"os/exec"
)

func defaultCommandExecutor(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCommandTimeout)
	defer cancel()
	return defaultCommandExecutorCtx(ctx, name, args...)
}

func defaultCommandExecutorCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
