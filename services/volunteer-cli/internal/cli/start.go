package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/client"
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

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseSlogLevel(cfg.LogLevel),
	}))

	// Connect to all configured servers.
	var connections []*daemon.ServerConnection
	var stateServers []daemon.ServerState

	for _, srv := range cfg.Servers {
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

		volID, isNew, err := client.Register(cmd.Context(), grpcClient, pub, cfg, cfgPath)
		if err != nil {
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
			logger.Info("registered as new volunteer", "server", name, "volunteer_id", volID)
		} else {
			logger.Info("re-registered with server", "server", name, "volunteer_id", volID)
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

	// Ensure WASM is in AvailableRuntimes (handles existing configs that predate WASM support).
	if !containsRuntime(cfg.AvailableRuntimes, "WASM") {
		cfg.AvailableRuntimes = append(cfg.AvailableRuntimes, "WASM")
	}

	// Create runtime registry.
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
	if machineManager != nil && machineSetupOK {
		d.SetMachineStartedByDaemon(true)
	}

	// Stop Podman machine on daemon shutdown if we started it.
	defer func() {
		if machineManager != nil && d.MachineStartedByDaemon() {
			logger.Info("stopping podman machine on daemon shutdown")
			if err := machineManager.Stop(); err != nil {
				logger.Warn("failed to stop podman machine", "error", err)
			}
		}
	}()

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
