package daemon

import (
	"log/slog"
	"time"
)

// MultiServerClient manages connections to multiple infrastructure servers.
// Work selection (deficit-ordered head + leaf, batched, server-directed retry
// delay) lives in the Fetcher; this type now only owns the connection list and
// its shared backoff knobs.
type MultiServerClient struct {
	servers        []*ServerConnection
	logger         *slog.Logger
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// NewMultiServerClient creates a multi-server client over the given connections.
func NewMultiServerClient(servers []*ServerConnection, logger *slog.Logger) *MultiServerClient {
	return &MultiServerClient{
		servers:        servers,
		logger:         logger,
		initialBackoff: 5 * time.Second,
		maxBackoff:     60 * time.Second,
	}
}

// Servers returns the list of server connections.
func (m *MultiServerClient) Servers() []*ServerConnection {
	return m.servers
}

// SetBackoff overrides backoff durations (for testing).
func (m *MultiServerClient) SetBackoff(initial, max time.Duration) {
	m.initialBackoff = initial
	m.maxBackoff = max
}
