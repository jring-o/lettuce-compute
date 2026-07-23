package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current state: daemon, servers, running tasks with progress, and credit earned",
		RunE:  runStatus,
	}
}

// statusAPIResponse mirrors the fields of the management API's GET /api/v1/status
// that this command renders.
type statusAPIResponse struct {
	UptimeSeconds int                `json:"uptime_seconds"`
	ActiveTasks   []statusActiveTask `json:"active_tasks"`
	QueuedTasks   []statusQueuedTask `json:"queued_tasks"`
	PausedReason  *string            `json:"paused_reason"`
}

type statusActiveTask struct {
	LeafName              string  `json:"leaf_name"`
	HeadName              string  `json:"head_name"`
	RuntimeType           string  `json:"runtime_type"`
	ProgressPct           int     `json:"progress_pct"`
	ElapsedSeconds        int     `json:"elapsed_seconds"`
	EstimatedRemainingSec *int    `json:"estimated_remaining_seconds"`
	TaskStatus            string  `json:"task_status"`
	StatusReason          *string `json:"status_reason"`
}

type statusQueuedTask struct {
	LeafName   string `json:"leaf_name"`
	ServerName string `json:"server_name"`
}

// creditAPIResponse mirrors the fields of GET /api/v1/credit that this command renders.
type creditAPIResponse struct {
	TotalCredit float64 `json:"total_credit"`
	Today       float64 `json:"today"`
	ThisWeek    float64 `json:"this_week"`
	Source      string  `json:"source"`
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
			if len(s.PinnedLeafIDs) > 0 {
				fmt.Printf("  - %s (all listed leafs + pinned: %s)\n", name, strings.Join(s.PinnedLeafIDs, ", "))
			} else {
				fmt.Printf("  - %s (all leafs)\n", name)
			}
		}
	} else {
		fmt.Println("Servers: (none configured)")
	}

	// When the daemon is running, show live tasks + progress + credit from the
	// local management API. These are best-effort: on any error we print a short
	// note and keep the rest of the output, so `status` never fails wholesale.
	if st.DaemonRunning {
		printActiveTasks(cfg.DataDir)
		printCredit(cfg.DataDir)
	}

	return nil
}

// managementGet performs an authenticated GET against the running daemon's local
// management API and decodes the JSON body into out. The request targets
// 127.0.0.1:<port> with the bearer token from daemon.json, satisfying both the
// management API's auth and its Host-header allowlist.
func managementGet(dataDir, path string, out any) error {
	info, err := management.ReadDaemonInfo(dataDir)
	if err != nil {
		return fmt.Errorf("daemon not running (no daemon.json)")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, path)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("management API returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// printActiveTasks renders the daemon's in-progress and buffered work units,
// including live progress percentages. Progress is read by the daemon from each
// task's progress file and works for both native and container runtimes, so a
// long-running container task now reports its progress here instead of only on
// disk.
func printActiveTasks(dataDir string) {
	var sr statusAPIResponse
	if err := managementGet(dataDir, "/api/v1/status", &sr); err != nil {
		fmt.Fprintf(os.Stderr, "Tasks: unavailable (%v)\n", err)
		return
	}

	if sr.PausedReason != nil && *sr.PausedReason != "" {
		fmt.Printf("Paused: %s\n", *sr.PausedReason)
	}

	if len(sr.ActiveTasks) == 0 {
		fmt.Println("Active tasks: none")
	} else {
		fmt.Printf("Active tasks (%d):\n", len(sr.ActiveTasks))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  LEAF\tRUNTIME\tPROGRESS\tELAPSED\tETA\tSTATUS")
		for _, t := range sr.ActiveTasks {
			eta := "—"
			if t.EstimatedRemainingSec != nil {
				eta = formatDurationSeconds(*t.EstimatedRemainingSec)
			}
			status := t.TaskStatus
			if t.StatusReason != nil && *t.StatusReason != "" {
				status = fmt.Sprintf("%s (%s)", t.TaskStatus, *t.StatusReason)
			}
			fmt.Fprintf(w, "  %s\t%s\t%d%%\t%s\t%s\t%s\n",
				labelOrDash(t.LeafName),
				labelOrDash(strings.ToLower(t.RuntimeType)),
				t.ProgressPct,
				formatDurationSeconds(t.ElapsedSeconds),
				eta,
				status,
			)
		}
		_ = w.Flush()
	}

	if len(sr.QueuedTasks) > 0 {
		fmt.Printf("Buffered (fetched, not started): %d\n", len(sr.QueuedTasks))
	}
}

// printCredit renders the volunteer's earned credit. Credit is secondary, so any
// error is silently ignored (the rest of `status` is still useful without it).
func printCredit(dataDir string) {
	var cr creditAPIResponse
	if err := managementGet(dataDir, "/api/v1/credit", &cr); err != nil {
		return
	}
	line := fmt.Sprintf("Credit: %s total (%s today, %s this week)",
		formatCredit(cr.TotalCredit), formatCredit(cr.Today), formatCredit(cr.ThisWeek))
	if cr.Source == "local" {
		line += " [local estimate — head unreachable]"
	}
	line += "  (run `lettuce-volunteer credit` for the full breakdown)"
	fmt.Println(line)
}

// formatDurationSeconds renders a whole-second count as a compact human string
// (e.g. 45s, 12m30s, 3h05m). Negative inputs are clamped to 0.
func formatDurationSeconds(secs int) string {
	if secs < 0 {
		secs = 0
	}
	d := time.Duration(secs) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := secs % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// labelOrDash returns s, or an em dash if s is empty, so table cells never blank out.
func labelOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
