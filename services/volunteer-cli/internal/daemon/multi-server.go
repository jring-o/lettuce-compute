package daemon

import (
	"context"
	"log/slog"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MultiServerClient manages connections to multiple infrastructure servers
// and cycles through them when requesting work.
type MultiServerClient struct {
	servers        []*ServerConnection
	current        int // index of the server to try next
	logger         *slog.Logger
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// NewMultiServerClient creates a multi-server client that cycles through
// servers in round-robin order.
func NewMultiServerClient(servers []*ServerConnection, logger *slog.Logger) *MultiServerClient {
	return &MultiServerClient{
		servers:        servers,
		current:        0,
		logger:         logger,
		initialBackoff: 5 * time.Second,
		maxBackoff:     60 * time.Second,
	}
}

// RequestWork tries each server in round-robin order until one returns a work
// unit or all servers have been tried. Returns the work unit response and the
// server it came from.
func (m *MultiServerClient) RequestWork(ctx context.Context, pubKey []byte, leafIDs, blockedIDs []string, hw *lettucev1.HardwareCapabilities) (*lettucev1.RequestWorkUnitResponse, *ServerConnection, error) {
	n := len(m.servers)
	start := m.current

	for i := 0; i < n; i++ {
		idx := (start + i) % n
		srv := m.servers[idx]

		// Skip servers in backoff.
		if !srv.Available && time.Since(srv.LastError) < srv.Backoff {
			m.logger.Debug("skipping server in backoff",
				"server", srv.Name,
				"backoff_remaining", srv.Backoff-time.Since(srv.LastError),
			)
			continue
		}

		resp, err := srv.Client.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
			VolunteerId:       srv.VolunteerID,
			PublicKey:         pubKey,
			ProjectIds:        leafIDs,
			BlockedProjectIds: blockedIDs,
			CurrentAvailable:  hw,
		})
		if err != nil {
			st, ok := status.FromError(err)
			if ok && st.Code() == codes.NotFound {
				// No work available — server is healthy, just empty.
				srv.Available = true
				srv.Backoff = 0
				m.logger.Debug("no work available", "server", srv.Name)
				continue
			}
			// Connection error — apply per-server backoff.
			srv.Available = false
			srv.LastError = time.Now()
			if srv.Backoff == 0 {
				srv.Backoff = m.initialBackoff
			} else {
				srv.Backoff = time.Duration(float64(srv.Backoff) * 2)
				if srv.Backoff > m.maxBackoff {
					srv.Backoff = m.maxBackoff
				}
			}
			m.logger.Warn("server request failed",
				"server", srv.Name,
				"error", err,
				"backoff", srv.Backoff,
			)
			continue
		}

		// Got work — advance to next server for fairness.
		srv.Available = true
		srv.Backoff = 0
		m.current = (idx + 1) % n
		return resp, srv, nil
	}

	return nil, nil, status.Error(codes.NotFound, "no work available from any server")
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
