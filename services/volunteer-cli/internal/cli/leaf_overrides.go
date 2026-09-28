package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/spf13/cobra"
)

// The per-leaf CPU overrides: `leafs cores <slug> <n>` fixes the cores each of
// the leaf's tasks is given (within the range the leaf declares), and `leafs
// max-running <slug> <n>` caps how many of its tasks run at once. With the
// daemon running they are applied through its management API, so they take
// effect at the next task start without a restart; otherwise they are saved
// to config.yaml for the next start.

func newLeafsCoresCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cores <slug> <cores|default>",
		Short: "Set the cores each of a leaf's tasks is given on this machine",
		Long: `Set the number of cores each of a leaf's tasks is given on this machine.

A leaf declares the range of cores its tasks can use (for example 2–4), and
without a setting each task is given as many of those as are free when it
starts. This fixes the figure instead. It is kept within the leaf's range and
your CPU limit (resource_limits.max_cpu_cores): a leaf that declares 2–4 given
6 runs at 4. Tasks are booked at the cores they are given, so the tasks
running together never use more than the CPU limit. "default" (or 0) returns
the leaf to its own range.

The setting applies to tasks started afterwards: a running task keeps the
cores it started with until it finishes, or until you restart it
(` + "`lettuce-volunteer tasks restart <id>`" + `). With the daemon running the
change is applied at once; otherwise it is saved for the next start.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLeafOverride(cmd, args, "cores")
		},
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func newLeafsMaxRunningCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "max-running <slug> <count|none>",
		Short: "Cap how many of a leaf's tasks run at once on this machine",
		Long: `Cap how many of a leaf's tasks run at once on this machine.

Without a cap, as many run as your CPU and memory limits hold. With one, no
more than that many of the leaf's tasks run together, and Lettuce buffers no
more of the leaf's work than that many tasks can get through in
work_buffer_hours — so a cap never makes it fetch work it cannot run. "none"
(or 0) removes the cap.

With the daemon running the change is applied at once; otherwise it is saved
for the next start.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLeafOverride(cmd, args, "max_running")
		},
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

// parseLeafOverride reads an override's value: a positive integer, or
// clearWord / 0 to remove the override (returned as 0).
func parseLeafOverride(raw, clearWord string) (int, error) {
	if strings.EqualFold(raw, clearWord) {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("expected a positive whole number or %q, got %q", clearWord, raw)
	}
	return n, nil
}

// runLeafOverride sets (or clears) one per-leaf override on every matching
// server and applies it: live through the running daemon when there is one,
// else by saving config.yaml.
func runLeafOverride(cmd *cobra.Command, args []string, key string) error {
	slug := args[0]
	clearWord := "default"
	if key == "max_running" {
		clearWord = "none"
	}
	value, err := parseLeafOverride(args[1], clearWord)
	if err != nil {
		return err
	}
	serverFilter, _ := cmd.Flags().GetString("server")

	var changed []config.ServerConfig
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}
		lp := &cfg.Servers[i].LeafPreferences
		m := &lp.Cores
		if key == "max_running" {
			m = &lp.MaxRunning
		}
		if value > 0 {
			if *m == nil {
				*m = make(map[string]int)
			}
			(*m)[slug] = value
		} else {
			delete(*m, slug)
			if len(*m) == 0 {
				*m = nil
			}
		}
		changed = append(changed, cfg.Servers[i])
	}
	if len(changed) == 0 {
		return fmt.Errorf("no matching server found")
	}

	if err := applyLeafOverridesLive(changed, key); err == nil {
		for _, srv := range changed {
			fmt.Println(describeLeafOverride(srv.DisplayName(), slug, key, value))
		}
		printLiveLeafCPU(changed, slug)
		return nil
	} else if !isDaemonAbsent(err) {
		return fmt.Errorf("the running daemon refused the change: %w", err)
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	for _, srv := range changed {
		fmt.Println(describeLeafOverride(srv.DisplayName(), slug, key, value))
	}
	fmt.Println("Saved to config.yaml; it takes effect when the daemon starts.")
	return nil
}

