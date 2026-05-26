package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// ServerConnection holds the connection state for a single infrastructure server.
type ServerConnection struct {
	Config      config.ServerConfig
	Client      WorkClient
	VolunteerID string
	Name        string        // display name (config.Name or hostname)
	Available   bool          // false if last request failed
	LastError   time.Time     // when the last error occurred
	Backoff     time.Duration // current backoff for this server
}

// DaemonState is persisted to disk so the status command can show per-server info.
type DaemonState struct {
	Servers []ServerState `json:"servers"`
}

// ServerState describes one server's connection state at daemon startup.
type ServerState struct {
	Name        string `json:"name"`
	GRPCAddress string `json:"grpc_address"`
	VolunteerID string `json:"volunteer_id"`
	Connected   bool   `json:"connected"`
}

// daemonStatePath returns the path to the daemon state file.
func daemonStatePath(dataDir string) string {
	return filepath.Join(dataDir, "daemon-state.json")
}

// WriteDaemonState persists the daemon's connection state to disk.
func WriteDaemonState(dataDir string, state *DaemonState) error {
	path := daemonStatePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling daemon state: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing daemon state: %w", err)
	}
	return nil
}

// ReadDaemonState reads the persisted daemon state from disk.
func ReadDaemonState(dataDir string) (*DaemonState, error) {
	data, err := os.ReadFile(daemonStatePath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading daemon state: %w", err)
	}

	var state DaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing daemon state: %w", err)
	}
	return &state, nil
}

// RemoveDaemonState removes the daemon state file.
func RemoveDaemonState(dataDir string) {
	os.Remove(daemonStatePath(dataDir))
}
