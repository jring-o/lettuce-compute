package cli

import (
	"errors"
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
			// An explicit --data-dir names an isolated profile: everything the
			// volunteer persists — config included — lives inside it. Unless
			// --config points somewhere explicitly, the profile's config is
			// <data-dir>/config.yaml, never the default profile's
			// ~/.lettuce/config.yaml (which an isolated run must not read, and
			// init must not rewrite). The path is made absolute first: the
			// daemon resolves cached binaries from it while compute children
			// run in their own working directories, so a relative value breaks
			// execution far from where it was typed.
			if cmd.Flags().Changed("data-dir") {
				abs, err := filepath.Abs(dataDir)
				if err != nil {
					return fmt.Errorf("resolving --data-dir: %w", err)
				}
				dataDir = abs
				if !cmd.Flags().Changed("config") {
					cfgPath = filepath.Join(dataDir, "config.yaml")
				}
			}

			// Skip config loading for init command — it creates the config.
			if cmd.Name() == "init" {
				return nil
			}

			var err error
			cfg, err = config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// --log-level and --log-file are one-time overrides for THIS
			// command, held apart from the serialized config so no later
			// cfg.Save turns them into the permanent setting (TB-5). The level
			// is validated and case-folded here rather than at the point of
			// use: an unrecognized value used to fall through to info silently,
			// so a typo — or plain `--log-level DEBUG` — cost the volunteer the
			// debug logs they asked for with nothing said (TB-1).
			levelOverride := ""
			if cmd.Flags().Changed("log-level") {
				canonical, err := normalizeLogLevel(logLevel)
				if err != nil {
					return err
				}
				levelOverride = canonical
			}
			fileOverride := ""
			if cmd.Flags().Changed("log-file") {
				fileOverride = logFile
			}
			cfg.SetLogOverrides(levelOverride, fileOverride)

			// --data-dir names the whole profile the config lives in, so
			// persisting it inside that profile is self-consistent; it stays an
			// ordinary field.
			if cmd.Flags().Changed("data-dir") {
				cfg.DataDir = dataDir
			}
			// A relative data_dir written INTO the config file breaks the same
			// way a relative flag does; resolve once here for every command.
			if abs, absErr := filepath.Abs(cfg.DataDir); absErr == nil {
				cfg.DataDir = abs
			}
			return nil
		},
		SilenceUsage: true,
	}

	// Cobra parses flags BEFORE it validates positional arguments, so a mistyped
	// subcommand is reported as a bad flag: `schedule ad --from 03:00` fails with
	// "unknown flag: --from" and sends the volunteer off to fix flags that were
	// never wrong (TB-6). An Args constraint alone cannot catch this — it runs
	// too late. When the command that failed owns subcommands and picked up a
	// stray token matching none of them, name the real mistake instead. The
	// handler is inherited by every subcommand, so setting it on root is enough.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if cmd.HasSubCommands() {
			if stray := cmd.Flags().Args(); len(stray) > 0 {
				return unknownSubcommandError(cmd, stray[0])
			}
		}
		return err
	})

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
		newCreditCmd(),
		newScheduleCmd(),
		newProjectsCmd(),
		newLeafsCmd(),
		newHeadsCmd(),
		newAttachCmd(),
		newDetachCmd(),
		newHistoryCmd(),
		newUpdateCmd(),
		newProveIdentityCmd(),
		newBindDIDCmd(),
		newDoctorCmd(),
		newAuditRunnerCmd(),
	)

	return root
}

// noStrayArgs is the Args validator for a command that owns subcommands. Cobra's
// default only rejects an unknown subcommand on the ROOT command, so a typo'd
// child — `lettuce-volunteer schedule ad` — is handed to the parent as a
// positional argument and quietly ignored (TB-6). Runnable parents need this
// explicitly; non-runnable ones would otherwise print their help as if nothing
// were wrong.
func noStrayArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return unknownSubcommandError(cmd, args[0])
}

// unknownSubcommandError renders cobra's own "unknown command" message for a
// token that matched no subcommand of cmd, including its spelling suggestions
// so `schedule ad` points at `add`.
func unknownSubcommandError(cmd *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	// Mirror cobra's own lazy default; SuggestionsFor honors the field but,
	// unlike the root path, never initializes it.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, s := range suggestions {
			msg += fmt.Sprintf("\t%s\n", s)
		}
	}
	return errors.New(msg)
}

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}
