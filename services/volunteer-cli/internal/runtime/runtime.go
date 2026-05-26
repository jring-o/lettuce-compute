package runtime

import (
	"context"
	"time"
)

// WorkUnit contains all info needed to execute a unit of work.
type WorkUnit struct {
	ID              string            // work unit UUID
	LeafID          string            // leaf UUID
	Runtime         string            // "native", "container", etc.
	InputData       []byte            // inline data (< 1 MB)
	InputDataURL    string            // URL for external data
	CodeArtifactURL string            // URL to download code/binary
	ParametersJSON  string            // parameter set as JSON
	DeadlineSeconds int32             // max wall-clock time
	EnvVars         map[string]string // environment variables
	ExecutionSpec   ExecutionSpec     // runtime-specific config
	RscFpopsEst     float64           // estimated FP ops (0 = unknown)

	// Checkpoint fields (from RequestWorkUnitResponse)
	HasCheckpoint             bool  // true if a checkpoint exists for this reassigned WU
	CheckpointSequence        int32 // sequence number of the latest checkpoint
	CheckpointIntervalSeconds int32 // from leaf config (0 = no checkpointing)
}

// ExecutionSpec describes how to run the work unit.
type ExecutionSpec struct {
	Binaries map[string]string // platform -> URL (native)
	// BinaryChecksums maps a platform key in Binaries to the expected lowercase
	// hex SHA-256 of the artifact at that URL. Runtimes verify downloaded bytes
	// against this before execution. For native binaries a checksum is required
	// (fail-closed); for wasm/viz it is verified when present.
	BinaryChecksums map[string]string
	Image           string // OCI image (container)
	GPURequired     bool
	GPUType         string // "nvidia", "amd", "any", or "" (empty = any)
	MinVRAMMB       int32  // minimum GPU VRAM required (from leaf config)
	MaxMemoryMB     int32
	MaxDiskMB       int32
	NetworkAccess   bool
}

// PrepareResult contains paths and metadata from the prepare phase.
type PrepareResult struct {
	BinaryPath    string // path to downloaded/cached binary
	InputPath     string // path to input data file (if written to disk)
	WorkDir       string // temp working directory for execution
	VizBundlePath string // path to extracted viz bundle ({workDir}/.lettuce-viz), or "" if none

	// PIDCallback is called by the native runtime after the process starts,
	// passing the child PID. Set by the slot before calling Execute().
	PIDCallback func(pid int)

	// ContainerIDCallback is called by the container runtime after the
	// container starts. Set by the slot before calling Execute().
	ContainerIDCallback func(containerID string)

	// OrphanPID, if > 0, means an already-running (suspended) process exists
	// from a previous daemon session. The slot should poll for its completion
	// instead of calling rt.Execute() to start a new process.
	OrphanPID int

	// OriginalStartedAt preserves the original start time for resumed tasks
	// so elapsed time displays correctly instead of resetting to zero.
	OriginalStartedAt time.Time
}

// ExecutionResult contains output and metrics from execution.
type ExecutionResult struct {
	OutputData     []byte           // raw output data
	OutputChecksum string           // SHA-256 hex digest of output
	ExitCode       int              // process exit code
	Metrics        ExecutionMetrics // resource usage
}

// ExecutionMetrics maps directly to the proto ExecutionMetadata.
type ExecutionMetrics struct {
	WallClockSeconds int64
	CPUSecondsUser   float64
	CPUSecondsSystem float64
	CPUCoresUsed     int32
	PeakMemoryMB     int32
	DiskReadMB       int64
	DiskWriteMB      int64
	// GPU metrics (v0.7)
	GPUSeconds    float64
	GPUModel      string
	GPUVRAMUsedMB int32
}

// Runtime is the interface all execution environments implement.
type Runtime interface {
	// Prepare downloads code/binaries and sets up the work directory.
	Prepare(ctx context.Context, wu *WorkUnit) (*PrepareResult, error)

	// Execute runs the work unit and returns the result.
	Execute(ctx context.Context, wu *WorkUnit, prep *PrepareResult) (*ExecutionResult, error)

	// Cleanup removes temp files and releases resources.
	Cleanup(prep *PrepareResult) error

	// CanHandle returns true if this runtime supports the given spec.
	CanHandle(spec *ExecutionSpec) bool

	// Name returns the runtime identifier ("native", "container", etc.)
	Name() string
}
