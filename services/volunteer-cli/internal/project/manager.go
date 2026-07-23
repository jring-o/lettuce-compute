package project

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// LeafSummary is a leaf returned from the infrastructure REST API.
type LeafSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	ResearchArea string `json:"research_area"`
	TaskPattern  string `json:"task_pattern"`
	State        string `json:"state"`
}

// Status describes the current volunteer state.
type Status struct {
	DaemonRunning bool
	DaemonPID     int
	VolunteerID   string
	Servers       []config.ServerConfig
}

// Manager handles leaf-level operations.
type Manager struct {
	cfg     *config.Config
	cfgPath string
	logger  *slog.Logger
}

// NewManager creates a leaf manager.
func NewManager(cfg *config.Config, cfgPath string, logger *slog.Logger) *Manager {
	return &Manager{cfg: cfg, cfgPath: cfgPath, logger: logger}
}

// listLeafsResponse mirrors the REST API paginated response.
type listLeafsResponse struct {
	Data       []LeafSummary `json:"data"`
	Pagination struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
		Total  int    `json:"total"`
	} `json:"pagination"`
}

// ListLeafs fetches available leafs from a server's REST API.
func (m *Manager) ListLeafs(ctx context.Context, httpAddress string) ([]LeafSummary, error) {
	var all []LeafSummary
	cursor := ""

	for {
		url := fmt.Sprintf("%s/api/v1/leafs?limit=50", strings.TrimRight(httpAddress, "/"))
		if cursor != "" {
			url += "&cursor=" + cursor
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching leafs: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
		}

		var page listLeafsResponse
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding response: %w", err)
		}
		resp.Body.Close()

		all = append(all, page.Data...)

		if page.Pagination.Cursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = page.Pagination.Cursor
	}

	// Filter by leaf preferences if applicable.
	if m.cfg.Leafs.Mode == "SPECIFIC" && len(m.cfg.Leafs.LeafIDs) > 0 {
		wanted := make(map[string]bool, len(m.cfg.Leafs.LeafIDs))
		for _, id := range m.cfg.Leafs.LeafIDs {
			wanted[id] = true
		}
		filtered := make([]LeafSummary, 0, len(all))
		for _, p := range all {
			if wanted[p.ID] {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}
	if m.cfg.Leafs.Mode == "BLOCKLIST" && len(m.cfg.Leafs.BlockedIDs) > 0 {
		blocked := make(map[string]bool, len(m.cfg.Leafs.BlockedIDs))
		for _, id := range m.cfg.Leafs.BlockedIDs {
			blocked[id] = true
		}
		filtered := make([]LeafSummary, 0, len(all))
		for _, p := range all {
			if !blocked[p.ID] {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}

	return all, nil
}

// AttachLeaf pins a specific leaf on a head. When the head is already attached
// (an entry with the same gRPC address exists) the pin is MERGED into that
// entry, keeping its TLS/trust settings — the old behavior of appending a whole
// second entry dropped those flags and the daemon then collapsed the duplicate
// at startup, silently discarding the pin (PB-16). When the head is not yet
// attached a new entry is created carrying the pin.
func (m *Manager) AttachLeaf(leafID, grpcAddr, httpAddr, name string) error {
	for i := range m.cfg.Servers {
		if m.cfg.Servers[i].GRPCAddress != grpcAddr {
			continue
		}
		for _, p := range m.cfg.Servers[i].PinnedLeafIDs {
			if p == leafID {
				return fmt.Errorf("already attached to leaf %s on %s", leafID, grpcAddr)
			}
		}
		m.cfg.Servers[i].PinnedLeafIDs = append(m.cfg.Servers[i].PinnedLeafIDs, leafID)
		return m.cfg.Save(m.cfgPath)
	}

	m.cfg.Servers = append(m.cfg.Servers, config.ServerConfig{
		GRPCAddress:   grpcAddr,
		HTTPAddress:   httpAddr,
		PinnedLeafIDs: []string{leafID},
		Name:          name,
	})

	return m.cfg.Save(m.cfgPath)
}

// HasServer reports whether a server entry with the given gRPC address exists.
func (m *Manager) HasServer(grpcAddr string) bool {
	for _, s := range m.cfg.Servers {
		if s.GRPCAddress == grpcAddr {
			return true
		}
	}
	return false
}

// ApplyServerFlags updates connection settings on an EXISTING head entry from
// explicitly-given attach flags (nil = flag not given, leave as configured).
// Part of the PB-16 fix: `attach --server <host> --leaf <id> --insecure
// --trust …` on an already-attached head used to drop these flags entirely.
func (m *Manager) ApplyServerFlags(grpcAddr string, insecure *bool, caCertPath *string, trustedRuntimes []string) error {
	for i := range m.cfg.Servers {
		if m.cfg.Servers[i].GRPCAddress != grpcAddr {
			continue
		}
		if insecure != nil {
			m.cfg.Servers[i].Insecure = *insecure
		}
		if caCertPath != nil {
			m.cfg.Servers[i].CACertPath = *caCertPath
		}
		if trustedRuntimes != nil {
			m.cfg.Servers[i].TrustedRuntimes = trustedRuntimes
		}
		return m.cfg.Save(m.cfgPath)
	}
	return fmt.Errorf("no configured server with address %s", grpcAddr)
}

// AttachServerWithTLS adds a self-hosted server connection with TLS configuration and the
// per-head runtime trust the volunteer chose for it (trustedRuntimes: the UPPERCASE opt-ins
// beyond the always-allowed WASM — e.g. ["CONTAINER"]). Pass a NON-NIL empty list for an
// explicit "WASM only": nil is reserved for entries that predate per-head trust, which the
// config loader re-seeds from the legacy global knobs — handing it a deliberate "none"
// would silently upgrade that choice (PB-28).
func (m *Manager) AttachServerWithTLS(host string, grpcPort, httpPort int, insecure bool, caCertPath string, trustedRuntimes []string) error {
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
	// Omit port from URL when using the standard port for the scheme.
	var httpAddr string
	if (httpScheme == "https" && httpPort == 443) || (httpScheme == "http" && httpPort == 80) {
		httpAddr = fmt.Sprintf("%s://%s", httpScheme, host)
	} else {
		httpAddr = fmt.Sprintf("%s://%s:%d", httpScheme, host, httpPort)
	}

	for _, s := range m.cfg.Servers {
		if s.GRPCAddress == grpcAddr && s.LeafID == "" {
			return fmt.Errorf("already attached to server %s", grpcAddr)
		}
	}

	m.cfg.Servers = append(m.cfg.Servers, config.ServerConfig{
		GRPCAddress:     grpcAddr,
		HTTPAddress:     httpAddr,
		Name:            host,
		Insecure:        insecure,
		CACertPath:      caCertPath,
		TrustedRuntimes: trustedRuntimes,
	})

	return m.cfg.Save(m.cfgPath)
}

// DetachLeaf removes a leaf pin from whichever head entry carries it. The head
// entry itself stays attached (use DetachServer to drop the head).
func (m *Manager) DetachLeaf(leafID string) error {
	for i := range m.cfg.Servers {
		pins := m.cfg.Servers[i].PinnedLeafIDs
		for j, p := range pins {
			if p != leafID {
				continue
			}
			m.cfg.Servers[i].PinnedLeafIDs = append(pins[:j], pins[j+1:]...)
			if len(m.cfg.Servers[i].PinnedLeafIDs) == 0 {
				m.cfg.Servers[i].PinnedLeafIDs = nil
			}
			return m.cfg.Save(m.cfgPath)
		}
	}
	return fmt.Errorf("leaf %s not found in configured servers", leafID)
}

// DetachServer removes all server entries matching a hostname.
func (m *Manager) DetachServer(host string) error {
	var remaining []config.ServerConfig
	found := false
	for _, s := range m.cfg.Servers {
		if strings.Contains(s.GRPCAddress, host) {
			found = true
			continue
		}
		remaining = append(remaining, s)
	}
	if !found {
		return fmt.Errorf("server %s not found in configured servers", host)
	}

	m.cfg.Servers = remaining
	return m.cfg.Save(m.cfgPath)
}

// GetStatus returns the current volunteer daemon status.
func (m *Manager) GetStatus(ctx context.Context) (*Status, error) {
	st := &Status{
		VolunteerID: m.cfg.VolunteerID,
		Servers:     m.cfg.Servers,
	}

	pid, err := daemon.ReadPID(m.cfg.DataDir)
	if err == nil && daemon.IsProcessRunning(pid) {
		st.DaemonRunning = true
		st.DaemonPID = pid
	}

	return st, nil
}

// GetHistory reads the most recent entries from the history file.
func (m *Manager) GetHistory(ctx context.Context, limit int) ([]daemon.HistoryEntry, error) {
	return daemon.ReadHistory(m.cfg.DataDir, limit)
}
