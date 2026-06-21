package cli

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newHeadsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "heads",
		Short: "Manage head (server) preferences (list, weight)",
	}

	cmd.AddCommand(
		newHeadsListCmd(),
		newHeadsWeightCmd(),
	)

	return cmd
}

// --- heads list ---

func newHeadsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured heads and their fetch-priority weights",
		RunE:  runHeadsList,
	}
}

func runHeadsList(cmd *cobra.Command, args []string) error {
	if len(cfg.Servers) == 0 {
		fmt.Println("No servers configured. Run `lettuce-volunteer attach --server <host>` first.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVER\tADDRESS\tWEIGHT\n")
	for _, srv := range cfg.Servers {
		weight := srv.Weight
		if weight <= 0 {
			weight = 100 // the daemon's effective default
		}
		fmt.Fprintf(w, "%s\t%s\t%d\n", srv.DisplayName(), srv.GRPCAddress, weight)
	}
	w.Flush()
	return nil
}

// --- heads weight ---

func newHeadsWeightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "weight <server> <weight>",
		Short: "Set this volunteer's fetch-priority weight for a head",
		Long: `Set this volunteer's local fetch-priority weight for a configured head.

Heads are selected in deficit order across their weights, so a head with
weight 200 receives roughly twice the share of your work as one with weight
100 (the default). Use ` + "`lettuce-volunteer heads list`" + ` to see the
configured head names.

The change is saved to config.yaml and takes effect on the next daemon start.`,
		Args: cobra.ExactArgs(2),
		RunE: runHeadsWeight,
	}
}

func runHeadsWeight(cmd *cobra.Command, args []string) error {
	server := args[0]
	weight, err := strconv.Atoi(args[1])
	if err != nil || weight <= 0 {
		return fmt.Errorf("weight must be a positive integer, got %q", args[1])
	}

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if name != server {
			continue
		}
		oldWeight := cfg.Servers[i].Weight
		if oldWeight <= 0 {
			oldWeight = 100
		}
		cfg.Servers[i].Weight = weight
		modified = true
		fmt.Printf("Set weight for head %q: %d → %d\n", name, oldWeight, weight)
	}

	if !modified {
		return fmt.Errorf("no configured head named %q (see `lettuce-volunteer heads list`)", server)
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Println("Saved. Restart the daemon for the change to take effect.")
	return nil
}
