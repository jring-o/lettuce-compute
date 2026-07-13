package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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
	cmd.Flags().String("server", "", "Server host to connect to")
	cmd.Flags().String("enabled-leafs", "", "Comma-separated leaf slugs to enable (sets SPECIFIC mode)")
	return cmd
}

func runInit(cmd *cobra.Command, args []string) error {
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

	keyFile := filepath.Join(dataDir, "identity.key")
	pubKeyFile := filepath.Join(dataDir, "identity.pub")
	c.KeyFile = keyFile
	c.PubKeyFile = pubKeyFile

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

		// Runtimes: WASM is the always-available sandboxed default; add CONTAINER when a
		// Docker/Podman backend is present. NATIVE is never enabled here — it is a per-head
		// trust opt-in chosen at `attach` (or later via `heads trust`).
		c.AvailableRuntimes = []string{"WASM"}
		backend := detectContainerBackendFunc(rtdetect.BundledPodmanPath())
		if backend.Backend != rtdetect.BackendNone {
			c.AvailableRuntimes = append(c.AvailableRuntimes, "CONTAINER")
			c.ContainerBackend = string(backend.Backend)
		}

		// Server.
		if host, _ := cmd.Flags().GetString("server"); host != "" {
			name := host
			sc := config.ServerConfig{}
			if strings.Contains(host, ":") {
				sc.GRPCAddress = host
				name = host[:strings.LastIndex(host, ":")]
				sc.HTTPAddress = fmt.Sprintf("https://%s", name)
			} else {
				sc.GRPCAddress = host + ":443"
				sc.HTTPAddress = fmt.Sprintf("https://%s", host)
			}
			sc.Name = name
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
		fmt.Println("\n=== GPU Detection ===")
		gpus := rtdetect.DetectGPUs()
		if len(gpus) > 0 {
			fmt.Printf("Detected %d GPU(s):\n", len(gpus))
			for i, g := range gpus {
				if g.VRAMMB > 0 {
					fmt.Printf("  [%d] %s (%s, %d MB VRAM)\n", i, g.Model, g.Vendor, g.VRAMMB)
				} else {
					fmt.Printf("  [%d] %s (%s)\n", i, g.Model, g.Vendor)
				}
			}
			allowGPU := promptString(scanner, "Allow GPU tasks? [Y/n]", "y")
			if strings.ToLower(allowGPU) == "n" || strings.ToLower(allowGPU) == "no" {
				c.ResourceLimits.MaxGPUVRAMPct = 0
			} else {
				c.ResourceLimits.MaxGPUVRAMPct = promptInt(scanner, "Max VRAM percentage [50]", 50)
			}
		} else {
			fmt.Println("No GPUs detected.")
		}

		// Step 3: Scheduling
		fmt.Println("\n=== Step 3: Scheduling ===")
		fmt.Println("Modes: always, idle, scheduled")
		mode := promptString(scanner, "Scheduling mode [always]", "always")
		switch strings.ToLower(mode) {
		case "idle":
			c.Scheduling.Mode = "WHEN_IDLE"
			c.Scheduling.IdleThresholdMins = promptInt(scanner, "Idle threshold minutes [5]", 5)
		case "scheduled":
			c.Scheduling.Mode = "SCHEDULED"
			c.Scheduling.CronExpression = promptString(scanner, "Cron expression", "")
		default:
			c.Scheduling.Mode = "ALWAYS"
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
		// WASM is the always-available sandboxed default; CONTAINER is offered below when a
		// backend is present. NATIVE is a per-head trust opt-in chosen at `attach`, never here.
		c.AvailableRuntimes = []string{"WASM"}
		fmt.Print("Checking for container runtime... ")
		backend := detectContainerBackendFunc(rtdetect.BundledPodmanPath())
		if backend.Backend != rtdetect.BackendNone {
			fmt.Printf("found %s", backend.Backend)
			if backend.Version != "" {
				fmt.Printf(" %s", backend.Version)
			}
			fmt.Println()
			enableContainer := promptString(scanner, "Enable container tasks? [Y/n]", "y")
			if strings.ToLower(enableContainer) != "n" && strings.ToLower(enableContainer) != "no" {
				c.AvailableRuntimes = append(c.AvailableRuntimes, "CONTAINER")
				c.ContainerBackend = string(backend.Backend)
			}
		} else {
			fmt.Println("not found (container tasks will not be available)")
		}
		fmt.Printf("Available runtimes: %s\n", strings.Join(c.AvailableRuntimes, ", "))

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
			name := host
			sc := config.ServerConfig{}
			if strings.Contains(host, ":") {
				sc.GRPCAddress = host
				name = host[:strings.LastIndex(host, ":")]
				sc.HTTPAddress = fmt.Sprintf("https://%s", name)
			} else {
				sc.GRPCAddress = host + ":443"
				sc.HTTPAddress = fmt.Sprintf("https://%s", host)
			}
			sc.Name = name
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
