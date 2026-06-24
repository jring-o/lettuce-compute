package cli

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"text/tabwriter"

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
	ByHead      []struct {
		HeadName    string  `json:"head_name"`
		VolunteerID string  `json:"volunteer_id"`
		TotalCredit float64 `json:"total_credit"`
		Available   bool    `json:"available"`
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

	return nil
}

// formatCredit renders a credit amount without trailing noise: whole numbers print
// as integers (e.g. "375"), fractional amounts with up to two decimals ("1.5").
func formatCredit(c float64) string {
	if c == math.Trunc(c) {
		return strconv.FormatFloat(c, 'f', 0, 64)
	}
	return strconv.FormatFloat(c, 'f', 2, 64)
}
