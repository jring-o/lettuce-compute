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

	// TargetCopies / MinQuorum split the conflated redundancy_factor (TODO #50):
	// target_copies is how many copies to dispatch concurrently; min_quorum is how many
	// agreeing results validate the unit. Invariant: 1 <= min_quorum <= target_copies.
	// Both omitempty with a 0 sentinel meaning "= redundancy_factor" — so a leaf that only
	// sets redundancy_factor behaves byte-for-byte as before (target == quorum ==
	// redundancy_factor). Setting target_copies > min_quorum lets a leaf over-dispatch and
	// validate at quorum without waiting for stragglers (removes the serial-reassignment
	// latency tail). redundancy_factor remains the back-compat alias for target == quorum.
	TargetCopies int `json:"target_copies,omitempty"`
	MinQuorum    int `json:"min_quorum,omitempty"`

	// MaxTotalCopies / MaxErrorCopies / MaxSuccessCopies are the hard caps that bound a
	// non-converging unit (TODO #50, reconciling #40). All 0 = the documented default:
	//   MaxTotalCopies   0 -> target_copies + a retry margin (the dead-letter ceiling,
	//                         previously only the inert per-WU column EffectiveMaxTotalCopies)
	//   MaxErrorCopies   0 -> unlimited (only MaxTotalCopies bounds errors, as today)
	//   MaxSuccessCopies 0 -> target_copies (dispatch already stops at target today)
	// Stamped per-unit at generation; resolved through transition.RedundancyPolicy.
	MaxTotalCopies   int `json:"max_total_copies,omitempty"`
	MaxErrorCopies   int `json:"max_error_copies,omitempty"`
	MaxSuccessCopies int `json:"max_success_copies,omitempty"`

	// IgnoreFields lists output JSON field paths to EXCLUDE from result comparison —
	// volatile provenance like a wall-clock "compute_time_ms" that legitimately differs
	// run-to-run and would otherwise break agreement. A path matches a field iff it
	// equals the field's dotted path, or is a dot-boundary prefix of it (subtree match);
	// inside arrays the index is elided, so "fights.compute_time_ms" drops that key from
	// every element. Honored by EXACT (the comparison checksum is recomputed from the
	// stored output with these fields stripped + keys sorted) and by NUMERIC_TOLERANCE.
	// Requires INLINE output (the head must have the bytes); ignored for EXTERNAL_REFERENCE.
	IgnoreFields []string `json:"ignore_fields,omitempty"`
	// CompareFields, when non-empty, RESTRICTS NUMERIC_TOLERANCE comparison to exactly
	// these output JSON field paths (numeric leaves compared within numeric_tolerance,
	// non-numeric leaves compared for equality). Use it to verify only aggregate science
	// (e.g. ["a_win_rate","knockout_rate","mean_duration_s"]) on a chaotic sim whose raw
	// per-fight trajectories legitimately diverge across volunteers. Empty = compare all
	// numeric leaves (minus IgnoreFields). No effect on EXACT.
	CompareFields []string `json:"compare_fields,omitempty"`
	// HomogeneousRedundancy, when true, pins every copy of a work unit to volunteers of a
	// single hardware class (CPU vendor + OS + arch), so bit-for-bit agreement is
	// achievable even for engines that are not portably deterministic. See dispatch.
	HomogeneousRedundancy bool `json:"homogeneous_redundancy,omitempty"`

	// AllowExternalOutput permits a volunteer to submit a result as an external
	// reference (output_data_url) instead of inline bytes. Such a reference carries a
	// volunteer-claimed output checksum that the head does NOT verify against the
	// referenced bytes — there is no fetch-and-hash pipeline — so under EXACT
	// comparison that unverified checksum becomes the agreement key, and "agreement"
	// degrades to colluders repeating the same string. Enabling this therefore weakens
	// corroboration for the leaf. Off by default: SubmitResult rejects external-
	// reference submissions for any leaf that has not opted in.
	AllowExternalOutput bool `json:"allow_external_output,omitempty"`

	// MinTrustedCorroborators is the per-leaf trust-gate override: how many DISTINCT
	// trusted subjects the agreeing group must contain to validate. 0 = inherit the head
	// default (the gate itself is enabled head-wide by config; this only overrides K).
	MinTrustedCorroborators int `json:"min_trusted_corroborators,omitempty"`
	// TrustFloor is the per-leaf trust-floor override: the snapshot score at or above
	// which a subject counts as trusted. 0 = inherit the head default. TIGHTEN-ONLY:
	// the effective floor is max(this, head default) — a leaf may demand a higher
	// floor, never redefine the head's trust currency downward.
	TrustFloor int `json:"trust_floor,omitempty"`

	// AuditRate is the per-leaf post-hoc result-audit sampling override, a fraction
	// in [0, 1]. 0 = no override. RAISE-ONLY: the effective rate is
	// max(this, head default) — leaf creation is self-service and the leaf owner is
	// the threat model's primary adversary, so a leaf can volunteer for MORE
	// scrutiny but never dodge the head's sampling floor.
	AuditRate float64 `json:"audit_rate,omitempty"`
}

