package cli

import (
	"fmt"
	"io"
	"time"
)

// headNoWorkAPI mirrors GET /api/v1/heads' per-head no_work: the reason a head
// gave on its latest empty work reply that named one — this machine at the
// head's in-flight cap, the account's results or standing, or the speed on
// record against a deadline — until the head sends work again.
type headNoWorkAPI struct {
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Leaf    string    `json:"leaf,omitempty"`
	At      time.Time `json:"at"`
}

// headNoWorkNeedsAttention reports the reasons doctor lists as warnings: the
// account is benched, or the head judges this account too slow for a leaf's
// deadlines. The others (the cap, results already in, a recent failed copy)
// resolve themselves.
func headNoWorkNeedsAttention(reason string) bool {
	return reason == "account_benched" || reason == "infeasible_deadline"
}

// printHeadNoWork is status's section for heads that said why they sent no
// work. Nothing is printed when none did.
func printHeadNoWork(w io.Writer, heads []leafsAPIHead) {
	var lines []string
	for _, h := range heads {
		if h.NoWork == nil || h.NoWork.Message == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("  - %s (%s)", h.NoWork.Message, h.NoWork.At.Local().Format("15:04")))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "Why a head is sending no work:")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// checkHeadNoWork is doctor's line per head that said why it sent no work.
func checkHeadNoWork(rep *doctorReport, heads []leafsAPIHead) {
	for _, h := range heads {
		if h.NoWork == nil || h.NoWork.Message == "" {
			continue
		}
		name := h.Name
		if name == "" {
			name = h.GRPCAddress
		}
		level := docInfo
		if headNoWorkNeedsAttention(h.NoWork.Reason) {
			level = docWarn
		}
		rep.add(level, name, fmt.Sprintf("sent no work at %s: %s", h.NoWork.At.Local().Format("15:04"), h.NoWork.Message), "")
	}
}
