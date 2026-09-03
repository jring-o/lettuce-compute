package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	rtdetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Interactive first-run setup",
		Long:  "Generate identity, configure resource limits, scheduling, leaf preferences, and server connection.",
		RunE:  runInit,
	}
	cmd.Flags().Int("cpu-cores", 0, "Max CPU cores to use")
	cmd.Flags().Int("memory-mb", 0, "Max memory in MB")
	cmd.Flags().Int("gpu-vram-pct", -1, "Max GPU VRAM percentage (0 disables GPU)")
	cmd.Flags().Int("disk-gb", 0, "Max disk storage in GB")
	cmd.Flags().String("schedule-mode", "", "Scheduling mode (always, idle, scheduled)")
	cmd.Flags().Int("idle-threshold", 0, "Idle threshold in minutes")
	cmd.Flags().String("server", "", "Head to connect to: host, host:port, or an http(s):// URL (compute.example.org, https://compute.example.org/)")
	cmd.Flags().String("trust", "", "With --server: runtimes to trust that head to run — comma list of container,native (wasm is always allowed). Omitted means WASM only")
	cmd.Flags().String("enabled-leafs", "", "Comma-separated leaf slugs to enable (sets SPECIFIC mode)")
	return cmd
}

func runInit(cmd *cobra.Command, args []string) error {
	trustFlag, _ := cmd.Flags().GetString("trust")
	// Determine if running non-interactively (flags provided by desktop app).
	nonInteractive := cmd.Flags().Changed("cpu-cores") || cmd.Flags().Changed("memory-mb") ||
		cmd.Flags().Changed("schedule-mode") || cmd.Flags().Changed("server")

	scanner := bufio.NewScanner(os.Stdin)

	// Check if config already exists.
	configExists := false
	if _, err := os.Stat(cfgPath); err == nil {
		configExists = true
		if nonInteractive {
			// Desktop app re-init: overwrite silently.
		} else {
			fmt.Print("Config already exists. Reinitialize? [y/N] ")
			if !scanner.Scan() || !isYes(scanner.Text()) {
				fmt.Println("Aborted.")
				return nil
			}
		}
	}

	// Base a RE-init on the existing file so prompts show [current] values and
	// unspecified fields (tuned limits, servers, leaf preferences) are preserved
	// instead of being silently reset to factory defaults (#30). A fresh init starts
	// from defaults and derives resource proposals from this machine's hardware below.
	c := config.Defaults()
	deriveFresh := true
	if configExists {
		loaded, err := config.Load(cfgPath)
		if err != nil {
			fmt.Printf("Warning: could not read existing config (%v); starting from defaults.\n", err)
		} else {
			c = loaded
			deriveFresh = false
		}
	}
	c.DataDir = dataDir
	// Identity paths are derived from the data dir (KeyFilePath/PubKeyFilePath).
	// Clear any explicit values a pre-existing config baked in — the old behavior
	// overwrote them with <data-dir> paths anyway — so the saved profile stays
	// self-contained under its data dir; and drop the retired host_id_file, which
	// nothing reads (host identity is head-issued, stored in host-ids.json).
	c.KeyFile = ""
	c.PubKeyFile = ""
	c.HostIDFile = ""

	keyFile := c.KeyFilePath()
	pubKeyFile := c.PubKeyFilePath()

	// Step 1: Identity — always generate/load keypair.
	if identity.KeyPairExists(keyFile, pubKeyFile) {
		if !nonInteractive {
			fmt.Println("\n=== Step 1: Identity ===")
			fmt.Println("Existing keypair found. Keeping current identity.")
		}
		pub, _, err := identity.LoadKeyPair(keyFile, pubKeyFile)
		if err != nil {
			return fmt.Errorf("loading existing keypair: %w", err)
		}
		if !nonInteractive {
			fmt.Printf("Public key: %s\n", identity.PublicKeyToBase64URL(pub))
		}
	} else {
		if !nonInteractive {
			fmt.Println("\n=== Step 1: Identity ===")
			fmt.Println("Generating new Ed25519 keypair...")
		}
		pub, priv, err := identity.Generate()
		if err != nil {
			return fmt.Errorf("generating keypair: %w", err)
		}
		if err := identity.SaveKeyPair(keyFile, pubKeyFile, priv, pub); err != nil {
			return fmt.Errorf("saving keypair: %w", err)
		}
		if !nonInteractive {
			fmt.Printf("Public key: %s\n", identity.PublicKeyToBase64URL(pub))
			fmt.Printf("Keys saved to %s\n", dataDir)
		}
	}

	// Host identity is HEAD-ISSUED (BG-25): the head mints a per-machine host id at
	// registration and the client persists it per-head in <DataDir>/host-ids.json. So
	// init no longer creates a host id — there is nothing to generate before first
	// contact, and the head is the sole minter. The id is acquired on the first
	// `start` (empty request => the head mints one under the per-account cap).

	// Fresh init: size the resource ceilings to this machine so a default volunteer is
	// eligible for standard leafs. The prior static defaults (2048 MB / 10 GB) left
	// max_memory_mb below the 4096 MB standard leaf cap, so a freshly-configured
	// volunteer silently matched no work (#30). Done after the data dir exists (above)
	// so free-disk detection reads the real volume. A re-init keeps the loaded values.
	if deriveFresh {
		c.ResourceLimits.MaxMemoryMB = proposeMemoryMB(int(client.TotalMemoryMB()))
		c.ResourceLimits.MaxDiskGB = proposeDiskGB(client.DiskAvailableMB(dataDir))
	}

	if nonInteractive {
		// Apply flags directly — skip all interactive prompts.
		if v, _ := cmd.Flags().GetInt("cpu-cores"); v > 0 {
			c.ResourceLimits.MaxCPUCores = v
		}
		if v, _ := cmd.Flags().GetInt("memory-mb"); v > 0 {
			c.ResourceLimits.MaxMemoryMB = v
		}
		if cmd.Flags().Changed("gpu-vram-pct") {
			v, _ := cmd.Flags().GetInt("gpu-vram-pct")
			if v >= 0 {
				c.ResourceLimits.MaxGPUVRAMPct = v
			}
		}
		if v, _ := cmd.Flags().GetInt("disk-gb"); v > 0 {
			c.ResourceLimits.MaxDiskGB = v
		}
		if v, _ := cmd.Flags().GetString("schedule-mode"); v != "" {
			switch strings.ToLower(v) {
			case "idle":
				c.Scheduling.Mode = "WHEN_IDLE"
				if t, _ := cmd.Flags().GetInt("idle-threshold"); t > 0 {
					c.Scheduling.IdleThresholdMins = t
				}
			case "scheduled":
				c.Scheduling.Mode = "SCHEDULED"
			default:
				c.Scheduling.Mode = "ALWAYS"
			}
		}

		// Keep MaxConcurrentTasks at its default of 1 (config.Defaults). Memory-
		// bound leaves (e.g. large model ensembles) consume tens of GB per task,
		// so auto-scaling concurrency to the CPU-core count oversubscribed RAM and
		// produced duplicate runs. The daemon's memory/GPU-aware admission still
		// runs more than one task when the machine genuinely has room, and the
		// operator can raise max_concurrent_tasks explicitly if desired.

		// Container backend preference. Which runtimes actually run is decided per
		// head (trusted_runtimes, chosen at attach or via `heads trust`) plus a live
		// engine probe at daemon start (TB-25) — init only records which detected
		// engine to prefer when both exist.
		backend := detectContainerBackendFunc(rtdetect.BundledPodmanPath())
		if backend.Backend != rtdetect.BackendNone {
			c.ContainerBackend = string(backend.Backend)
		}

		// Server.
		if host, _ := cmd.Flags().GetString("server"); host != "" {
			// Per-head runtime trust, from --trust. There is no one to prompt here, so
			// the flag IS the consent; omitting it is an informed WASM-only default
			// because the flag is documented on this command (TB-7).
			trusted, terr := parseTrustRuntimes(trustFlag)
			if terr != nil {
				return fmt.Errorf("--trust: %w", terr)
			}
			sc, err := serverConfigFromAddress(host)
			if err != nil {
				return fmt.Errorf("--server: %w", err)
			}
			applyInitTrust(&sc, trusted)
			c.Servers = []config.ServerConfig{sc}
		}

		// Per-server leaf preferences (from wizard leaf selection).
		if el, _ := cmd.Flags().GetString("enabled-leafs"); el != "" && len(c.Servers) > 0 {
			slugs := splitAndTrim(el)
			if len(slugs) > 0 {
				c.Servers[len(c.Servers)-1].LeafPreferences = config.LeafPreferences{
					Mode:    "SPECIFIC",
					Enabled: slugs,
				}
			}
		}
	} else {
		// Interactive mode — original prompts.

		// Step 2: Resource Limits
		fmt.Println("\n=== Step 2: Resource Limits ===")
		c.ResourceLimits.MaxCPUCores = promptInt(scanner, fmt.Sprintf("Max CPU cores [%d]", c.ResourceLimits.MaxCPUCores), c.ResourceLimits.MaxCPUCores)
		c.ResourceLimits.MaxMemoryMB = promptInt(scanner, fmt.Sprintf("Max memory MB [%d]", c.ResourceLimits.MaxMemoryMB), c.ResourceLimits.MaxMemoryMB)
		c.ResourceLimits.MaxDiskGB = promptInt(scanner, fmt.Sprintf("Max disk GB [%d]", c.ResourceLimits.MaxDiskGB), c.ResourceLimits.MaxDiskGB)

		// Step 2b: GPU
		//
		// Like every other prompt on a re-init, the defaults are the CURRENT values.
		// This prompt used to default to enabled/50 regardless of the config, so a
		// volunteer who had tuned the percentage — or disabled GPU outright — and
		// pressed Enter through a re-init silently lost that choice (TB-28).
		fmt.Println("\n=== GPU Detection ===")
		gpus := detectGPUsFunc()
		if len(gpus) > 0 {
			fmt.Printf("Detected %d GPU(s):\n", len(gpus))
			for i, g := range gpus {
				if g.VRAMMB > 0 {
					fmt.Printf("  [%d] %s (%s, %d MB VRAM)\n", i, g.Model, g.Vendor, g.VRAMMB)
				} else {
					fmt.Printf("  [%d] %s (%s)\n", i, g.Model, g.Vendor)
				}
			}
			allowPrompt, allowDefault := "Allow GPU tasks? [Y/n]", "y"
			if c.ResourceLimits.MaxGPUVRAMPct == 0 {
				allowPrompt, allowDefault = "Allow GPU tasks? [y/N]", "n"
			}
			allowGPU := promptString(scanner, allowPrompt, allowDefault)
			if strings.ToLower(allowGPU) == "n" || strings.ToLower(allowGPU) == "no" {
				c.ResourceLimits.MaxGPUVRAMPct = 0
			} else {
				pctDefault := c.ResourceLimits.MaxGPUVRAMPct
				if pctDefault <= 0 {
					pctDefault = 50 // re-enabling after a disable: propose the factory default
				}
				c.ResourceLimits.MaxGPUVRAMPct = promptInt(scanner, fmt.Sprintf("Max VRAM percentage [%d]", pctDefault), pctDefault)
			}
		} else {
			fmt.Println("No GPUs detected.")
		}

		// Step 3: Scheduling
		//
		// The defaults are the CURRENT mode and windows, exactly as the resource
		// prompts above propose current values. This step used to default to the
		// literal `always`, so a volunteer who had scheduled overnight-only crunching
		// and pressed Enter through a re-init silently went 24/7 — while their
		// windows stayed in the file, inert, saying otherwise (TB-28).
		fmt.Println("\n=== Step 3: Scheduling ===")
		fmt.Println("Modes: always, idle, scheduled")
		curMode := "always"
		switch c.Scheduling.Mode {
		case "WHEN_IDLE":
			curMode = "idle"
		case "SCHEDULED":
			curMode = "scheduled"
		}
		mode := promptString(scanner, fmt.Sprintf("Scheduling mode [%s]", curMode), curMode)
		switch strings.ToLower(mode) {
		case "idle":
			c.Scheduling.Mode = "WHEN_IDLE"
			idleDefault := c.Scheduling.IdleThresholdMins
			if idleDefault <= 0 {
				idleDefault = 5
			}
			c.Scheduling.IdleThresholdMins = promptInt(scanner, fmt.Sprintf("Idle threshold minutes [%d]", idleDefault), idleDefault)
		case "scheduled":
			c.Scheduling.Mode = "SCHEDULED"
			// Daily windows, not raw cron. This step used to ask for a bare "Cron
			// expression" and store whatever was typed, so an answer that was not a
			// 5-field cron — flags, a time range, a typo — produced a volunteer that
			// looked configured and never ran (TB-3), and left a cron expression for
			// `schedule add` to silently delete later (TB-2). Windows are the same
			// language `schedule set` speaks and are validated as they are entered.
			c.Scheduling.CronExpression = ""
			if len(c.Scheduling.ScheduleRanges) > 0 {
				fmt.Println("Current windows:")
				for _, r := range c.Scheduling.ScheduleRanges {
					fmt.Printf("  %s\n", describeRange(r))
				}
				keep := promptString(scanner, "Keep these windows? [Y/n]", "y")
				if strings.ToLower(keep) == "n" || strings.ToLower(keep) == "no" {
					c.Scheduling.ScheduleRanges = []config.ScheduleRange{promptScheduleWindow(scanner)}
				}
			} else {
				c.Scheduling.ScheduleRanges = []config.ScheduleRange{promptScheduleWindow(scanner)}
			}
		default:
			c.Scheduling.Mode = "ALWAYS"
		}
		// Windows survive a mode change in the file but have no effect outside
		// SCHEDULED mode — say so, or the config quietly contradicts the behavior
		// (the TB-28 trap, and the `config set scheduling.mode` family, TQ-14).
		if c.Scheduling.Mode != "SCHEDULED" && len(c.Scheduling.ScheduleRanges) > 0 {
			fmt.Printf("Note: your %d saved schedule window(s) will be ignored in %s mode.\n",
				len(c.Scheduling.ScheduleRanges), c.Scheduling.Mode)
		}

		// Step 4: Leaf Preferences
		fmt.Println("\n=== Step 4: Leaf Preferences ===")
		fmt.Println("Modes: all, specific, blocklist")
		leafMode := promptString(scanner, "Leaf mode [all]", "all")
		switch strings.ToLower(leafMode) {
		case "specific":
			c.Leafs.Mode = "SPECIFIC"
			ids := promptString(scanner, "Leaf IDs (comma-separated)", "")
			if ids != "" {
				c.Leafs.LeafIDs = splitAndTrim(ids)
			}
		case "blocklist":
			c.Leafs.Mode = "BLOCKLIST"
			ids := promptString(scanner, "Blocked leaf IDs (comma-separated)", "")
			if ids != "" {
				c.Leafs.BlockedIDs = splitAndTrim(ids)
			}
		default:
			c.Leafs.Mode = "ALL"
		}

		// Step 5: Runtimes
		fmt.Println("\n=== Step 5: Runtimes ===")
		// Detection only. Which runtimes actually run is decided per head — the
		// trust consent at `attach` (or `heads trust`) plus a live engine probe at
		// daemon start (TB-25) — so there is no enable/disable choice to record
		// here; init just notes which detected engine to prefer when both exist.
		fmt.Print("Checking for container runtime... ")
		backend := detectContainerBackendFunc(rtdetect.BundledPodmanPath())
		if backend.Backend != rtdetect.BackendNone {
			fmt.Printf("found %s", backend.Backend)
			if backend.Version != "" {
				fmt.Printf(" %s", backend.Version)
			}
			fmt.Println()
			c.ContainerBackend = string(backend.Backend)
			fmt.Println("Container leafs can run here once you trust a head for CONTAINER (chosen at attach, or later with 'heads trust').")
		} else {
			fmt.Println("not found (container leafs will not run on this machine; WASM always runs)")
		}

		// Step 6: Thermal Protection
		fmt.Println("\n=== Step 6: Thermal Protection ===")
		enableThermal := promptString(scanner, "Enable thermal protection? [Y/n]", "y")
		if strings.ToLower(enableThermal) == "n" || strings.ToLower(enableThermal) == "no" {
			c.Thermal.Enabled = false
		} else {
			c.Thermal.Enabled = true
			fmt.Println("Using default thresholds (CPU: 85/75°C, GPU: 80/70°C)")
		}

		// Step 7: Server
		fmt.Println("\n=== Step 7: Server ===")
		host := promptString(scanner, "Server host (optional, press Enter to skip)", "")
		if host != "" {
			sc, err := serverConfigFromAddress(host)
			if err != nil {
				return err
			}
			// The same consent `attach` gives, on the same prompt (TB-7). Container
			// availability is the machine's real capability, exactly as at attach —
			// trust is a per-head ceiling, and what the daemon actually advertises is
			// that ceiling intersected with the runtimes it detects at start.
			applyInitTrust(&sc, promptRuntimeTrust(scanner, sc.Name, containerBackendAvailable()))
			c.Servers = []config.ServerConfig{sc}
		}
	}

	// Validate before saving.
	if err := c.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Save config.
	if err := c.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	// Print summary.
	fmt.Println("\n=== Configuration Summary ===")
	out, _ := yaml.Marshal(c)
	fmt.Print(string(out))
	fmt.Printf("\nConfig saved to %s\n", cfgPath)

	return nil
}

