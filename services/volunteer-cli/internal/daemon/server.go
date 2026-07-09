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
	// HostID is the SERVER-ISSUED per-machine host id THIS head minted for this machine
	// (BG-25), echoed on every work request so the head keys per-machine metering on it.
	// It is per-head: each head issues its own id, so it lives on the connection beside
	// the per-head VolunteerID rather than as one daemon-wide value. Empty => host-less
	// (per-account fallback). Updated in place when the work path self-heals a
	// host-unknown refusal by re-registering.
	HostID    string
	Name      string        // display name (config.Name or hostname)
	Available bool          // false if last request failed
	LastError time.Time     // when the last error occurred
	Backoff   time.Duration // current backoff for this server

	// NextContactAt is the earliest wall-clock time the fetcher may issue the
	// next RequestWorkUnit to this head. It is set from the head's authoritative
	// server-directed retry delay (RequestWorkUnitResponse.RetryAfterSeconds) on
	// every reply, and from a fixed jittered local backoff on ResourceExhausted.
	// It lives on the per-head connection (not the Fetcher) so it survives the
	// fetcher being recreated on pause/resume. Zero means "contact immediately".
	NextContactAt time.Time
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
