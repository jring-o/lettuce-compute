package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	stdruntime "runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the volunteer daemon",
		RunE:  runStart,
	}
}

// dedupeServersByAddress collapses configured servers to one entry per gRPC
// address so the daemon opens exactly one connection per head. When several
// entries share an address it keeps a single entry, preferring a head-level entry
// (LeafID == "") over a leaf-scoped one so the surviving connection can serve all
// of the head's leafs rather than being pinned to one leaf. Collapsed duplicates
// are logged so the operator can see it happened.
func dedupeServersByAddress(servers []config.ServerConfig, logger *slog.Logger) []config.ServerConfig {
	indexByAddr := make(map[string]int, len(servers))
	result := make([]config.ServerConfig, 0, len(servers))
	for _, srv := range servers {
		name := srv.Name
		if name == "" {
			name = srv.GRPCAddress
		}
		if idx, ok := indexByAddr[srv.GRPCAddress]; ok {
			// Prefer a head-level (no LeafID) entry so the single connection is not
			// restricted to one leaf.
			if result[idx].LeafID != "" && srv.LeafID == "" {
				result[idx] = srv
			}
			logger.Warn("collapsing duplicate server entry; one connection per head",
				"address", srv.GRPCAddress, "server", name)
			continue
		}
		indexByAddr[srv.GRPCAddress] = len(result)
		result = append(result, srv)
	}
	return result
}

