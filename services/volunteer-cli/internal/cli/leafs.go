package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/spf13/cobra"
)

func newLeafsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leafs",
		Short: "Manage leaf preferences (list, enable, disable, weight, reset)",
	}

	cmd.AddCommand(
		newLeafsListCmd(),
		newLeafsEnableCmd(),
		newLeafsDisableCmd(),
		newLeafsWeightCmd(),
		newLeafsResetCmd(),
	)

	return cmd
}

// --- leafs list ---

func newLeafsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available leafs from all connected servers",
		RunE:  runLeafsList,
	}
}

// leafsAPIResponse is the shape of GET /api/v1/heads from the management API.
type leafsAPIResponse struct {
	Heads []leafsAPIHead `json:"heads"`
}

type leafsAPIHead struct {
	Name   string          `json:"name"`
	Weight int             `json:"weight"`
	Leafs  []leafsAPILeaf  `json:"leafs"`
}

type leafsAPILeaf struct {
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	State            string `json:"state"`
	QueuedWorkUnits  int    `json:"queued_work_units"`
	ActiveVolunteers int    `json:"active_volunteers"`
	EffectiveWeight  int    `json:"effective_weight"`
	Enabled          bool   `json:"enabled"`
}

func runLeafsList(cmd *cobra.Command, args []string) error {
	if len(cfg.Servers) == 0 {
		fmt.Println("No servers configured. Run `lettuce-volunteer attach --server <host>` first.")
		return nil
	}

	// Try to get leaf info from the management API (daemon running).
	mgmtURL := fmt.Sprintf("http://127.0.0.1:%d", managementPort())
	heads, err := fetchHeadsFromAPI(mgmtURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Daemon not running or unreachable, showing config-only info.\n\n")
		return printLeafsFromConfig()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVER\tSLUG\tNAME\tSTATE\tQUEUED\tVOLUNTEERS\tWEIGHT\tENABLED\n")

	for _, h := range heads {
		for _, l := range h.Leafs {
			enabled := "✓"
			if !l.Enabled {
				enabled = "✗"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
				h.Name, l.Slug, l.Name, l.State,
				l.QueuedWorkUnits, l.ActiveVolunteers,
				l.EffectiveWeight, enabled,
			)
		}
	}
	w.Flush()
	return nil
}

func fetchHeadsFromAPI(baseURL string) ([]leafsAPIHead, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/api/v1/heads")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management API returned status %d", resp.StatusCode)
	}

	var result leafsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Heads, nil
}

func printLeafsFromConfig() error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVER\tMODE\tWEIGHT\tENABLED SLUGS\tDISABLED SLUGS\n")
	for _, srv := range cfg.Servers {
		name := srv.DisplayName()
		mode := srv.LeafPreferences.Mode
		if mode == "" {
			mode = "ALL"
		}
		weight := srv.Weight
		if weight <= 0 {
			weight = 100
		}
		enabled := "-"
		if len(srv.LeafPreferences.Enabled) > 0 {
			enabled = fmt.Sprintf("%v", srv.LeafPreferences.Enabled)
		}
		disabled := "-"
		if len(srv.LeafPreferences.Disabled) > 0 {
			disabled = fmt.Sprintf("%v", srv.LeafPreferences.Disabled)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", name, mode, weight, enabled, disabled)
	}
	w.Flush()
	return nil
}

func managementPort() int {
	info, err := management.ReadDaemonInfo(cfg.DataDir)
	if err == nil && info.Port > 0 {
		return info.Port
	}
	return 7780
}

// --- leafs enable ---

func newLeafsEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable <slug>",
		Short: "Enable a leaf on a server",
		Args:  cobra.ExactArgs(1),
		RunE:  runLeafsEnable,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsEnable(cmd *cobra.Command, args []string) error {
	slug := args[0]
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		mode := lp.Mode
		if mode == "" {
			mode = "ALL"
		}

		switch mode {
		case "ALL":
			// Switch to BLOCKLIST and ensure slug is not in Disabled.
			lp.Mode = "BLOCKLIST"
			lp.Disabled = removeFromSlice(lp.Disabled, slug)
		case "BLOCKLIST":
			lp.Disabled = removeFromSlice(lp.Disabled, slug)
		case "SPECIFIC":
			if !contains(lp.Enabled, slug) {
				lp.Enabled = append(lp.Enabled, slug)
			}
		}
		modified = true
		fmt.Printf("Enabled leaf %q on server %q (mode: %s)\n", slug, name, lp.Mode)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs disable ---

func newLeafsDisableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable <slug>",
		Short: "Disable a leaf on a server",
		Args:  cobra.ExactArgs(1),
		RunE:  runLeafsDisable,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsDisable(cmd *cobra.Command, args []string) error {
	slug := args[0]
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		mode := lp.Mode
		if mode == "" {
			mode = "ALL"
		}

		switch mode {
		case "ALL":
			lp.Mode = "BLOCKLIST"
			if !contains(lp.Disabled, slug) {
				lp.Disabled = append(lp.Disabled, slug)
			}
		case "BLOCKLIST":
			if !contains(lp.Disabled, slug) {
				lp.Disabled = append(lp.Disabled, slug)
			}
		case "SPECIFIC":
			lp.Enabled = removeFromSlice(lp.Enabled, slug)
		}
		modified = true
		fmt.Printf("Disabled leaf %q on server %q (mode: %s)\n", slug, name, lp.Mode)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs weight ---

func newLeafsWeightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "weight <slug> <weight>",
		Short: "Set custom weight for a leaf",
		Args:  cobra.ExactArgs(2),
		RunE:  runLeafsWeight,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsWeight(cmd *cobra.Command, args []string) error {
	slug := args[0]
	weight, err := strconv.Atoi(args[1])
	if err != nil || weight <= 0 {
		return fmt.Errorf("weight must be a positive integer, got %q", args[1])
	}
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		if lp.Weights == nil {
			lp.Weights = make(map[string]int)
		}
		oldWeight := lp.Weights[slug]
		lp.Weights[slug] = weight
		modified = true
		fmt.Printf("Set weight for leaf %q on server %q: %d → %d\n", slug, name, oldWeight, weight)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs reset ---

func newLeafsResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset leaf preferences to researcher defaults",
		RunE:  runLeafsReset,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsReset(cmd *cobra.Command, args []string) error {
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		cfg.Servers[i].LeafPreferences = config.LeafPreferences{Mode: "ALL"}
		modified = true
		fmt.Printf("Reset leaf preferences for server %q to ALL (researcher defaults)\n", name)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- helpers ---

func removeFromSlice(s []string, val string) []string {
	var result []string
	for _, v := range s {
		if v != val {
			result = append(result, v)
		}
	}
	return result
}

func contains(s []string, val string) bool {
	for _, v := range s {
		if v == val {
			return true
		}
	}
	return false
}
