package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/spf13/cobra"
)

// version is overridden via ldflags at build time.
var version = "dev"

var (
	cfgPath  string
	logLevel string
	logFile  string
	dataDir  string
	cfg      *config.Config
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "lettuce-volunteer",
		Short:   "Lettuce volunteer compute client",
		Long:    "Volunteer your computing resources to distributed science via the Lettuce network.",
		Version: version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip config loading for init command — it creates the config.
			if cmd.Name() == "init" {
				return nil
			}

			var err error
			cfg, err = config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// Override log level if set via flag.
			if cmd.Flags().Changed("log-level") {
				cfg.LogLevel = logLevel
			}
			if cmd.Flags().Changed("data-dir") {
				cfg.DataDir = dataDir
			}
			if cmd.Flags().Changed("log-file") {
				cfg.LogFile = logFile
			}
			return nil
		},
		SilenceUsage: true,
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	defaultDataDir := filepath.Join(home, ".lettuce")
	defaultCfgPath := filepath.Join(defaultDataDir, "config.yaml")

	root.PersistentFlags().StringVar(&cfgPath, "config", defaultCfgPath, "path to config file")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	root.PersistentFlags().StringVar(&dataDir, "data-dir", defaultDataDir, "path to data directory")
	root.PersistentFlags().StringVar(&logFile, "log-file", "", "log file path (default <data-dir>/logs/volunteer.log; logs also go to stderr)")

	root.AddCommand(
		newInitCmd(),
		newConfigCmd(),
		newStartCmd(),
		newStopCmd(),
		newStatusCmd(),
		newScheduleCmd(),
		newProjectsCmd(),
		newLeafsCmd(),
		newHeadsCmd(),
		newAttachCmd(),
		newDetachCmd(),
		newHistoryCmd(),
		newUpdateCmd(),
		newProveIdentityCmd(),
		newDoctorCmd(),
	)

	return root
}

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}
