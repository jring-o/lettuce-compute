package leaf

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LeafState represents the lifecycle state of a leaf.
type LeafState string

const (
	StateDraft       LeafState = "DRAFT"
	StateConfiguring LeafState = "CONFIGURING"
	StateActive      LeafState = "ACTIVE"
	StatePaused      LeafState = "PAUSED"
	StateCompleted   LeafState = "COMPLETED"
	StateArchived    LeafState = "ARCHIVED"
)

// TaskPattern represents the computational pattern of a leaf.
type TaskPattern string

const (
	PatternParameterSweep TaskPattern = "PARAMETER_SWEEP"
	PatternMapReduce      TaskPattern = "MAP_REDUCE"
	PatternMonteCarlo     TaskPattern = "MONTE_CARLO"
	PatternCustom         TaskPattern = "CUSTOM"
)

// LeafVisibility controls discoverability.
type LeafVisibility string

const (
	VisibilityPublic   LeafVisibility = "PUBLIC"
	VisibilityUnlisted LeafVisibility = "UNLISTED"
	VisibilityPrivate  LeafVisibility = "PRIVATE"
)

// Runtime type constants.
const (
	RuntimeNative    = "NATIVE"
	RuntimeContainer = "CONTAINER"
	RuntimeWasm      = "WASM"
	RuntimeScript    = "SCRIPT"
)

// GPU type constants.
const (
	GPUTypeAny    = "ANY"
	GPUTypeNvidia = "NVIDIA"
	GPUTypeAMD    = "AMD"
	GPUTypeWebGPU = "WEBGPU"
)

// Script language constants.
const (
	LangPython = "PYTHON"
	LangR      = "R"
	LangJulia  = "JULIA"
)

// Comparison mode constants.
const (
	ComparisonExact            = "EXACT"
	ComparisonNumericTolerance = "NUMERIC_TOLERANCE"
	ComparisonCustom           = "CUSTOM"
)

// Transfer strategy constants.
const (
	TransferInline            = "INLINE"
	TransferPlatformManaged   = "PLATFORM_MANAGED"
	TransferExternalReference = "EXTERNAL_REFERENCE"
)

// Aggregation format constants.
const (
	AggregationJSON    = "JSON"
	AggregationCSV     = "CSV"
	AggregationParquet = "PARQUET"
	AggregationCustom  = "CUSTOM"
)

// ExecutionConfig defines runtime type, binaries, resource limits.
type ExecutionConfig struct {
	Runtime  string            `json:"runtime"`
	Binaries map[string]string `json:"binaries,omitempty"`
	// BinaryChecksums maps each platform key in Binaries to the lowercase hex
	// SHA-256 of the artifact at that URL. Volunteers verify downloaded bytes
	// against this before execution. Required for every Binaries entry when
	// Runtime is NATIVE; optional (but format-validated) otherwise.
	BinaryChecksums map[string]string `json:"binary_checksums,omitempty"`
	Image           *string           `json:"image"`
	Dockerfile    *string           `json:"dockerfile"`
	Language      *string           `json:"language"`
	EntryPoint    *string           `json:"entry_point"`
	Dependencies  *string           `json:"dependencies"`
	GPURequired   bool              `json:"gpu_required"`
	GPUType       string            `json:"gpu_type"`
	MinVRAMGB     int               `json:"min_vram_gb"`
	NetworkAccess bool              `json:"network_access"`
	MaxMemoryMB   int               `json:"max_memory_mb"`
	MaxDiskMB     int               `json:"max_disk_mb"`
	MaxCPUSeconds int               `json:"max_cpu_seconds"`
	EnvVars       map[string]string `json:"env_vars,omitempty"`
	RscFpopsEst   float64           `json:"rsc_fpops_est,omitempty"` // estimated FP ops per work unit (for time estimates)
}

// ValidationConfig defines redundancy and comparison settings.
type ValidationConfig struct {
	RedundancyFactor    int      `json:"redundancy_factor"`
	AgreementThreshold  float64  `json:"agreement_threshold"`
	ComparisonMode      string   `json:"comparison_mode"`
	NumericTolerance    *float64 `json:"numeric_tolerance"`
	CustomComparatorRef *string  `json:"custom_comparator_ref"`
	MaxRetries          int      `json:"max_retries"`
	SpotCheckEnabled    bool     `json:"spot_check_enabled"`
	SpotCheckPercentage float64  `json:"spot_check_percentage"`
}

// FaultToleranceConfig defines heartbeat and deadline settings.
type FaultToleranceConfig struct {
	HeartbeatIntervalSeconds  int     `json:"heartbeat_interval_seconds"`
	MissedHeartbeatsThreshold int     `json:"missed_heartbeats_threshold"`
	DeadlineMultiplier        float64 `json:"deadline_multiplier"`
	// NoDeadline disables the hard wall-clock deadline for this leaf's work
	// units: ResolveDeadlineSeconds returns 0, which the volunteer runtime reads
	// as "no timeout" and FindExpiredWorkUnits skips (the deadline_seconds > 0
	// guard). Liveness is then governed solely by heartbeats, so a work unit may
	// run indefinitely as long as it keeps heartbeating. Defaults to false
	// (deadline enforced via DeadlineMultiplier).
	NoDeadline                bool  `json:"no_deadline"`
	MaxReassignments          int   `json:"max_reassignments"`
	CheckpointingEnabled      bool  `json:"checkpointing_enabled"`
	CheckpointIntervalSeconds *int  `json:"checkpoint_interval_seconds"`
	MaxCheckpointSizeBytes    int64 `json:"max_checkpoint_size_bytes"`
}