func runStart(cmd *cobra.Command, args []string) error {
	// Check if daemon is already running.
	pid, err := daemon.ReadPID(cfg.DataDir)
	if err == nil && daemon.IsProcessRunning(pid) {
		return fmt.Errorf("daemon is already running (PID: %d). Use 'lettuce-volunteer stop' to stop it", pid)
	}

	// Load identity keypair.
	pub, priv, err := identity.LoadKeyPair(cfg.KeyFile, cfg.PubKeyFile)
	if err != nil {
		return fmt.Errorf("loading identity: %w (run 'lettuce-volunteer init' first)", err)
	}

	// Verify at least one server is configured.
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("no servers configured. Run `lettuce-volunteer attach --server <host>` first")
	}

	// File-backed logger for the whole daemon lifetime: JSON to both stderr and
	// a size-rotated file under <DataDir>/logs/. Deferred first so it closes the
	// log file last, after all other shutdown logging has flushed.
	logger, closeLogger := newLogger(cfg)
	defer closeLogger()

	// Run-start banner: the first line in every daemon log, so a pasted log is
	// self-identifying. version is the single most diagnostic field given the
	// head<->volunteer protocol-version coupling (an out-of-date build is
	// rejected fleet-wide with "volunteer too old for this head"); os/arch is
	// load-bearing for the Hackintosh/OCLP population whose runtime quirks track
	// the patched platform they report.
	logger.Info("volunteer starting",
		"version", version,
		"os", stdruntime.GOOS,
		"arch", stdruntime.GOARCH,
		"data_dir", cfg.DataDir,
		"log_level", cfg.LogLevel,
	)

	logger.Info("logging to file", "path", cfg.LogFilePath(), "enabled", cfg.LogToFile)

	// Record which identity + config this daemon is running under: a SHORT public-key
	// fingerprint (first 8 hex chars of the Ed25519 PUBLIC key — never the private
	// key), the config path, and the data dir. Makes "which volunteer is this log
	// from" answerable from the log alone.
	pubFP := "unknown"
	if len(pub) >= 4 {
		pubFP = fmt.Sprintf("%x", pub[:4])
	}
	logger.Info("identity loaded",
		"pubkey_fp", pubFP,
		"config_path", cfgPath,
		"data_dir", cfg.DataDir,
	)

	// Ensure WASM is in AvailableRuntimes (handles existing configs that predate
	// WASM support); the WASM runtime is always available.
	if !containsRuntime(cfg.AvailableRuntimes, "WASM") {
		cfg.AvailableRuntimes = append(cfg.AvailableRuntimes, "WASM")
	}

	// Build the runtime registry up front — before registering with any head — so
	// we advertise the runtimes this box can ACTUALLY run, not whatever config
	// lists. A machine that lists CONTAINER but has no working Docker/Podman then
	// never gets container work it can only abandon (which would churn units to
	// FAILED on the head). native/wasm are always registered; container only when
	// a backend is detected and initializes.
	registry, machineManager, machineSetupOK := buildRuntimeRegistry(cfg, logger)
	advertised := advertisedRuntimes(registry)
	logger.Info("runtimes available", "advertised", advertised)

	// If we started a Podman machine, stop it on exit. Registered here (right
	// after setup, before the connect loop) so it also fires on the early
	// "could not connect to any server" return below — otherwise a failed
	// startup would leak the running VM.
	if machineManager != nil && machineSetupOK {
		defer func() {
			logger.Info("stopping podman machine on daemon shutdown")
			if err := machineManager.Stop(); err != nil {
				logger.Warn("failed to stop podman machine", "error", err)
			}
		}()
	}

	// Connect to all configured servers — one gRPC connection per head address.
	// Multiple cfg.Servers entries can reference the same head (a plain
	// `attach --server` plus per-leaf attaches, or a wizard entry plus an attach),
	// and connecting once per entry would open DUPLICATE connections to the same
	// head: double the RPC rate (worsening the head's rate-limit shedding) and a
	// confusing duplicate row in `status` / `leafs list`. Collapse to one entry per
	// address first. (Per-entry leaf-preference merging across collapsed entries is
	// TODO #26.)
	var connections []*daemon.ServerConnection
	var stateServers []daemon.ServerState

	for _, srv := range dedupeServersByAddress(cfg.Servers, logger) {
		name := srv.Name
		if name == "" {
			name = srv.GRPCAddress
		}

		grpcClient, err := client.ConnectWithRetry(cmd.Context(), client.ClientConfig{
			ServerURL:     srv.GRPCAddress,
			Insecure:      srv.Insecure,
			TLSCertFile:   srv.CACertPath,
			TLSClientCert: srv.CertPath,
			TLSClientKey:  srv.KeyPath,
			Identity:      &client.Identity{PublicKey: pub, PrivateKey: priv},
		}, client.RetryConfig{
			MaxRetries: 3,
		}, logger)
		if err != nil {
			// Log warning but continue — don't fail if one server is down.
			logger.Warn("failed to connect to server, skipping",
				"server", name, "address", srv.GRPCAddress, "error", err)
			stateServers = append(stateServers, daemon.ServerState{
				Name:        name,
				GRPCAddress: srv.GRPCAddress,
				Connected:   false,
			})
			continue
		}

		// Read the head's build version over the unauthenticated GetServerStatus RPC.
		// It is stamped on the registration log below and drives the version-mismatch
		// warning: head and volunteers are protocol-version coupled (an out-of-date
		// build is rejected fleet-wide with "volunteer too old for this head"), so a
		// mismatch is the single most useful thing to spot at startup. A status error
		// must never block startup — fall back to an unknown head version.
		var headVersion string
		if statusResp, statusErr := grpcClient.GetServerStatus(cmd.Context()); statusErr != nil {
			logger.Debug("could not read head version (GetServerStatus failed)",
				"server", name, "error", statusErr)
		} else {
			headVersion = statusResp.Version
		}

		volID, isNew, err := client.Register(cmd.Context(), grpcClient, pub, cfg, cfgPath, advertised...)
		if err != nil {
			if client.IsVolunteerTooOldError(err) {
				logger.Warn("this volunteer build is too old for the head; run 'lettuce-volunteer update'",
					"server", name, "error", err)
			}
			logger.Warn("failed to register with server, skipping",
				"server", name, "error", err)
			grpcClient.Close()
			stateServers = append(stateServers, daemon.ServerState{
				Name:        name,
				GRPCAddress: srv.GRPCAddress,
				Connected:   false,
			})
			continue
		}

		connections = append(connections, &daemon.ServerConnection{
			Config:      srv,
			Client:      grpcClient,
			VolunteerID: volID,
			Name:        name,
			Available:   true,
		})

		stateServers = append(stateServers, daemon.ServerState{
			Name:        name,
			GRPCAddress: srv.GRPCAddress,
			VolunteerID: volID,
			Connected:   true,
		})

		if isNew {
			logger.Info("registered as new volunteer", "server", name, "volunteer_id", volID, "head_version", headVersion)
		} else {
			logger.Info("re-registered with server", "server", name, "volunteer_id", volID, "head_version", headVersion)
		}

		// Protocol-version coupling: warn loudly when the head and this volunteer are
		// on different non-dev builds, since that is exactly the condition that gets a
		// volunteer rejected ("too old for this head"). "dev" builds are local and
		// never coupled, so they are excluded to avoid crying wolf during development.
		// Compare NORMALIZED versions: the volunteer release stamps a bare "0.5.2"
		// (release.yml strips the leading v) while the head, built from
		// `git describe --tags`, stamps "v0.5.2" — without normalizing, a correctly
		// matched pair would false-positive purely over the "v" prefix.
		if headVersion != "" && version != "" && headVersion != "dev" && version != "dev" &&
			normalizeVersion(headVersion) != normalizeVersion(version) {
			logger.Warn("volunteer/head version mismatch; head and volunteers must run matching builds (protocol-version coupling)",
				"server", name, "head_version", headVersion, "volunteer_version", version)
		}
	}

	if len(connections) == 0 {
		return fmt.Errorf("could not connect to any configured server")
	}

	// Print startup summary.
	fmt.Printf("Volunteer daemon started. Connected to %d server(s):\n", len(connections))
	for _, conn := range connections {
		fmt.Printf("  - %s (volunteer ID: %s)\n", conn.Name, conn.VolunteerID)
	}
	if cfg.LogToFile {
		fmt.Printf("Logs: %s (rotating; also on stderr)\n", cfg.LogFilePath())
	}

	// Persist daemon state so the status command can read it.
	if err := daemon.WriteDaemonState(cfg.DataDir, &daemon.DaemonState{
		Servers: stateServers,
	}); err != nil {
		logger.Warn("failed to write daemon state", "error", err)
	}

	// Close all gRPC connections and remove state on exit.
	defer func() {
		for _, conn := range connections {
			conn.Client.Close()
		}
		daemon.RemoveDaemonState(cfg.DataDir)
	}()

	// Create daemon with runtime registry.
	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config:          cfg,
		PubKey:          pub,
		PrivKey:         priv,
		Servers:         connections,
		RuntimeRegistry: registry,
		MachineManager:  machineManager,
		Logger:          logger,
	})

	// Start management API server.
	mgmtServer := management.NewServer(cfg.DataDir, logger)
	bridge := management.NewDaemonBridge(d, cfgPath)
	if err := mgmtServer.Start(bridge); err != nil {
		logger.Warn("failed to start management API", "error", err)
	} else {
		fmt.Printf("Management API listening on http://127.0.0.1:%d\n", mgmtServer.Port())
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			mgmtServer.Shutdown(shutdownCtx)
		}()
	}

	// Set up signal handling.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down gracefully", "signal", sig)
		fmt.Fprintf(os.Stderr, "\nReceived %s. Finishing current work unit before exiting...\n", sig)
		cancel()
	}()

	// Run daemon loop — blocks until shutdown.
	return d.Run(ctx)
}