// promptScheduleWindow asks for a daily time window (the --from/--to/--days trio
// the `schedule` commands use) and re-asks until it parses, so init cannot write a
// schedule the daemon will refuse. On a closed stdin every prompt takes its
// default, which parses, so a non-interactive stream terminates on the first pass.
func promptScheduleWindow(scanner *bufio.Scanner) config.ScheduleRange {
	fmt.Println("Run only inside a daily time window. Hours are whole hours and the window may")
	fmt.Println("wrap past midnight, so \"dusk till dawn\" is simply 20:00 to 06:00.")
	for {
		from := promptString(scanner, "Start of window, e.g. 20:00 [20:00]", "20:00")
		to := promptString(scanner, "End of window, e.g. 06:00 [06:00]", "06:00")
		days := promptString(scanner, "Days it applies, e.g. mon-fri or sat,sun [mon-sun]", "mon-sun")
		r, err := buildScheduleRange(from, to, days)
		if err != nil {
			fmt.Printf("  %v\n  Let's try that again.\n", err)
			continue
		}
		fmt.Printf("Schedule: %s\n", describeRange(r))
		fmt.Println("You can change it later with `lettuce-volunteer schedule set` / `schedule add`.")
		return r
	}
}

// applyInitTrust records the per-head runtime trust for a head attached during
// init and prints what was decided.
//
// init used to attach a head without ever putting the "a head is a trust domain"
// consent that `attach` gives, writing an entry with an empty trusted_runtimes —
// a silent, un-asked "WASM only". Against a head whose leafs are all
// CONTAINER/NATIVE that volunteer then fetched nothing at all, with no prompt, no
// warning and nothing pointing at `heads trust` (TB-7). The list is always
// non-nil: an empty decision must persist as the explicit choice it is, never as
// an absent key the legacy-trust migration would re-seed (PB-28).
func applyInitTrust(sc *config.ServerConfig, trusted []string) {
	if trusted == nil {
		trusted = []string{}
	}
	sc.TrustedRuntimes = trusted
	fmt.Printf("%s may run: %s\n", sc.Name, trustSummary(trusted))
	if len(trusted) == 0 {
		fmt.Printf("Note: WASM only. If this head hosts container or native leafs you will receive no\n"+
			"work from it until you say so: lettuce-volunteer heads trust %s container\n", sc.Name)
	}
}

