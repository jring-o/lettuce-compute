package cli

import (
	"fmt"
	"log/slog"

	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/project"
	"github.com/spf13/cobra"
)

func newAttachCmd() *cobra.Command {
	var (
		server     string
		grpcPort   int
		httpPort   int
		leafID     string
		insecure   bool
		caCertPath string
	)

	cmd := &cobra.Command{
		Use:   "attach [leaf-id]",
		Short: "Add a leaf or server to preferences",
		Long: `Attach a specific leaf by ID or connect to a self-hosted server.

Examples:
  lettuce-volunteer attach <leaf-id>
  lettuce-volunteer attach --server my-server.example.com
  lettuce-volunteer attach --server my-server.example.com --grpc-port 9090 --http-port 8080
  lettuce-volunteer attach --server my-server.example.com --leaf <id>
  lettuce-volunteer attach --server localhost --insecure`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, closeLogger := newLogger(cfg)
			defer closeLogger()
			mgr := project.NewManager(cfg, cfgPath, logger)

			// Case 1: attach --server <host>
			if server != "" {
				return attachServer(cmd, mgr, server, grpcPort, httpPort, leafID, insecure, caCertPath, logger)
			}

			// Case 2: attach <leaf-id> (on first configured server)
			if len(args) == 1 {
				return attachLeafByID(mgr, args[0])
			}

			return fmt.Errorf("specify a leaf ID or use --server <host>")
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "server hostname or IP")
	cmd.Flags().IntVar(&grpcPort, "grpc-port", 443, "gRPC port (default 443)")
	cmd.Flags().IntVar(&httpPort, "http-port", 443, "HTTPS port (default 443)")
	cmd.Flags().StringVar(&leafID, "leaf", "", "leaf ID on the server")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "disable TLS (development only)")
	cmd.Flags().StringVar(&caCertPath, "ca-cert", "", "path to CA certificate for server verification")

	return cmd
}

func attachServer(cmd *cobra.Command, mgr *project.Manager, host string, grpcPort, httpPort int, leafID string, insecure bool, caCertPath string, logger *slog.Logger) error {
	if grpcPort <= 0 {
		grpcPort = 443
	}
	if httpPort <= 0 {
		httpPort = 443
	}

	grpcAddr := fmt.Sprintf("%s:%d", host, grpcPort)
	httpScheme := "https"
	if insecure {
		httpScheme = "http"
	}
	var httpAddr string
	if (httpScheme == "https" && httpPort == 443) || (httpScheme == "http" && httpPort == 80) {
		httpAddr = fmt.Sprintf("%s://%s", httpScheme, host)
	} else {
		httpAddr = fmt.Sprintf("%s://%s:%d", httpScheme, host, httpPort)
	}

	// Validate by checking server status.
	grpcClient, err := client.ConnectWithRetry(cmd.Context(), client.ClientConfig{
		ServerURL:   grpcAddr,
		Insecure:    insecure,
		TLSCertFile: caCertPath,
	}, client.RetryConfig{
		MaxRetries: 3,
	}, logger)
	if err != nil {
		return fmt.Errorf("cannot reach server at %s: %w", grpcAddr, err)
	}
	defer grpcClient.Close()

	statusResp, err := grpcClient.GetServerStatus(cmd.Context())
	if err != nil {
		return fmt.Errorf("server at %s is not responding: %w", grpcAddr, err)
	}
	logger.Info("server validated", "version", statusResp.Version, "status", statusResp.Status)

	if leafID != "" {
		if err := mgr.AttachLeaf(leafID, grpcAddr, httpAddr, host); err != nil {
			return err
		}
		fmt.Printf("Attached to leaf %s on %s. gRPC: %s, HTTP: %s.\n", leafID, host, grpcAddr, httpAddr)
	} else {
		if err := mgr.AttachServerWithTLS(host, grpcPort, httpPort, insecure, caCertPath); err != nil {
			return err
		}
		fmt.Printf("Attached to %s. gRPC: %s, HTTP: %s. The daemon will include this in its work pool on next startup.\n", host, grpcAddr, httpAddr)
	}
	return nil
}

func attachLeafByID(mgr *project.Manager, leafID string) error {
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("no servers configured. Use `lettuce-volunteer attach --server <host>` first")
	}

	srv := cfg.Servers[0]
	if err := mgr.AttachLeaf(leafID, srv.GRPCAddress, srv.HTTPAddress, srv.Name); err != nil {
		return err
	}
	fmt.Printf("Attached leaf %s on server %s.\n", leafID, srv.GRPCAddress)
	return nil
}