// describeLeafOverride is the one-line confirmation of a change.
func describeLeafOverride(server, slug, key string, value int) string {
	switch {
	case key == "cores" && value > 0:
		return fmt.Sprintf("Leaf %q on server %q: each task is to be given %s (kept within the leaf's range and your CPU limit).", slug, server, pluralCount(value, "core"))
	case key == "cores":
		return fmt.Sprintf("Leaf %q on server %q: tasks are given cores from the leaf's own range again.", slug, server)
	case value > 0:
		return fmt.Sprintf("Leaf %q on server %q: at most %d of its tasks run at once.", slug, server, value)
	default:
		return fmt.Sprintf("Leaf %q on server %q: no cap of its own on how many tasks run at once.", slug, server)
	}
}

// pluralCount prints "1 core", "3 cores".
func pluralCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// applyLeafOverridesLive sends the changed servers' override maps to the
// running daemon (PUT /api/v1/config), which saves and applies them. An error
// wrapping errDaemonAbsent means no daemon answered.
func applyLeafOverridesLive(servers []config.ServerConfig, key string) error {
	entries := make([]any, 0, len(servers))
	for _, srv := range servers {
		m := srv.LeafPreferences.Cores
		if key == "max_running" {
			m = srv.LeafPreferences.MaxRunning
		}
		if m == nil {
			m = map[string]int{}
		}
		entries = append(entries, map[string]any{
			"name":             srv.DisplayName(),
			"leaf_preferences": map[string]any{key: m},
		})
	}
	return managementSend(cfg.DataDir, http.MethodPut, "/api/v1/config", map[string]any{"servers": entries}, nil)
}

// printLiveLeafCPU says what the change comes to, as the running daemon now
// sees it: the cores each of the leaf's tasks is given and how many run at
// once. Silent when the daemon's leaf list cannot be read.
func printLiveLeafCPU(servers []config.ServerConfig, slug string) {
	resp, err := fetchHeadsFromAPI()
	if err != nil {
		return
	}
	for _, h := range resp.Heads {
		for _, srv := range servers {
			if !strings.EqualFold(h.GRPCAddress, srv.GRPCAddress) && h.Name != srv.DisplayName() {
				continue
			}
			for _, l := range h.Leafs {
				if l.Slug != slug || l.CPU == nil {
					continue
				}
				fmt.Printf("  Now on %s: each %s task is given %s; at most %d run at once. Running tasks keep the cores they started with (`lettuce-volunteer tasks restart <id>` restarts one).\n",
					h.Name, slug, coreRangeLabel(l.CPU.TaskCoresMin, l.CPU.TaskCoresMax), l.CPU.RunsAtOnce)
			}
		}
	}
}

// coreRangeLabel prints a task's core range: "3 cores", "2–4 cores".
func coreRangeLabel(minCores, maxCores int) string {
	if maxCores > minCores {
		return fmt.Sprintf("%d–%d cores", minCores, maxCores)
	}
	return pluralCount(minCores, "core")
}

// errDaemonAbsent marks a management call that found no daemon to answer it.
var errDaemonAbsent = errors.New("daemon not running")

// isDaemonAbsent reports whether err means no daemon answered.
func isDaemonAbsent(err error) bool {
	return errors.Is(err, errDaemonAbsent)
}

// managementSend performs an authenticated request with a JSON body against
// the running daemon's local management API and, when out is non-nil, decodes
// the JSON reply into it. A refusal carries the API's own message.
func managementSend(dataDir, method, path string, body, out any) error {
	info, err := management.ReadDaemonInfo(dataDir)
	if err != nil {
		return fmt.Errorf("%w (no daemon.json)", errDaemonAbsent)
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%w (unreachable: %v)", errDaemonAbsent, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &apiErr)
		msg := apiErr.Error.Message
		if msg == "" {
			msg = apiErr.Message
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("management API returned %d: %s", resp.StatusCode, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
