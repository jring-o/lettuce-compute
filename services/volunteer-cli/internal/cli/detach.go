package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newDetachCmd() *cobra.Command {
	var server string

	cmd := &cobra.Command{
		Use:   "detach [leaf-id]",
		Short: "Remove a leaf or server from preferences",
		Long: `Detach a specific leaf by ID or remove all entries for a server.

Examples:
  lettuce-volunteer detach <leaf-id>
  lettuce-volunteer detach --server <host>`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
				Level: parseSlogLevel(cfg.LogLevel),
			}))
			mgr := project.NewManager(cfg, cfgPath, logger)

			if server != "" {
				if err := mgr.DetachServer(server); err != nil {
					return err
				}
				fmt.Printf("Detached from server %s.\n", server)
				return nil
			}

			if len(args) == 1 {
				if err := mgr.DetachLeaf(args[0]); err != nil {
					return err
				}
				fmt.Printf("Detached from leaf %s.\n", args[0])
				return nil
			}

			return fmt.Errorf("specify a leaf ID or use --server <host>")
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "server hostname or IP to detach")

	return cmd
}
