package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a gRPC connection to the Lettuce infrastructure server.
type Client struct {
	conn           *grpc.ClientConn
	svc            lettucev1.VolunteerServiceClient
	logger         *slog.Logger
	requestTimeout time.Duration
}

// ClientConfig holds connection configuration.
type ClientConfig struct {
	ServerURL      string        // e.g., "example.com:443"
	TLSCertFile    string        // optional: path to CA cert for server verification
	TLSClientCert  string        // optional: path to client cert for mTLS
	TLSClientKey   string        // optional: path to client key for mTLS
	Insecure       bool          // skip TLS (for development only)
	ConnTimeout    time.Duration // connection timeout, default 10s
	RequestTimeout time.Duration // per-RPC timeout, default 30s
	// Identity is the volunteer's Ed25519 keypair used to sign authenticated RPCs.
	// When nil, the client only signs nothing and can be used for the public
	// discovery RPCs (GetServerStatus, GetHeadInfo) — e.g. during `attach`.
	Identity *Identity
}

// New creates a gRPC client. The connection is lazy — actual connectivity
// is verified on the first RPC call.
func New(cfg ClientConfig, logger *slog.Logger) (*Client, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 30 * time.Second
	}

	var creds credentials.TransportCredentials
	switch {
	case cfg.Insecure:
		creds = insecure.NewCredentials()
	default:
		tlsCfg := &tls.Config{}

		// Custom CA certificate for server verification.
		if cfg.TLSCertFile != "" {
			certPEM, err := os.ReadFile(cfg.TLSCertFile)
			if err != nil {
				return nil, fmt.Errorf("reading CA cert file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(certPEM) {
				return nil, fmt.Errorf("failed to parse CA cert file: %s", cfg.TLSCertFile)
			}
			tlsCfg.RootCAs = pool
		}

		// Client certificate for mTLS.
		if cfg.TLSClientCert != "" && cfg.TLSClientKey != "" {
			clientCert, err := tls.LoadX509KeyPair(cfg.TLSClientCert, cfg.TLSClientKey)
			if err != nil {
				return nil, fmt.Errorf("loading client certificate: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{clientCert}
		} else if cfg.TLSClientCert != "" || cfg.TLSClientKey != "" {
			return nil, fmt.Errorf("both client cert and key must be provided for mTLS")
		}

		creds = credentials.NewTLS(tlsCfg)
	}

	conn, err := grpc.NewClient(cfg.ServerURL,
		grpc.WithTransportCredentials(creds),
		grpc.WithChainUnaryInterceptor(signingClientInterceptor(cfg.Identity)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating gRPC client: %w", err)
	}

	return &Client{
		conn:           conn,
		svc:            lettucev1.NewVolunteerServiceClient(conn),
		logger:         logger,
		requestTimeout: cfg.RequestTimeout,
	}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// rpcCtx returns ctx with the default request timeout if no deadline is set.
func (c *Client) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

// GetServerStatus calls the GetServerStatus RPC.
func (c *Client) GetServerStatus(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
}

// RegisterVolunteer calls the RegisterVolunteer RPC.
func (c *Client) RegisterVolunteer(ctx context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.RegisterVolunteer(ctx, req)
}

// RequestWorkUnit calls the RequestWorkUnit RPC.
func (c *Client) RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.RequestWorkUnit(ctx, req)
}

// SubmitResult calls the SubmitResult RPC.
func (c *Client) SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.SubmitResult(ctx, req)
}

// StartWork calls the StartWork RPC (run-start: QUEUED -> ASSIGNED for a buffered
// reserved unit the slot has begun executing). It replaces the removed per-task
// Heartbeat RPC; liveness is now deadline-based.
func (c *Client) StartWork(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.StartWork(ctx, req)
}

// SaveCheckpoint calls the SaveCheckpoint RPC.
func (c *Client) SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.SaveCheckpoint(ctx, req)
}

// GetCheckpoint calls the GetCheckpoint RPC.
func (c *Client) GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.GetCheckpoint(ctx, req)
}

// GetHeadInfo calls the GetHeadInfo RPC.
func (c *Client) GetHeadInfo(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.GetHeadInfo(ctx, req)
}

// AbandonWorkUnit calls the AbandonWorkUnit RPC.
func (c *Client) AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.svc.AbandonWorkUnit(ctx, req)
}