// buildRuntimeRegistry constructs the runtime registry. native and wasm are
// always registered; the container runtime is added only when CONTAINER is
// configured AND a working backend (Docker/Podman, setting up a Podman machine
// if needed) is detected and initializes. Returning the machine manager and the
// setup flag lets the caller stop a daemon-started Podman machine on shutdown.
func buildRuntimeRegistry(cfg *config.Config, logger *slog.Logger) (*daemon.RuntimeRegistry, *runtime.PodmanMachineManager, bool) {
	registry := daemon.NewRuntimeRegistry()

	// Always register native runtime.
	nativeRuntime := runtime.NewNativeRuntime(cfg.DataDir, logger)
	registry.Register(nativeRuntime)

	// Always register WASM runtime (wazero is embedded, no external dependencies).
	wasmRuntime := runtime.NewWasmRuntime(cfg.DataDir, logger)
	registry.Register(wasmRuntime)

	// Register container runtime if configured.
	var machineManager *runtime.PodmanMachineManager
	machineSetupOK := false
	if containsRuntime(cfg.AvailableRuntimes, "CONTAINER") {
		// Honor the operator's configured backend preference (container_backend).
		// When set to "docker", Docker is chosen if present so large images use
		// host storage instead of a Podman-machine VM. Empty = auto (Podman first).
		preferred := runtime.ContainerBackend(cfg.ContainerBackend)
		backend := runtime.DetectContainerBackendPreferred(runtime.BundledPodmanPath(), preferred)
		if backend.Backend == runtime.BackendPodman {
			machineManager = runtime.NewPodmanMachineManager(backend.BinaryPath, logger)
			if machineManager.NeedsMachine() {
				logger.Info("setting up Podman machine for container runtime")
				cpus := cfg.ResourceLimits.MaxCPUCores
				memMB := cfg.ResourceLimits.MaxMemoryMB
				diskGB := cfg.ResourceLimits.MaxDiskGB
				if cpus <= 0 {
					cpus = 2
				}
				if memMB <= 0 {
					memMB = 4096
				}
				if diskGB <= 0 {
					diskGB = 20
				}
				if err := machineManager.Setup(cpus, memMB, diskGB); err != nil {
					logger.Warn("podman machine setup failed, container runtime may be unavailable", "error", err)
				} else if err := machineManager.WaitForReady(60 * time.Second); err != nil {
					logger.Warn("podman machine not ready after setup", "error", err)
				} else {
					machineSetupOK = true
				}
				// Re-detect backend after machine setup to get updated socket path.
				backend = runtime.DetectContainerBackendPreferred(runtime.BundledPodmanPath(), preferred)
			}
		}
		if backend.Backend != runtime.BackendNone {
			cr, err := runtime.NewContainerRuntimeForBackend(cfg.DataDir, logger, backend)
			if err != nil {
				logger.Warn("container runtime unavailable", "error", err)
			} else {
				cr.SetMaxCPUCores(cfg.ResourceLimits.MaxCPUCores)
				cr.SetMaxGPUVRAMPct(cfg.ResourceLimits.MaxGPUVRAMPct)
				gpus := runtime.DetectGPUs()
				if len(gpus) > 0 {
					cr.SetGPUs(gpus)
				}
				if backend.Backend == runtime.BackendPodman {
					runtime.EnsurePodmanGPUReady(logger)
				}
				registry.Register(cr)
			}
		} else {
			logger.Warn("container runtime configured but no backend available")
		}
	}

	return registry, machineManager, machineSetupOK
}

// advertisedRuntimes returns the UPPERCASE runtime enum names the volunteer can
// actually run, derived from what's actually registered (registry Name()s are
// lowercase: native/wasm/container). This is what we send the head at
// registration instead of cfg.AvailableRuntimes, so the advertised capabilities
// reflect reality and a backend-less box never gets container work.
func advertisedRuntimes(registry *daemon.RuntimeRegistry) []string {
	names := registry.AvailableRuntimes()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.ToUpper(n))
	}
	sort.Strings(out)
	return out
}

func containsRuntime(runtimes []string, name string) bool {
	for _, r := range runtimes {
		if r == name {
			return true
		}
	}
	return false
}

func parseSlogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// normalizeVersion strips surrounding whitespace and a single leading "v" so the
// volunteer's release stamp ("0.5.2", v-less per release.yml) compares equal to the
// head's stamp ("v0.5.2" when built from `git describe --tags`). Used only for the
// version-mismatch check, not for display.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