func isYes(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "y" || s == "yes"
}

func promptString(scanner *bufio.Scanner, prompt string, defaultVal string) string {
	fmt.Printf("%s: ", prompt)
	if !scanner.Scan() {
		return defaultVal
	}
	val := strings.TrimSpace(scanner.Text())
	if val == "" {
		return defaultVal
	}
	return val
}

func promptInt(scanner *bufio.Scanner, prompt string, defaultVal int) int {
	fmt.Printf("%s: ", prompt)
	if !scanner.Scan() {
		return defaultVal
	}
	val := strings.TrimSpace(scanner.Text())
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		fmt.Printf("Invalid number, using default: %d\n", defaultVal)
		return defaultVal
	}
	return n
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

var detectContainerBackendFunc = rtdetect.DetectContainerBackend

// serverConfigFromAddress builds the config entry for a head from whatever the
// volunteer typed — a bare host, host:port, or an http(s):// URL (TB-51). The
// entry's name is the host; gRPC and HTTP targets come from the same parsed
// address, so a URL never reaches gRPC and "https" is never a head's name.
func serverConfigFromAddress(input string) (config.ServerConfig, error) {
	addr, err := config.ParseHeadAddress(input)
	if err != nil {
		return config.ServerConfig{}, err
	}
	return config.ServerConfig{
		GRPCAddress: addr.GRPCAddress(),
		HTTPAddress: addr.HTTPAddress(),
		Name:        addr.Host,
		Insecure:    addr.Insecure,
	}, nil
}
