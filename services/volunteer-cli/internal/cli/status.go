package cli

import (
	"fmt"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current state: running tasks, resource usage, credit earned",
		RunE:  runStatus,
	}
}

func runStatus(cmd *cobra.Command, args []string) error {
	logger, closeLogger := newLogger(cfg)
	defer closeLogger()
	mgr := project.NewManager(cfg, cfgPath, logger)

	st, err := mgr.GetStatus(cmd.Context())
	if err != nil {
		return err
	}

	if st.DaemonRunning {
		fmt.Printf("Daemon: running (PID: %d)\n", st.DaemonPID)
	} else {
		fmt.Println("Daemon: not running")
	}

	// Read daemon state for per-server connection info.
	dstate, _ := daemon.ReadDaemonState(cfg.DataDir)

	if dstate != nil && len(dstate.Servers) > 0 && st.DaemonRunning {
		// Show live server state from the running daemon.
		fmt.Println("Servers:")
		for _, s := range dstate.Servers {
			if s.Connected {
				fmt.Printf("  - %s  Volunteer ID: %s  Status: connected\n", s.GRPCAddress, s.VolunteerID)
			} else {
				fmt.Printf("  - %s  Status: unreachable\n", s.GRPCAddress)
			}
		}
	} else if len(st.Servers) > 0 {
		// Fall back to config-based server list.
		fmt.Println("Servers:")
		for _, s := range st.Servers {
			name := s.Name
			if name == "" {
				name = s.GRPCAddress
			}
			if s.LeafID != "" {
				fmt.Printf("  - %s (leaf: %s)\n", name, s.LeafID)
			} else {
				fmt.Printf("  - %s (all leafs)\n", name)
			}
		}
	} else {
		fmt.Println("Servers: (none configured)")
	}

	return nil
}