// Generation mode constants.
const (
	GenerationModeEager = "eager"
	GenerationModeLazy  = "lazy"
)

// DataConfig defines data transfer and splitting settings.
type DataConfig struct {
	TransferStrategy   string         `json:"transfer_strategy"`
	StorageBucket      *string        `json:"storage_bucket"`
	ExternalBaseURL    *string        `json:"external_base_url"`
	SplittingStrategy  *string        `json:"splitting_strategy"`
	SplittingConfig    map[string]any `json:"splitting_config,omitempty"`
	AggregationFormat  string         `json:"aggregation_format"`
	AggregationConfig  map[string]any `json:"aggregation_config,omitempty"`
	MaxInputSizeBytes  int64          `json:"max_input_size_bytes"`
	MaxOutputSizeBytes int64          `json:"max_output_size_bytes"`
	GenerationMode     string         `json:"generation_mode"`
	LazyThreshold      int            `json:"lazy_threshold"`
	LazyBatchSize      int            `json:"lazy_batch_size"`
}

// CreditConfig defines credit calculation parameters.
type CreditConfig struct {
	CreditPerValidatedWorkUnit float64 `json:"credit_per_validated_work_unit"`
}

// HealthConfig defines configurable health metric alert thresholds.
type HealthConfig struct {
	ContributionFlowAlertHours  int     `json:"contribution_flow_alert_hours"`  // default: 48
	WorkAvailabilityAlertRatio  float64 `json:"work_availability_alert_ratio"`  // default: 0.1
	VolunteerActivityAlertCount int     `json:"volunteer_activity_alert_count"` // default: 0
}

// DefaultHealthConfig returns the default health thresholds.
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		ContributionFlowAlertHours:  48,
		WorkAvailabilityAlertRatio:  0.1,
		VolunteerActivityAlertCount: 0,
	}
}

// ResourceRequirements defines minimum volunteer hardware.
type ResourceRequirements struct {
	MinCPUCores int `json:"min_cpu_cores"`
	// Memory has no separate minimum: it is governed solely by
	// ExecutionConfig.MaxMemoryMB (the container limit), which is also what the
	// scheduler matches against — a single memory bound for the whole task.
	MinDiskMB            int     `json:"min_disk_mb"`
	MinGPUVRAMMB         int     `json:"min_gpu_vram_mb"`
	GPURequired          bool    `json:"gpu_required"`
	GPUComputeCapability *string `json:"gpu_compute_capability"`
	MinBandwidthMbps     int     `json:"min_bandwidth_mbps"`
}

// Leaf is a computation within a head (server) — the fundamental entity of Lettuce.
type Leaf struct {
	ID                   types.ID             `json:"id"`
	Name                 string               `json:"name"`
	Slug                 string               `json:"slug"`
	Description          string               `json:"description"`
	ResearchArea         []string             `json:"research_area"`
	CreatorID            *types.ID            `json:"creator_id"`
	CreatorPublicKey     []byte               `json:"creator_public_key,omitempty"`
	State                LeafState            `json:"state"`
	TaskPattern          TaskPattern          `json:"task_pattern"`
	ExecutionConfig      ExecutionConfig      `json:"execution_config"`
	ValidationConfig     ValidationConfig     `json:"validation_config"`
	FaultToleranceConfig FaultToleranceConfig `json:"fault_tolerance_config"`
	DataConfig           DataConfig           `json:"data_config"`
	CreditConfig         CreditConfig         `json:"credit_config"`
	ResourceRequirements ResourceRequirements `json:"resource_requirements"`
	HealthConfig         HealthConfig         `json:"health_config"`
	IsOngoing            bool                 `json:"is_ongoing"`
	Visibility           LeafVisibility       `json:"visibility"`
	StatsCacheSeconds    int                  `json:"stats_cache_seconds"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
}

// SortField specifies which column to sort by.
type SortField string

const (
	SortCreatedAt SortField = "created_at"
	SortUpdatedAt SortField = "updated_at"
	SortName      SortField = "name"
)

// SortOrder specifies ascending or descending.
type SortOrder string

const (
	OrderAsc  SortOrder = "ASC"
	OrderDesc SortOrder = "DESC"
)

// LeafListFilters controls filtering, sorting, and searching for List queries.
type LeafListFilters struct {
	State        *LeafState      `json:"state,omitempty"`
	CreatorID    *types.ID       `json:"creator_id,omitempty"`
	Visibility   *LeafVisibility `json:"visibility,omitempty"`
	ResearchArea *string         `json:"research_area,omitempty"`
	Search       *string         `json:"search,omitempty"`
	Sort         SortField       `json:"sort"`
	Order        SortOrder       `json:"order"`
}
