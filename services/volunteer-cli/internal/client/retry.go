package client

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// RetryConfig controls connection retry behavior.
type RetryConfig struct {
	InitialBackoff time.Duration // default: 1s
	MaxBackoff     time.Duration // default: 30s
	Multiplier     float64       // default: 2.0
	MaxRetries     int           // default: 0 (infinite)
}

// DefaultRetryConfig returns retry config with sensible defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2.0,
		MaxRetries:     0,
	}
}

// ConnectWithRetry attempts to connect to the server with exponential backoff.
// It creates a single gRPC client (which uses lazy connections) and verifies
// connectivity by calling GetServerStatus on each attempt. gRPC handles
// reconnection internally, so we reuse the client across retries.
// Jitter of 0-25% is added to each backoff interval to prevent thundering herd.
func ConnectWithRetry(ctx context.Context, cfg ClientConfig, retryCfg RetryConfig, logger *slog.Logger) (*Client, error) {
	if retryCfg.InitialBackoff == 0 {
		retryCfg.InitialBackoff = 1 * time.Second
	}
	if retryCfg.MaxBackoff == 0 {
		retryCfg.MaxBackoff = 30 * time.Second
	}
	if retryCfg.Multiplier == 0 {
		retryCfg.Multiplier = 2.0
	}
	if cfg.ConnTimeout == 0 {
		cfg.ConnTimeout = 10 * time.Second
	}

	client, err := New(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("creating gRPC client: %w", err)
	}

	backoff := retryCfg.InitialBackoff

	for attempt := 1; ; attempt++ {
		connCtx, cancel := context.WithTimeout(ctx, cfg.ConnTimeout)
		_, err := client.GetServerStatus(connCtx)
		cancel()
		if err == nil {
			logger.Info("connected to server", "address", cfg.ServerURL, "attempt", attempt)
			return client, nil
		}

		if retryCfg.MaxRetries > 0 && attempt >= retryCfg.MaxRetries {
			client.Close()
			return nil, fmt.Errorf("max retries (%d) exceeded: %w", retryCfg.MaxRetries, err)
		}

		// Add jitter: random 0-25% of backoff.
		var jitter time.Duration
		if jitterMax := int64(backoff) / 4; jitterMax > 0 {
			jitter = time.Duration(rand.Int64N(jitterMax))
		}
		sleepDur := backoff + jitter

		logger.Info("connection failed, retrying",
			"attempt", attempt,
			"error", err,
			"backoff", sleepDur,
		)

		select {
		case <-ctx.Done():
			client.Close()
			return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		case <-time.After(sleepDur):
		}

		backoff = time.Duration(float64(backoff) * retryCfg.Multiplier)
		if backoff > retryCfg.MaxBackoff {
			backoff = retryCfg.MaxBackoff
		}
	}
}
