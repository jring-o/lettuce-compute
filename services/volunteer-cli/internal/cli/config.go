package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or edit configuration",
		Long:  "Display current configuration, or get/set individual values.",
		RunE:  runConfigShow,
	}

	cmd.AddCommand(newConfigSetCmd())
	cmd.AddCommand(newConfigGetCmd())

	return cmd
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	fmt.Print(string(out))
	return nil
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long:  "Update a config key using dot-notation (e.g., resource_limits.max_cpu_cores 4).",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.SetByPath(args[0], args[1]); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}
			if err := cfg.Save(cfgPath); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Printf("%s = %s\n", args[0], args[1])
			return nil
		},
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Get a configuration value",
		Long:  "Retrieve a config value using dot-notation (e.g., resource_limits.max_cpu_cores).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			val, err := cfg.GetByPath(args[0])
			if err != nil {
				return err
			}
			fmt.Println(val)
			return nil
		},
	}
}
