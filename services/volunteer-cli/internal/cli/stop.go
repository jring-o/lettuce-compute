package cli

import (
	"fmt"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop gracefully (finish current work unit, then stop)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(cmd, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"kill the daemon immediately without finishing the current work unit (all in-flight compute is lost)")
	return cmd
}

func runStop(cmd *cobra.Command, force bool) error {
	pid, err := daemon.ReadPID(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("no running daemon found: %w", err)
	}

	if !daemon.IsProcessRunning(pid) {
		// Stale PID file — clean it up.
		daemon.RemovePID(cfg.DataDir)
		return fmt.Errorf("no running daemon found (stale PID file removed)")
	}

	if force {
		if err := daemon.KillProcess(pid); err != nil {
			return fmt.Errorf("failed to kill daemon (PID: %d): %w", pid, err)
		}
		fmt.Printf("Killed volunteer daemon (PID: %d). Any in-flight work unit was lost and will be re-dispatched by the head.\n", pid)
		return nil
	}

	if err := daemon.RequestGracefulStop(pid); err != nil {
		return fmt.Errorf("failed to stop daemon (PID: %d): %w (use 'lettuce-volunteer stop --force' to kill it immediately, losing in-flight work)", pid, err)
	}

	fmt.Printf("Sent stop signal to volunteer daemon (PID: %d). It will finish the current work unit before exiting.\n", pid)
	return nil
}
