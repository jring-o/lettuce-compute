package cli

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/spf13/cobra"
)

func newCreditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "credit",
		Short: "Show your account's credit across all attached heads and leaves",
		Long: "Show how much work your ACCOUNT has done, as reported by the head(s) you\n" +
			"are attached to. Credit is keyed to your identity key, so this total spans\n" +
			"every machine you run under the same account — not just this host. When no\n" +
			"head can be reached it falls back to a local estimate from this host's\n" +
			"history.\n\n" +
			"It also lists, per head and leaf, your results by state (waiting for\n" +
			"validation, agreed, did not agree, ...), runs stopped because enough results\n" +
			"arrived, and the copies you hold in progress, so work that has not earned\n" +
			"credit yet is visible too.\n\n" +
			"The daemon must be running (`lettuce-volunteer start`).",
		RunE: runCredit,
	}
}

// creditCommandResponse mirrors GET /api/v1/credit for the `credit` command.
type creditCommandResponse struct {
	TotalCredit float64 `json:"total_credit"`
	Today       float64 `json:"today"`
	ThisWeek    float64 `json:"this_week"`
	ThisMonth   float64 `json:"this_month"`
	Source      string  `json:"source"`
	DayBoundary string  `json:"day_boundary"`
	ByHead      []struct {
		HeadName    string                     `json:"head_name"`
		VolunteerID string                     `json:"volunteer_id"`
		TotalCredit float64                    `json:"total_credit"`
		Available   bool                       `json:"available"`
		WorkStatus  *management.HeadWorkStatus `json:"work_status"`
	} `json:"by_head"`
	ByLeaf []struct {
		LeafID   string  `json:"leaf_id"`
		LeafName string  `json:"leaf_name"`
		Credit   float64 `json:"credit"`
	} `json:"by_leaf"`
}

func runCredit(cmd *cobra.Command, args []string) error {
	var cr creditCommandResponse
	if err := managementGet(cfg.DataDir, "/api/v1/credit", &cr); err != nil {
		return fmt.Errorf("could not read credit (is the daemon running? run `lettuce-volunteer start`): %w", err)
	}

	fmt.Printf("Total credit: %s\n", formatCredit(cr.TotalCredit))
	fmt.Printf("  Today: %s    This week: %s    This month: %s\n",
		formatCredit(cr.Today), formatCredit(cr.ThisWeek), formatCredit(cr.ThisMonth))
	// The head records credit by UTC date, so head-derived day buckets cannot
	// follow this machine's clock; say so rather than let "today" be misread
	// against a local-day history list (TB-57).
	if cr.DayBoundary == "utc" {
		fmt.Println("  Days are counted in UTC, the head's clock; `history` shows your local day.")
	}
	if cr.Source == "local" {
		fmt.Println("  Note: local estimate from this host's history — no head was reachable.")
		fmt.Println("        Connect to a head for your authoritative, account-wide total.")
	}

	if len(cr.ByHead) > 0 {
		fmt.Println("\nBy head:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  HEAD\tCREDIT\tSTATUS")
		for _, h := range cr.ByHead {
			status := "ok"
			if !h.Available {
				status = "unavailable"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", labelOrDash(h.HeadName), formatCredit(h.TotalCredit), status)
		}
		_ = w.Flush()
	}

	if len(cr.ByLeaf) > 0 {
		fmt.Println("\nBy leaf:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  LEAF\tCREDIT")
		for _, l := range cr.ByLeaf {
			name := l.LeafName
			if name == "" {
				name = l.LeafID
			}
			fmt.Fprintf(w, "  %s\t%s\n", labelOrDash(name), formatCredit(l.Credit))
		}
		_ = w.Flush()
	}

	if cr.Source == "head" {
		printWorkStatus(cr)
	}

	return nil
}

// printWorkStatus lists, per head and leaf, the account's results by validation
// state and its copies in progress: the work that credit alone does not show.
// Only non-zero lines print. A head that answered without the figures (an older
// head) says so rather than printing zeros; an unreachable head is already marked
// in the table above.
func printWorkStatus(cr creditCommandResponse) {
	anyAnswered := false
	for _, h := range cr.ByHead {
		anyAnswered = anyAnswered || h.Available
	}
	if !anyAnswered {
		return
	}

	fmt.Println("\nYour results by state (all your machines):")
	anyPending := false
	for _, h := range cr.ByHead {
		if !h.Available {
			continue
		}
		fmt.Printf("  %s\n", labelOrDash(h.HeadName))
		if h.WorkStatus == nil {
			fmt.Println("    not reported by this head (it runs an older version)")
			continue
		}
		if len(h.WorkStatus.ByLeaf) == 0 {
			fmt.Println("    no results or copies on this head yet")
			continue
		}
		for _, l := range h.WorkStatus.ByLeaf {
			name := l.LeafName
			if name == "" {
				name = l.LeafID
			}
			fmt.Printf("    %s\n", labelOrDash(name))
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, row := range []struct {
				label string
				n     int
			}{
				{"waiting for validation", l.ResultsPending},
				{"agreed (credited)", l.ResultsAgreed},
				{"did not agree", l.ResultsDisagreed},
				{"checking the uploaded output", l.ResultsAwaitingContentVerification},
				{"the uploaded output could not be checked", l.ResultsContentVerificationFailed},
				{"not compared (the work unit was retired)", l.ResultsSuperseded},
				{"stopped: enough results arrived while it ran", l.RunsStopped},
			} {
				if row.n > 0 {
					fmt.Fprintf(w, "      %s\t%d\n", row.label, row.n)
				}
			}
			if l.CopiesRunning > 0 || l.CopiesWaitingToStart > 0 {
				fmt.Fprintf(w, "      in progress\t%d running, %d waiting to start\n", l.CopiesRunning, l.CopiesWaitingToStart)
			}
			_ = w.Flush()
			anyPending = anyPending || l.ResultsPending > 0
		}
	}
	if anyPending {
		fmt.Println("\n  A result earns credit only once it is validated. On a leaf that needs")
		fmt.Println("  agreeing results, that takes a matching result from a different account;")
		fmt.Println("  your own other machines are never sent the same work unit.")
	}
}

// formatCredit renders a credit amount without trailing noise: whole numbers print
// as integers (e.g. "375"), fractional amounts with up to two decimals ("1.5").
func formatCredit(c float64) string {
	if c == math.Trunc(c) {
		return strconv.FormatFloat(c, 'f', 0, 64)
	}
	return strconv.FormatFloat(c, 'f', 2, 64)
}