// EffectiveTargetCopies resolves the target_copies 0 sentinel (TODO #50): the configured
// target_copies if set, else redundancy_factor (floored at 1). This is the leaf-config
// layer of the resolution; transition.ResolvePolicy layers the per-unit stamp on top.
func (c ValidationConfig) EffectiveTargetCopies() int {
	if c.TargetCopies > 0 {
		return c.TargetCopies
	}
	if c.RedundancyFactor < 1 {
		return 1
	}
	return c.RedundancyFactor
}

// EffectiveMinQuorum resolves the min_quorum 0 sentinel (TODO #50): the configured
// min_quorum if set, else redundancy_factor (floored at 1, and defensively clamped to not
// exceed the effective target — the validator rejects min_quorum > target_copies up front).
func (c ValidationConfig) EffectiveMinQuorum() int {
	q := c.MinQuorum
	if q <= 0 {
		q = c.RedundancyFactor
	}
	if q < 1 {
		q = 1
	}
	if t := c.EffectiveTargetCopies(); q > t {
		q = t
	}
	return q
}

// FaultToleranceConfig defines deadline-based reassignment settings.
type FaultToleranceConfig struct {
	// HeartbeatIntervalSeconds and MissedHeartbeatsThreshold are DEPRECATED and
	// INERT: v0.3.0 removed per-task heartbeats in favour of deadline-based
	// reassignment (DeadlineMultiplier + the StartWork-stamped reclaim deadline).
	// They are no longer required, range-checked, or read by any liveness logic.
	// The fields are retained only so older callers that still send them do not
	// break; new callers should omit them.
	HeartbeatIntervalSeconds  int     `json:"heartbeat_interval_seconds"`
	MissedHeartbeatsThreshold int     `json:"missed_heartbeats_threshold"`
	DeadlineMultiplier        float64 `json:"deadline_multiplier"`
	// DeadlineSeconds, when set (> 0), is the absolute per-work-unit hard deadline
	// in seconds and takes precedence over deadline_multiplier. It lets an operator
	// state a real deadline directly instead of relying on a multiplier applied to a
	// fixed baseline runtime estimate, so the resulting deadline can be matched to
	// how long a unit actually takes (and to how long volunteers may pause). Omitted
	// or <= 0 means "derive from deadline_multiplier". Ignored when NoDeadline.
	DeadlineSeconds *int `json:"deadline_seconds,omitempty"`
	// NoDeadline disables the hard wall-clock deadline for this leaf's work units
	// at the execution level: ResolveDeadlineSeconds stamps a large synthetic
	// reclaim ceiling (NoDeadlineCeilingSeconds, default 6h, operator-tunable via
	// head.no_deadline_ceiling_seconds) which the runtime treats as effectively no
	// timeout. With per-task heartbeats removed, liveness is purely deadline-based,
	// so the ceiling (rather than a literal 0) guarantees the head always reclaims a
	// unit whose volunteer vanished — FindExpiredWorkUnits covers it because
	// deadline_seconds > 0. Defaults to false (deadline enforced via
	// DeadlineMultiplier).
	NoDeadline                bool  `json:"no_deadline"`
	MaxReassignments          int   `json:"max_reassignments"`
	CheckpointingEnabled      bool  `json:"checkpointing_enabled"`
	CheckpointIntervalSeconds *int  `json:"checkpoint_interval_seconds"`
	MaxCheckpointSizeBytes    int64 `json:"max_checkpoint_size_bytes"`
}

// DefaultWorkUnitDurationSeconds is the baseline per-unit runtime (in seconds)
// that deadline_multiplier scales to derive a work-unit deadline when no explicit
// deadline_seconds is configured.
const DefaultWorkUnitDurationSeconds = 3600

// ResolveDeadlineSeconds returns the per-work-unit hard deadline implied by this
// fault-tolerance config, NOT accounting for NoDeadline (callers apply the
// NoDeadline reclaim ceiling separately). An explicit deadline_seconds (> 0) wins;
// otherwise the deadline is DefaultWorkUnitDurationSeconds * deadline_multiplier
// (multiplier floored at 1.0).
func (c FaultToleranceConfig) ResolveDeadlineSeconds() int {
	if c.DeadlineSeconds != nil && *c.DeadlineSeconds > 0 {
		return *c.DeadlineSeconds
	}
	multiplier := c.DeadlineMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return int(float64(DefaultWorkUnitDurationSeconds) * multiplier)
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
	// CurrentArtifactVersionID points at the leaf_artifact_versions row this leaf
	// currently dispatches (TODO #38). Nil = no published version yet: the legacy
	// path, where assignments build from ExecutionConfig directly. Owned by
	// ArtifactVersionRepository.SetCurrentVersion; never written by Update.
	CurrentArtifactVersionID *types.ID `json:"current_artifact_version_id,omitempty"`
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
