package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show completed work units and credit earned",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistory(cmd, limit)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of entries to show")

	return cmd
}

func runHistory(cmd *cobra.Command, limit int) error {
	logger, closeLogger := newLogger(cfg)
	defer closeLogger()
	mgr := project.NewManager(cfg, cfgPath, logger)

	entries, err := mgr.GetHistory(cmd.Context(), limit)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Println("No completed work units yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "WORK UNIT\tLEAF\tSERVER\tCOMPLETED\tDURATION\tACCEPTED\n")
	for _, e := range entries {
		accepted := "yes"
		if !e.ResultAccepted {
			accepted = "no"
		}
		server := e.ServerName
		if server == "" {
			server = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%ds\t%s\n",
			truncate(e.WorkUnitID, 12),
			truncate(e.LeafID, 12),
			truncate(server, 20),
			e.CompletedAt.Format("2006-01-02 15:04"),
			e.WallClockSeconds,
			accepted,
		)
	}
	w.Flush()
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
