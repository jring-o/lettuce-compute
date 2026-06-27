package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/spf13/cobra"
)

func newScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Show or set when the volunteer runs (daily time windows)",
		Long: `Control when the volunteer computes.

With no arguments, prints the current schedule. Use "schedule set" to run only
within a daily time window — for example overnight, when the machine is cool and
idle — "schedule add" to layer additional windows (e.g. a different weekend
window), and "schedule clear" to go back to running always.

Windows are whole-hour and may wrap past midnight, so a "dusk till dawn" window
is simply: schedule set --from 20:00 --to 06:00

The volunteer runs whenever the current time falls in ANY configured window, so
weekday/weekend splits are just two windows:
  schedule set --from 19:00 --to 07:00 --days mon-fri
  schedule add --from 00:00 --to 00:00 --days sat,sun   # all day Sat & Sun

Advanced: the same windows can be hand-edited in config.yaml under
scheduling.schedule_ranges, each entry being:
  - days: [0,1,2,3,4]   # 0=Mon … 6=Sun
    start_hour: 19       # 0-23
    end_hour: 7          # 0-23; end <= start means the window wraps past midnight`,
		RunE: runScheduleShow,
	}
	cmd.AddCommand(
		newScheduleSetCmd(),
		newScheduleAddCmd(),
		newScheduleClearCmd(),
		&cobra.Command{
			Use:   "show",
			Short: "Show the current schedule",
			Args:  cobra.NoArgs,
			RunE:  runScheduleShow,
		},
	)
	return cmd
}

// buildScheduleRange parses the --from/--to/--days flag trio into a ScheduleRange.
func buildScheduleRange(from, to, days string) (config.ScheduleRange, error) {
	if from == "" || to == "" {
		return config.ScheduleRange{}, fmt.Errorf("--from and --to are required (e.g. --from 20:00 --to 06:00)")
	}
	start, err := parseScheduleHour(from)
	if err != nil {
		return config.ScheduleRange{}, fmt.Errorf("--from: %w", err)
	}
	end, err := parseScheduleHour(to)
	if err != nil {
		return config.ScheduleRange{}, fmt.Errorf("--to: %w", err)
	}
	dayList, err := parseScheduleDays(days)
	if err != nil {
		return config.ScheduleRange{}, fmt.Errorf("--days: %w", err)
	}
	return config.ScheduleRange{Days: dayList, StartHour: start, EndHour: end}, nil
}

func newScheduleSetCmd() *cobra.Command {
	var from, to, days string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Run only within a daily time window",
		Long: `Switch to SCHEDULED mode and run only within the given daily window.

Examples:
  # Overnight, every day (dusk till dawn):
  lettuce-volunteer schedule set --from 20:00 --to 06:00

  # Weeknights only:
  lettuce-volunteer schedule set --from 19:00 --to 07:00 --days mon-fri

Hours are whole-hour (minutes, if given, must be :00) and the window may wrap
past midnight. This replaces any existing schedule window.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := buildScheduleRange(from, to, days)
			if err != nil {
				return err
			}

			cfg.Scheduling.Mode = "SCHEDULED"
			cfg.Scheduling.CronExpression = ""
			cfg.Scheduling.ScheduleRanges = []config.ScheduleRange{r}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}
			if err := cfg.Save(cfgPath); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Schedule set: %s\n", describeRange(r))
			switch {
			case r.StartHour == r.EndHour:
				fmt.Println("Note: --from equals --to, so this runs the full 24 hours on those days.")
			case r.StartHour > r.EndHour:
				fmt.Println("(This window wraps past midnight.)")
			}
			fmt.Println("Tip: use `schedule add` to layer a second window (e.g. a different weekend window).")
			fmt.Println("Restart the daemon for the change to take effect: lettuce-volunteer stop && lettuce-volunteer start")
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "start of the daily window, whole hour, e.g. 20:00")
	cmd.Flags().StringVar(&to, "to", "", "end of the daily window, whole hour, e.g. 06:00")
	cmd.Flags().StringVar(&days, "days", "mon-sun", "days the window applies, e.g. mon-fri, sat,sun, mon-sun")
	return cmd
}

func newScheduleAddCmd() *cobra.Command {
	var from, to, days string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add another daily window (layer on top of existing windows)",
		Long: `Append a window to the schedule instead of replacing it, so you can run
on different hours on different days.

Examples:
  # Weeknights overnight, plus all day on weekends:
  lettuce-volunteer schedule set --from 19:00 --to 07:00 --days mon-fri
  lettuce-volunteer schedule add --from 00:00 --to 00:00 --days sat,sun

The volunteer runs whenever the current time falls in ANY configured window.
Hours are whole-hour and a window may wrap past midnight.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := buildScheduleRange(from, to, days)
			if err != nil {
				return err
			}

			cfg.Scheduling.Mode = "SCHEDULED"
			cfg.Scheduling.CronExpression = ""
			cfg.Scheduling.ScheduleRanges = append(cfg.Scheduling.ScheduleRanges, r)

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}
			if err := cfg.Save(cfgPath); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Window added: %s\n", describeRange(r))
			fmt.Println("Active windows now:")
			for _, w := range cfg.Scheduling.ScheduleRanges {
				fmt.Printf("  - %s\n", describeRange(w))
			}
			fmt.Println("Restart the daemon for the change to take effect: lettuce-volunteer stop && lettuce-volunteer start")
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "start of the daily window, whole hour, e.g. 20:00")
	cmd.Flags().StringVar(&to, "to", "", "end of the daily window, whole hour, e.g. 06:00")
	cmd.Flags().StringVar(&days, "days", "mon-sun", "days the window applies, e.g. mon-fri, sat,sun, mon-sun")
	return cmd
}

func newScheduleClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Run always (remove any schedule window)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Scheduling.Mode = "ALWAYS"
			cfg.Scheduling.ScheduleRanges = nil
			cfg.Scheduling.CronExpression = ""
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}
			if err := cfg.Save(cfgPath); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Println("Schedule cleared: the volunteer will run always (mode ALWAYS).")
			fmt.Println("Restart the daemon for the change to take effect: lettuce-volunteer stop && lettuce-volunteer start")
			return nil
		},
	}
}

func runScheduleShow(cmd *cobra.Command, args []string) error {
	s := cfg.Scheduling
	mode := s.Mode
	if mode == "" {
		mode = "ALWAYS"
	}
	fmt.Printf("Mode: %s\n", mode)
	switch mode {
	case "ALWAYS":
		fmt.Println("The volunteer runs whenever the daemon is started.")
	case "WHEN_IDLE":
		fmt.Printf("The volunteer runs only after %d minute(s) of machine idle.\n", s.IdleThresholdMins)
	case "SCHEDULED":
		switch {
		case len(s.ScheduleRanges) > 0:
			fmt.Println("Active windows:")
			for _, r := range s.ScheduleRanges {
				fmt.Printf("  - %s\n", describeRange(r))
			}
		case s.CronExpression != "":
			fmt.Printf("Cron expression: %s\n", s.CronExpression)
		default:
			fmt.Println("No window configured — SCHEDULED with neither a window nor a cron expression means the volunteer never runs. Use `schedule set` or `schedule clear`.")
		}
	}
	return nil
}

// parseScheduleHour parses a whole-hour time-of-day ("20" or "20:00") into an
// hour in [0,23]. Minutes, if present, must be zero — schedule windows are
// whole-hour (the scheduler matches on the hour only).
func parseScheduleHour(s string) (int, error) {
	s = strings.TrimSpace(s)
	hourPart := s
	if i := strings.IndexByte(s, ':'); i >= 0 {
		hourPart = s[:i]
		min, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err != nil || min != 0 {
			return 0, fmt.Errorf("windows are whole-hour; minutes must be 00 (got %q)", s)
		}
	}
	h, err := strconv.Atoi(strings.TrimSpace(hourPart))
	if err != nil {
		return 0, fmt.Errorf("invalid hour %q (use 0-23, e.g. 20 or 20:00)", s)
	}
	if h < 0 || h > 23 {
		return 0, fmt.Errorf("hour must be 0-23 (got %d)", h)
	}
	return h, nil
}

var scheduleDayNames = map[string]int{
	"mon": 0, "monday": 0,
	"tue": 1, "tues": 1, "tuesday": 1,
	"wed": 2, "weds": 2, "wednesday": 2,
	"thu": 3, "thur": 3, "thurs": 3, "thursday": 3,
	"fri": 4, "friday": 4,
	"sat": 5, "saturday": 5,
	"sun": 6, "sunday": 6,
}

// parseScheduleDays parses a day spec into sorted day indices (0=Mon..6=Sun).
// Accepts comma-separated single days and inclusive ranges, e.g. "mon-fri",
// "sat,sun", "mon,wed,fri". Ranges may wrap the week (e.g. "fri-mon").
func parseScheduleDays(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "mon-sun"
	}
	seen := map[int]bool{}
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if i := strings.IndexByte(tok, '-'); i >= 0 {
			lo, ok1 := scheduleDayNames[strings.TrimSpace(tok[:i])]
			hi, ok2 := scheduleDayNames[strings.TrimSpace(tok[i+1:])]
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("unknown day in range %q (use mon..sun)", tok)
			}
			for d := lo; ; d = (d + 1) % 7 {
				seen[d] = true
				if d == hi {
					break
				}
			}
		} else {
			d, ok := scheduleDayNames[tok]
			if !ok {
				return nil, fmt.Errorf("unknown day %q (use mon..sun)", tok)
			}
			seen[d] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no days specified")
	}
	days := make([]int, 0, len(seen))
	for d := 0; d < 7; d++ {
		if seen[d] {
			days = append(days, d)
		}
	}
	return days, nil
}

var scheduleDayShort = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// describeRange renders a ScheduleRange as a human-readable line.
func describeRange(r config.ScheduleRange) string {
	var window string
	switch {
	case r.StartHour == r.EndHour:
		window = "all day"
	case r.StartHour > r.EndHour:
		window = fmt.Sprintf("%02d:00–%02d:00 (overnight)", r.StartHour, r.EndHour)
	default:
		window = fmt.Sprintf("%02d:00–%02d:00", r.StartHour, r.EndHour)
	}
	return fmt.Sprintf("%s on %s", window, formatScheduleDays(r.Days))
}

// formatScheduleDays renders day indices as "every day" or a short list.
func formatScheduleDays(days []int) string {
	if len(days) == 7 {
		return "every day"
	}
	parts := make([]string, 0, len(days))
	for _, d := range days {
		if d >= 0 && d < 7 {
			parts = append(parts, scheduleDayShort[d])
		}
	}
	if len(parts) == 0 {
		return "(no days)"
	}
	return strings.Join(parts, ", ")
}
