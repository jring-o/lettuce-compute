package cli

import (
	"fmt"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop gracefully (finish current work unit, then stop)",
		RunE:  runStop,
	}
}

func runStop(cmd *cobra.Command, args []string) error {
	pid, err := daemon.ReadPID(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("no running daemon found: %w", err)
	}

	if !daemon.IsProcessRunning(pid) {
		// Stale PID file — clean it up.
		daemon.RemovePID(cfg.DataDir)
		return fmt.Errorf("no running daemon found (stale PID file removed)")
	}

	if err := daemon.StopProcess(pid); err != nil {
		return fmt.Errorf("failed to stop daemon (PID: %d): %w", pid, err)
	}

	fmt.Printf("Sent stop signal to volunteer daemon (PID: %d). It will finish the current work unit before exiting.\n", pid)
	return nil
}
