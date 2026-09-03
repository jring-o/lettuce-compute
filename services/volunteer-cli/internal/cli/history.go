package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show the work units this machine has completed, with totals",
		Long: "List every work unit this machine has finished and submitted, newest first,\n" +
			"with a count of how many there are in all. This is the local, per-machine record:\n" +
			"it answers \"how many units have I processed?\" whether or not the head has\n" +
			"validated them yet. Credit is decided later by the head; see `lettuce-volunteer\n" +
			"credit` for that.\n\n" +
			"Works with the daemon stopped. When the daemon is running, leaves recorded only by\n" +
			"id (entries older than this build) are looked up by name through it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistory(cmd, limit)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of entries to show (0 = all)")

	return cmd
}

func runHistory(cmd *cobra.Command, limit int) error {
	logger, closeLogger := newLogger(cfg)
	defer closeLogger()
	mgr := project.NewManager(cfg, cfgPath, logger)

	entries, err := mgr.GetHistory(cmd.Context())
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Println("No completed work units yet.")
		return nil
	}

	printHistory(os.Stdout, entries, limit, lookupHistoryLeafNames(entries))
	return nil
}

// printHistory renders the newest `limit` entries of all (0 = every entry) and a
// footer with the counts the volunteer actually asked for. `names` maps leaf ids to
// display names for entries that did not record one (nil is fine).
//
// The footer exists because this command is the one self-serve answer to "how many
// units have I processed?" and it used to print twenty rows with no total, so
// counting them gave a confidently wrong answer (TB-46). The HEAD ACCEPTED column
// is the head's verdict on the submission itself, not validation or credit, and the
// footer says so — that distinction is the question the command gets asked about.
func printHistory(w io.Writer, all []daemon.HistoryEntry, limit int, names map[string]string) {
	shown := all
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}

	accepted := 0
	for _, e := range all {
		if e.ResultAccepted {
			accepted++
		}
	}

	unnamed := 0
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "WORK UNIT\tLEAF\tSERVER\tCOMPLETED\tDURATION\tHEAD ACCEPTED\n")
	for _, e := range shown {
		acceptedCell := "yes"
		if !e.ResultAccepted {
			acceptedCell = "no"
		}
		server := e.ServerName
		if server == "" {
			server = "-"
		}
		leaf := e.LeafName
		if leaf == "" {
			leaf = names[e.LeafID]
		}
		if leaf == "" {
			leaf = truncate(e.LeafID, 12)
			unnamed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%ds\t%s\n",
			truncate(e.WorkUnitID, 12),
			leaf,
			truncate(server, 20),
			e.CompletedAt.Local().Format("2006-01-02 15:04"),
			e.WallClockSeconds,
			acceptedCell,
		)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nShowing %d of %d completed %s; the head accepted %d on submission.\n",
		len(shown), len(all), plural(len(all), "unit", "units"), accepted)
	fmt.Fprintln(w, "Head acceptance is not credit: validation happens later on the head (see `lettuce-volunteer credit`).")
	if len(shown) < len(all) {
		fmt.Fprintln(w, "Use --limit N to show more (0 = all).")
	}
	if unnamed > 0 {
		fmt.Fprintf(w, "%d %s shown by leaf id: the name was not recorded and no running daemon could look it up.\n",
			unnamed, plural(unnamed, "entry is", "entries are"))
	}
}

// historyHeadsResponse is the slice of GET /api/v1/heads that history needs: each
// head's leaves, by id and name.
type historyHeadsResponse struct {
	Heads []struct {
		Leafs []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"leafs"`
	} `json:"heads"`
}

// lookupHistoryLeafNames asks the running daemon for leaf names, but only when some
// entry needs one: entries written by this build carry their leaf's name already,
// older ones carry only the head's UUID. Best effort — with no daemon running the
// command still works from the file alone, and those rows show the id.
func lookupHistoryLeafNames(entries []daemon.HistoryEntry) map[string]string {
	needed := false
	for _, e := range entries {
		if e.LeafName == "" {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}

	var resp historyHeadsResponse
	if err := managementGet(cfg.DataDir, "/api/v1/heads", &resp); err != nil {
		return nil
	}
	names := make(map[string]string)
	for _, h := range resp.Heads {
		for _, l := range h.Leafs {
			if l.ID != "" && l.Name != "" {
				names[l.ID] = l.Name
			}
		}
	}
	return names
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
