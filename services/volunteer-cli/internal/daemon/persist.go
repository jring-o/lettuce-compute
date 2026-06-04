package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// PersistedTask captures all state needed to resume a work unit across daemon restarts.
type PersistedTask struct {
	WorkUnitID              string            `json:"work_unit_id"`
	LeafID                  string            `json:"leaf_id"`
	ServerGRPCAddress       string            `json:"server_grpc_address"`
	ServerName              string            `json:"server_name"`
	VolunteerID             string            `json:"volunteer_id"`
	RuntimeName             string            `json:"runtime"`
	WorkDir                 string            `json:"work_dir"`
	BinaryPath              string            `json:"binary_path,omitempty"`
	InputPath               string            `json:"input_path,omitempty"`
	CodeArtifactURL         string            `json:"code_artifact_url,omitempty"`
	ParametersJSON          string            `json:"parameters_json,omitempty"`
	DeadlineSeconds         int32             `json:"deadline_seconds"`
	EnvVars                 map[string]string `json:"env_vars,omitempty"`
	ExecutionSpec           runtime.ExecutionSpec `json:"execution_spec"`
	CheckpointSequence      int32             `json:"checkpoint_sequence"`
	CheckpointIntervalSecs  int32             `json:"checkpoint_interval_seconds"`
	RscFpopsEst             float64           `json:"rsc_fpops_est,omitempty"`
	VizBundlePath           string            `json:"viz_bundle_path,omitempty"`
	StartedAt               time.Time         `json:"started_at"`
	PID                     int               `json:"pid,omitempty"` // OS PID for resuming suspended orphans
}

// PersistedState is the top-level structure saved to disk.
type PersistedState struct {
	SavedAt time.Time       `json:"saved_at"`
	Tasks   []PersistedTask `json:"tasks"`
}

func activeTasksPath(dataDir string) string {
	return filepath.Join(dataDir, "active-tasks.json")
}

// SaveActiveState writes the current in-progress tasks to disk so they can be
// resumed on the next daemon startup.
func SaveActiveState(dataDir string, tasks []PersistedTask) error {
	state := PersistedState{
		SavedAt: time.Now().UTC(),
		Tasks:   tasks,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling active tasks: %w", err)
	}
	return os.WriteFile(activeTasksPath(dataDir), data, 0600)
}

// LoadActiveState reads previously saved in-progress tasks from disk.
// Returns nil, nil if no state file exists.
func LoadActiveState(dataDir string) (*PersistedState, error) {
	data, err := os.ReadFile(activeTasksPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading active tasks: %w", err)
	}
	var state PersistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing active tasks: %w", err)
	}
	return &state, nil
}

// ClearActiveState removes the active tasks file.
func ClearActiveState(dataDir string) {
	os.Remove(activeTasksPath(dataDir))
}
