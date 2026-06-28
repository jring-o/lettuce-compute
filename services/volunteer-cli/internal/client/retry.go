package client

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// connectRateLimitBackoff is the floor backoff applied when a connect probe is
// rejected with codes.ResourceExhausted (the head's gRPC rate limiter). It is
// sized to outlast the head's per-IP rate-limit window (~60s fixed / continuous
// token refill) so a retry actually gets through. CRUCIALLY, rate-limit retries do
// NOT count against RetryConfig.MaxRetries: a rate limit is the head saying "slow
// down", not an unreachable head, so a transiently-rate-limited single head must
// never make the daemon give up and exit (TODO #33). Genuine connection failures
// (Unavailable, DNS, refused) still consume MaxRetries and surface as a hard error.
// It is a var (not const) solely so tests can shrink it via a test seam.
var connectRateLimitBackoff = 30 * time.Second

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
	attempt := 0 // counts only genuine (non-rate-limit) connect failures

	for {
		connCtx, cancel := context.WithTimeout(ctx, cfg.ConnTimeout)
		_, err := client.GetServerStatus(connCtx)
		cancel()
		if err == nil {
			logger.Info("connected to server", "address", cfg.ServerURL, "attempt", attempt+1)
			return client, nil
		}

		// A rate-limit (ResourceExhausted) is the head shedding load, not an
		// unreachable head: back off a full window and keep retrying WITHOUT
		// consuming the MaxRetries budget, so a single rate-limited head can never
		// make the daemon exit (TODO #33). Any other error is a genuine connection
		// failure and counts toward MaxRetries with exponential backoff.
		rateLimited := status.Code(err) == codes.ResourceExhausted

		var sleepDur time.Duration
		if rateLimited {
			sleepDur = connectRateLimitBackoff + jitter(connectRateLimitBackoff)
			logger.Info("server rate-limited, backing off (will keep retrying)",
				"address", cfg.ServerURL,
				"backoff", sleepDur,
			)
		} else {
			attempt++
			if retryCfg.MaxRetries > 0 && attempt >= retryCfg.MaxRetries {
				client.Close()
				return nil, fmt.Errorf("max retries (%d) exceeded: %w", retryCfg.MaxRetries, err)
			}
			sleepDur = backoff + jitter(backoff)
			logger.Info("connection failed, retrying",
				"attempt", attempt,
				"error", err,
				"backoff", sleepDur,
			)
		}

		select {
		case <-ctx.Done():
			client.Close()
			return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		case <-time.After(sleepDur):
		}

		// Only grow the exponential backoff on genuine failures; the rate-limit
		// backoff is a fixed window-sized floor.
		if !rateLimited {
			backoff = time.Duration(float64(backoff) * retryCfg.Multiplier)
			if backoff > retryCfg.MaxBackoff {
				backoff = retryCfg.MaxBackoff
			}
		}
	}
}

// retryRPCOnRateLimit calls fn and, on codes.ResourceExhausted (the head's gRPC
// rate limiter shedding load), backs off a full rate-limit window and retries —
// WITHOUT any retry cap — until fn succeeds, returns a non-rate-limit error, or
// ctx is cancelled. A rate limit is the head saying "slow down", not a fatal
// condition, so an authenticated bootstrap RPC (RegisterVolunteer) must ride it
// out rather than fail its single-head caller into exiting (TODO #33/#64): unlike
// the connect probe in ConnectWithRetry, a register failure is fatal at the call
// site, so a transiently rate-limited register would otherwise drop the daemon.
// Any other error (or success) is returned to the caller on the first occurrence,
// so a genuinely unreachable head is surfaced, never masked. It reuses the same
// window-sized backoff (connectRateLimitBackoff + jitter) ConnectWithRetry uses.
func retryRPCOnRateLimit(ctx context.Context, logger *slog.Logger, op string, fn func(context.Context) error) error {
	for {
		err := fn(ctx)
		if status.Code(err) != codes.ResourceExhausted {
			return err
		}

		sleepDur := connectRateLimitBackoff + jitter(connectRateLimitBackoff)
		if logger != nil {
			logger.Info("server rate-limited, backing off (will keep retrying)",
				"op", op,
				"backoff", sleepDur,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during %s retry: %w", op, ctx.Err())
		case <-time.After(sleepDur):
		}
	}
}

// jitter returns a random 0-25% of d (0 if d is too small to jitter), used to
// spread retry backoffs and prevent a thundering herd.
func jitter(d time.Duration) time.Duration {
	if jitterMax := int64(d) / 4; jitterMax > 0 {
		return time.Duration(rand.Int64N(jitterMax))
	}
	return 0
}
