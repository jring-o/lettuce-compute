package cli

import (
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newProjectsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "projects",
		Short:  "List available leafs matching preferences (deprecated: use 'leafs list')",
		RunE:   runProjects,
		Hidden: true,
	}
}

func runProjects(cmd *cobra.Command, args []string) error {
	fmt.Fprintln(os.Stderr, "Note: 'projects' is deprecated. Use 'leafs list' instead.")
	if len(cfg.Servers) == 0 {
		fmt.Println("No servers configured. Run `lettuce-volunteer attach --server <host>` first.")
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseSlogLevel(cfg.LogLevel),
	}))
	mgr := project.NewManager(cfg, cfgPath, logger)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tNAME\tRESEARCH AREA\tSTATE\tTASK PATTERN\n")

	found := false
	for _, srv := range cfg.Servers {
		if srv.HTTPAddress == "" {
			continue
		}
		leafs, err := mgr.ListLeafs(cmd.Context(), srv.HTTPAddress)
		if err != nil {
			logger.Warn("failed to list leafs from server", "server", srv.HTTPAddress, "error", err)
			continue
		}
		for _, p := range leafs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.ResearchArea, p.State, p.TaskPattern)
			found = true
		}
	}
	w.Flush()

	if !found {
		fmt.Println("No leafs found.")
	}
	return nil
}
