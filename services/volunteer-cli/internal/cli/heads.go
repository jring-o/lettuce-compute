package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
		newHeadsTrustCmd(),
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
	fmt.Fprintf(w, "SERVER\tADDRESS\tWEIGHT\tMAY RUN\n")
	for _, srv := range cfg.Servers {
		weight := srv.Weight
		if weight <= 0 {
			weight = 100 // the daemon's effective default
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", srv.DisplayName(), srv.GRPCAddress, weight, strings.Join(srv.EffectiveTrustedRuntimes(), ","))
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

// --- heads trust ---

func newHeadsTrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust <server> [runtimes]",
		Short: "View or set which runtimes you trust a head to run on this machine",
		Long: `View or set this volunteer's per-head runtime trust.

A head is a trust domain: this controls which runtime kinds that head may run on
YOUR machine. WASM is always allowed (it is fully sandboxed). CONTAINER and NATIVE
are opt-ins you grant per head.

  lettuce-volunteer heads trust my-head                  # show current trust
  lettuce-volunteer heads trust my-head container        # allow WASM + CONTAINER
  lettuce-volunteer heads trust my-head container,native # allow WASM + CONTAINER + NATIVE
  lettuce-volunteer heads trust my-head none             # WASM only

NATIVE runs code directly on your machine with no sandbox — grant it only to an
operator you fully trust. The change is saved to config.yaml and takes effect on
the next daemon start.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: runHeadsTrust,
	}
}

func runHeadsTrust(cmd *cobra.Command, args []string) error {
	server := args[0]
	idx := -1
	for i := range cfg.Servers {
		if cfg.Servers[i].DisplayName() == server {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no configured head named %q (see `lettuce-volunteer heads list`)", server)
	}

	// View mode: `heads trust <server>` with no runtimes argument.
	if len(args) == 1 {
		fmt.Printf("Head %q may run: %s\n", server, trustSummary(cfg.Servers[idx].TrustedRuntimes))
		return nil
	}

	// Set mode.
	trusted, err := parseTrustRuntimes(args[1])
	if err != nil {
		return err
	}
	oldSummary := trustSummary(cfg.Servers[idx].TrustedRuntimes)
	cfg.Servers[idx].TrustedRuntimes = trusted
	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Printf("Head %q runtime trust: %s → %s\n", server, oldSummary, trustSummary(trusted))
	for _, r := range trusted {
		if r == "NATIVE" {
			fmt.Println("Note: NATIVE runs code on this machine with no sandbox — grant only to a fully trusted operator.")
			break
		}
	}
	fmt.Println("Saved. Restart the daemon for the change to take effect.")
	return nil
}
