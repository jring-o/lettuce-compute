package leaf

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// binaryURLAllowInsecure mirrors the well-known LETTUCE_SIGNING_KEY_AUTOGEN dev
// escape hatch: when LETTUCE_BINARY_URL_ALLOW_INSECURE=true the binary URL SSRF
// rules (https-required, no loopback/private IPs, FQDN-required) are relaxed so
// local dev / integration tests can point leafs at a loopback http server.
// Defaults to false; production deployments must never set it. Production binaries
// reading the env var at parse time make this work for the standalone server
// binary too (where //go:build integration test files are not linked).
var binaryURLAllowInsecure = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LETTUCE_BINARY_URL_ALLOW_INSECURE")))
	return v == "1" || v == "true" || v == "yes"
}()

// validateBinaryURL checks that a binary URL uses HTTPS and does not point to
// private/internal addresses (SSRF prevention).
func validateBinaryURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if binaryURLAllowInsecure {
		if u.Hostname() == "" {
			return fmt.Errorf("missing hostname")
		}
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https scheme, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing hostname")
	}
	// Reject localhost and loopback.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return fmt.Errorf("localhost URLs are not allowed")
	}
	// Reject non-FQDN hostnames (no dots).
	if !strings.Contains(host, ".") {
		return fmt.Errorf("hostname must be a fully qualified domain name")
	}
	// Reject private IP ranges.
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("private/internal IP addresses are not allowed")
		}
	}
	return nil
}

// validateBinaryChecksums validates the format of every checksum present in
// c.BinaryChecksums. Each value must be a 64-char lowercase hex SHA-256 digest.
// It does not enforce presence — that is runtime-specific (required for NATIVE).
func validateBinaryChecksums(c *ExecutionConfig) *apierror.APIError {
	for platform, checksum := range c.BinaryChecksums {
		if !isValidSHA256Hex(checksum) {
			return apierror.ValidationError(
				fmt.Sprintf("binary_checksums[%q] must be a 64-character lowercase hex SHA-256 digest", platform),
				validationDetail{Field: "binary_checksums." + platform, Reason: "invalid_sha256"})
		}
	}
	return nil
}

// ociImageRefRegex validates OCI container image references.
// Supports: repo, repo:tag, registry/repo:tag, registry/repo@sha256:digest
var ociImageRefRegex = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[a-zA-Z0-9][\w.-]{0,127}|@sha256:[a-f0-9]{64})?$`)

// sha256HexRegex matches a lowercase hex SHA-256 digest (exactly 64 hex chars).
var sha256HexRegex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// isValidSHA256Hex reports whether s is a 64-character lowercase hex string.
func isValidSHA256Hex(s string) bool {
	return sha256HexRegex.MatchString(s)
}

// validationDetail provides structured field-level error information.
type validationDetail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// --- Default-filling functions ---

// ApplyExecutionConfigDefaults fills zero-value fields with documented defaults.
func ApplyExecutionConfigDefaults(c *ExecutionConfig) {
	if c.GPUType == "" {
		c.GPUType = GPUTypeAny
	}
	if c.MaxMemoryMB == 0 {
		c.MaxMemoryMB = 4096
	}
	if c.MaxDiskMB == 0 {
		c.MaxDiskMB = 10240
	}
	if c.MaxCPUSeconds == 0 {
		c.MaxCPUSeconds = 86400
	}
}

// ApplyValidationConfigDefaults fills zero-value fields with documented defaults.
func ApplyValidationConfigDefaults(c *ValidationConfig) {
	if c.RedundancyFactor == 0 {
		c.RedundancyFactor = 2
	}
	if c.AgreementThreshold == 0 {
		c.AgreementThreshold = 1.0
	}
	if c.ComparisonMode == "" {
		c.ComparisonMode = ComparisonExact
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.SpotCheckEnabled && c.SpotCheckPercentage == 0 {
		c.SpotCheckPercentage = 5.0
	}
}

// ApplyFaultToleranceConfigDefaults fills zero-value fields with documented defaults.
func ApplyFaultToleranceConfigDefaults(c *FaultToleranceConfig) {
	// HeartbeatIntervalSeconds and MissedHeartbeatsThreshold are deprecated/inert
	// (deadline-based reassignment replaced per-task heartbeats in v0.3.0). They no
	// longer drive anything; the defaults below are kept only so persisted JSON keeps
	// a stable shape for older callers that still read them.
	if c.HeartbeatIntervalSeconds == 0 {
		c.HeartbeatIntervalSeconds = 300
	}
	if c.MissedHeartbeatsThreshold == 0 {
		c.MissedHeartbeatsThreshold = 3
	}
	if c.DeadlineMultiplier == 0 {
		c.DeadlineMultiplier = 3.0
	}
	if c.MaxReassignments == 0 {
		c.MaxReassignments = 3
	}
	if c.MaxCheckpointSizeBytes == 0 {
		c.MaxCheckpointSizeBytes = 104857600 // 100 MB
	}
}

// ApplyDataConfigDefaults fills zero-value fields with documented defaults.
func ApplyDataConfigDefaults(c *DataConfig) {
	if c.TransferStrategy == "" {
		c.TransferStrategy = TransferInline
	}
	if c.AggregationFormat == "" {
		c.AggregationFormat = AggregationJSON
	}
	if c.MaxInputSizeBytes == 0 {
		c.MaxInputSizeBytes = 1048576
	}
	if c.MaxOutputSizeBytes == 0 {
		c.MaxOutputSizeBytes = 104857600
	}
	if c.GenerationMode == "" {
		c.GenerationMode = GenerationModeEager
	}
	if c.GenerationMode == GenerationModeLazy {
		if c.LazyThreshold == 0 {
			c.LazyThreshold = 100
		}
		if c.LazyBatchSize == 0 {
			c.LazyBatchSize = 1000
		}
	}
}

// ApplyCreditConfigDefaults fills zero-value fields with documented defaults.
func ApplyCreditConfigDefaults(c *CreditConfig) {
	if c.CreditPerValidatedWorkUnit == 0 {
		c.CreditPerValidatedWorkUnit = 1.0
	}
}

// ApplyResourceRequirementsDefaults fills zero-value fields with documented defaults.
func ApplyResourceRequirementsDefaults(r *ResourceRequirements) {
	if r.MinCPUCores == 0 {
		r.MinCPUCores = 1
	}
	if r.MinDiskMB == 0 {
		r.MinDiskMB = 1024
	}
}

// --- Validation functions ---

// ValidateMetadata validates leaf metadata fields.
func ValidateMetadata(p *Leaf) *apierror.APIError {
	// Name length: 3-100
	if len(p.Name) < 3 {
		return apierror.ValidationError("leaf name must be between 3 and 100 characters",
			validationDetail{Field: "name", Reason: "too_short"})
	}
	if len(p.Name) > 100 {
		return apierror.ValidationError("leaf name must be between 3 and 100 characters",
			validationDetail{Field: "name", Reason: "too_long"})
	}

	// Description length: 10-10,000
	if len(p.Description) < 10 {
		return apierror.ValidationError("leaf description must be between 10 and 10000 characters",
			validationDetail{Field: "description", Reason: "too_short"})
	}
	if len(p.Description) > 10000 {
		return apierror.ValidationError("leaf description must be between 10 and 10000 characters",
			validationDetail{Field: "description", Reason: "too_long"})
	}

	// Visibility must be valid
	switch p.Visibility {
	case VisibilityPublic, VisibilityUnlisted, VisibilityPrivate:
		// valid
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid visibility: %q; must be one of PUBLIC, UNLISTED, PRIVATE", p.Visibility),
			validationDetail{Field: "visibility", Reason: "invalid_value"})
	}

	// Research area required when PUBLIC
	if p.Visibility == VisibilityPublic && len(p.ResearchArea) == 0 {
		return apierror.ValidationError("research_area is required for public leafs",
			validationDetail{Field: "research_area", Reason: "required_for_public"})
	}

	// Task pattern must be valid and supported
	switch p.TaskPattern {
	case PatternParameterSweep:
		// supported
	case PatternMapReduce:
		// supported in Beta v0.8
	case PatternMonteCarlo:
		// supported in Beta v0.8
	case PatternCustom:
		// supported in Beta v0.8
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid task_pattern: %q; must be one of PARAMETER_SWEEP, MAP_REDUCE, MONTE_CARLO, CUSTOM", p.TaskPattern),
			validationDetail{Field: "task_pattern", Reason: "invalid_value"})
	}

	// Creator identity: exactly one of creator_id or creator_public_key
	hasCreatorID := p.CreatorID != nil && !types.IsNilID(*p.CreatorID)
	hasCreatorKey := len(p.CreatorPublicKey) > 0
	if hasCreatorID && hasCreatorKey {
		return apierror.ValidationError("creator_id and creator_public_key are mutually exclusive",
			validationDetail{Field: "creator_identity", Reason: "mutually_exclusive"})
	}
	if !hasCreatorID && !hasCreatorKey {
		return apierror.ValidationError("one of creator_id or creator_public_key is required",
			validationDetail{Field: "creator_identity", Reason: "required"})
	}

	return nil
}

// ValidateExecutionConfig validates execution configuration.
func ValidateExecutionConfig(c *ExecutionConfig) *apierror.APIError {
	// Runtime is required
	if c.Runtime == "" {
		return apierror.ValidationError("runtime is required",
			validationDetail{Field: "runtime", Reason: "required"})
	}

	// Runtime must be a known value
	switch c.Runtime {
	case RuntimeNative:
		// supported
	case RuntimeContainer:
		// supported (v0.7)
	case RuntimeWasm:
		// supported (v0.9.3)
	case RuntimeScript:
		return apierror.ValidationError(
			fmt.Sprintf("runtime %q is not yet supported; supported: NATIVE, CONTAINER, WASM", c.Runtime),
			validationDetail{Field: "runtime", Reason: "unsupported_runtime"})
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid runtime: %q; must be one of NATIVE, CONTAINER, WASM, SCRIPT", c.Runtime),
			validationDetail{Field: "runtime", Reason: "invalid_value"})
	}

	// Binaries required when runtime = NATIVE
	if c.Runtime == RuntimeNative {
		if len(c.Binaries) == 0 {
			return apierror.ValidationError("binaries is required when runtime is NATIVE",
				validationDetail{Field: "binaries", Reason: "required_for_native"})
		}
		for platform, binaryURL := range c.Binaries {
			if err := validateBinaryURL(binaryURL); err != nil {
				return apierror.ValidationError(
					fmt.Sprintf("binaries[%q]: %s", platform, err),
					validationDetail{Field: "binaries." + platform, Reason: "invalid_url"})
			}
			// Native code runs directly on volunteer hosts, so integrity is
			// mandatory: every binary must carry a SHA-256 checksum the
			// volunteer verifies before execution (fail-closed).
			checksum, ok := c.BinaryChecksums[platform]
			if !ok || checksum == "" {
				return apierror.ValidationError(
					fmt.Sprintf("binary_checksums[%q] is required when runtime is NATIVE (SHA-256 of the binary)", platform),
					validationDetail{Field: "binary_checksums." + platform, Reason: "required_for_native"})
			}
		}
	}

	// Validate the format of every provided checksum, regardless of runtime.
	// Must be a 64-char lowercase hex SHA-256 digest. Checksums are optional for
	// wasm/viz/container, but malformed entries are always rejected.
	if err := validateBinaryChecksums(c); err != nil {
		return err
	}

	// Container validation
	if c.Runtime == RuntimeContainer {
		if c.Image == nil && c.Dockerfile == nil {
			return apierror.ValidationError("image or dockerfile is required when runtime is CONTAINER",
				validationDetail{Field: "image", Reason: "required_for_container"})
		}
		if c.Image != nil && !ociImageRefRegex.MatchString(*c.Image) {
			return apierror.ValidationError(
				fmt.Sprintf("invalid OCI image reference: %q", *c.Image),
				validationDetail{Field: "image", Reason: "invalid_oci_reference"})
		}
	}

	// WASM validation
	if c.Runtime == RuntimeWasm {
		// Binaries["wasm"] is required
		wasmURL := c.Binaries["wasm"]
		if wasmURL == "" {
			return apierror.ValidationError("binaries[\"wasm\"] is required when runtime is WASM",
				validationDetail{Field: "binaries.wasm", Reason: "required_for_wasm"})
		}
		// Must end in .wasm
		if !strings.HasSuffix(wasmURL, ".wasm") {
			return apierror.ValidationError("binaries[\"wasm\"] must end in .wasm",
				validationDetail{Field: "binaries.wasm", Reason: "invalid_extension"})
		}
		// Validate URL
		if err := validateBinaryURL(wasmURL); err != nil {
			return apierror.ValidationError(
				fmt.Sprintf("binaries[\"wasm\"]: %s", err),
				validationDetail{Field: "binaries.wasm", Reason: "invalid_url"})
		}
		// No OCI image
		if c.Image != nil && *c.Image != "" {
			return apierror.ValidationError("image must be empty when runtime is WASM",
				validationDetail{Field: "image", Reason: "not_applicable_for_wasm"})
		}
		// No Dockerfile
		if c.Dockerfile != nil && *c.Dockerfile != "" {
			return apierror.ValidationError("dockerfile must be empty when runtime is WASM",
				validationDetail{Field: "dockerfile", Reason: "not_applicable_for_wasm"})
		}
		// No script fields
		if c.Language != nil && *c.Language != "" {
			return apierror.ValidationError("language must be empty when runtime is WASM",
				validationDetail{Field: "language", Reason: "not_applicable_for_wasm"})
		}
		if c.EntryPoint != nil && *c.EntryPoint != "" {
			return apierror.ValidationError("entry_point must be empty when runtime is WASM",
				validationDetail{Field: "entry_point", Reason: "not_applicable_for_wasm"})
		}
		if c.Dependencies != nil && *c.Dependencies != "" {
			return apierror.ValidationError("dependencies must be empty when runtime is WASM",
				validationDetail{Field: "dependencies", Reason: "not_applicable_for_wasm"})
		}
		// No network access (WASI preview 1 has no network APIs)
		if c.NetworkAccess {
			return apierror.ValidationError("network_access must be false when runtime is WASM (WASI has no network APIs)",
				validationDetail{Field: "network_access", Reason: "not_supported_for_wasm"})
		}
		// GPU type restriction: only WEBGPU or ANY for WASM
		if c.GPURequired {
			switch c.GPUType {
			case "", GPUTypeAny, GPUTypeWebGPU:
				// valid
			default:
				return apierror.ValidationError(
					fmt.Sprintf("WASM leafs only support gpu_type WEBGPU or ANY, got %q", c.GPUType),
					validationDetail{Field: "gpu_type", Reason: "invalid_for_wasm"})
			}
			// Warn if gpu_required but no wgsl shader (module might embed shaders)
			if c.Binaries["wgsl"] == "" {
				slog.Warn("WASM leaf has gpu_required=true but no binaries[\"wgsl\"] shader specified; the WASM module may embed shaders")
			}
		}
		// Warn if MaxMemoryMB > 4096 (WASM 32-bit linear memory limit)
		if c.MaxMemoryMB > 4096 {
			slog.Warn("WASM leaf max_memory_mb exceeds 4GB WASM linear memory limit", "max_memory_mb", c.MaxMemoryMB)
		}
	}

	// GPU tasks require container or WASM runtime (native binaries don't support GPU device passthrough)
	if c.GPURequired && c.Runtime == RuntimeNative {
		return apierror.ValidationError("GPU tasks require container or WASM runtime",
			validationDetail{Field: "runtime", Reason: "gpu_requires_container_or_wasm"})
	}

	// Script validation (for future use)
	if c.Runtime == RuntimeScript {
		if c.Language == nil || *c.Language == "" {
			return apierror.ValidationError("language is required when runtime is SCRIPT",
				validationDetail{Field: "language", Reason: "required_for_script"})
		}
		switch *c.Language {
		case LangPython, LangR, LangJulia:
			// valid
		default:
			return apierror.ValidationError(
				fmt.Sprintf("invalid language: %q; must be one of PYTHON, R, JULIA", *c.Language),
				validationDetail{Field: "language", Reason: "invalid_value"})
		}
		if c.EntryPoint == nil || *c.EntryPoint == "" {
			return apierror.ValidationError("entry_point is required when runtime is SCRIPT",
				validationDetail{Field: "entry_point", Reason: "required_for_script"})
		}
	}

	// GPU type validation (only relevant when gpu_required = true)
	if c.GPURequired {
		switch c.GPUType {
		case "":
			// Empty gpu_type is valid — ApplyExecutionConfigDefaults will set it to ANY.
		case GPUTypeAny, GPUTypeNvidia, GPUTypeAMD, GPUTypeWebGPU:
			// valid
		default:
			return apierror.ValidationError(
				fmt.Sprintf("invalid gpu_type: %q; must be one of ANY, NVIDIA, AMD, WEBGPU", c.GPUType),
				validationDetail{Field: "gpu_type", Reason: "invalid_value"})
		}
		if c.MinVRAMGB < 0 {
			return apierror.ValidationError("min_vram_gb must be non-negative",
				validationDetail{Field: "min_vram_gb", Reason: "must_be_non_negative"})
		}
	}

	// Resource limits must be positive
	if c.MaxMemoryMB <= 0 {
		return apierror.ValidationError("max_memory_mb must be a positive integer",
			validationDetail{Field: "max_memory_mb", Reason: "must_be_positive"})
	}
	if c.MaxDiskMB <= 0 {
		return apierror.ValidationError("max_disk_mb must be a positive integer",
			validationDetail{Field: "max_disk_mb", Reason: "must_be_positive"})
	}
	if c.MaxCPUSeconds <= 0 {
		return apierror.ValidationError("max_cpu_seconds must be a positive integer",
			validationDetail{Field: "max_cpu_seconds", Reason: "must_be_positive"})
	}

	return nil
}

// ValidateValidationConfig validates result validation configuration.
func ValidateValidationConfig(c *ValidationConfig) *apierror.APIError {
	// Redundancy factor: head-owned, no upper bound. Must be at least 1.
	if c.RedundancyFactor < 1 {
		return apierror.ValidationError("redundancy_factor must be at least 1",
			validationDetail{Field: "redundancy_factor", Reason: "out_of_range"})
	}

	// Agreement threshold: 0.0-1.0
	if c.AgreementThreshold < 0 || c.AgreementThreshold > 1 {
		return apierror.ValidationError("agreement_threshold must be between 0.0 and 1.0",
			validationDetail{Field: "agreement_threshold", Reason: "out_of_range"})
	}

	// Comparison mode
	switch c.ComparisonMode {
	case ComparisonExact, ComparisonNumericTolerance, ComparisonCustom:
		// valid
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid comparison_mode: %q; must be one of EXACT, NUMERIC_TOLERANCE, CUSTOM", c.ComparisonMode),
			validationDetail{Field: "comparison_mode", Reason: "invalid_value"})
	}

	// Numeric tolerance required when mode = NUMERIC_TOLERANCE
	if c.ComparisonMode == ComparisonNumericTolerance {
		if c.NumericTolerance == nil || *c.NumericTolerance <= 0 {
			return apierror.ValidationError("numeric_tolerance must be a positive number when comparison_mode is NUMERIC_TOLERANCE",
				validationDetail{Field: "numeric_tolerance", Reason: "required_and_positive"})
		}
	}

	// Custom comparator ref required when mode = CUSTOM
	if c.ComparisonMode == ComparisonCustom {
		if c.CustomComparatorRef == nil || *c.CustomComparatorRef == "" {
			return apierror.ValidationError("custom_comparator_ref is required when comparison_mode is CUSTOM",
				validationDetail{Field: "custom_comparator_ref", Reason: "required_for_custom"})
		}
	}

	// Max retries: 1-10
	if c.MaxRetries < 1 || c.MaxRetries > 10 {
		return apierror.ValidationError("max_retries must be between 1 and 10",
			validationDetail{Field: "max_retries", Reason: "out_of_range"})
	}

	// Spot-check validation
	if c.SpotCheckEnabled {
		if c.RedundancyFactor > 1 {
			return apierror.ValidationError(
				"spot-check validation is only for redundancy_factor=1 leafs; use redundancy_factor >= 2 instead",
				validationDetail{Field: "spot_check_enabled", Reason: "requires_redundancy_1"})
		}
		if c.SpotCheckPercentage < 1.0 || c.SpotCheckPercentage > 20.0 {
			return apierror.ValidationError(
				"spot_check_percentage must be between 1.0 and 20.0",
				validationDetail{Field: "spot_check_percentage", Reason: "out_of_range"})
		}
	}

	// Comparison field-path lists (ignore_fields / compare_fields): each entry must be a
	// non-empty, reasonably-bounded dotted JSON path. compare_fields only applies to
	// NUMERIC_TOLERANCE; ignore_fields applies to EXACT (canonical) and NUMERIC_TOLERANCE.
	if err := validateFieldPaths("ignore_fields", c.IgnoreFields); err != nil {
		return err
	}
	if err := validateFieldPaths("compare_fields", c.CompareFields); err != nil {
		return err
	}
	if len(c.CompareFields) > 0 && c.ComparisonMode != ComparisonNumericTolerance {
		return apierror.ValidationError(
			"compare_fields is only valid when comparison_mode is NUMERIC_TOLERANCE",
			validationDetail{Field: "compare_fields", Reason: "not_applicable_for_mode"})
	}

	return nil
}

// validateFieldPaths checks a list of output JSON field paths (ignore_fields /
// compare_fields): at most 256 entries, each a non-empty path <= 256 chars with no
// surrounding whitespace.
func validateFieldPaths(field string, paths []string) *apierror.APIError {
	if len(paths) > 256 {
		return apierror.ValidationError(
			fmt.Sprintf("%s may have at most 256 entries", field),
			validationDetail{Field: field, Reason: "too_many"})
	}
	for _, p := range paths {
		if p == "" || strings.TrimSpace(p) != p {
			return apierror.ValidationError(
				fmt.Sprintf("%s entries must be non-empty and not surrounded by whitespace", field),
				validationDetail{Field: field, Reason: "invalid_value"})
		}
		if len(p) > 256 {
			return apierror.ValidationError(
				fmt.Sprintf("%s entries must be at most 256 characters", field),
				validationDetail{Field: field, Reason: "too_long"})
		}
	}
	return nil
}

// ValidateFaultToleranceConfig validates fault tolerance configuration.
//
// heartbeat_interval_seconds and missed_heartbeats_threshold are DEPRECATED and
// INERT: v0.3.0 removed per-task heartbeats in favour of deadline-based
// reassignment (DeadlineMultiplier + the StartWork-stamped reclaim deadline), so
// these two fields no longer drive anything. They are intentionally not required
// or range-checked here — any value (including 0 / omitted) is accepted — but the
// struct fields are retained so older callers that still send them do not break.
func ValidateFaultToleranceConfig(c *FaultToleranceConfig) *apierror.APIError {
	// Deadline multiplier: head-owned, no upper bound. Must be positive.
	if c.DeadlineMultiplier <= 0 {
		return apierror.ValidationError("deadline_multiplier must be greater than 0",
			validationDetail{Field: "deadline_multiplier", Reason: "out_of_range"})
	}

	// Max reassignments: head-owned, no upper bound. Must be at least 1.
	if c.MaxReassignments < 1 {
		return apierror.ValidationError("max_reassignments must be at least 1",
			validationDetail{Field: "max_reassignments", Reason: "out_of_range"})
	}

	// Checkpoint validation: if enabled, interval must be >= 60 seconds.
	if c.CheckpointingEnabled {
		if c.CheckpointIntervalSeconds == nil || *c.CheckpointIntervalSeconds < 60 {
			return apierror.ValidationError("checkpoint_interval_seconds must be >= 60 when checkpointing is enabled",
				validationDetail{Field: "checkpoint_interval_seconds", Reason: "required_and_minimum_60"})
		}
	}

	// Max checkpoint size must be positive when set.
	if c.MaxCheckpointSizeBytes < 0 {
		return apierror.ValidationError("max_checkpoint_size_bytes must be non-negative",
			validationDetail{Field: "max_checkpoint_size_bytes", Reason: "must_be_non_negative"})
	}

	return nil
}

// ValidateDataConfig validates data transfer and splitting configuration.
func ValidateDataConfig(c *DataConfig, taskPattern TaskPattern) *apierror.APIError {
	// Transfer strategy
	switch c.TransferStrategy {
	case TransferInline, TransferExternalReference:
		// valid
	case TransferPlatformManaged:
		// PLATFORM_MANAGED is only available on the hosted platform; self-hosted
		// servers must manage transfer themselves via INLINE or EXTERNAL_REFERENCE.
		return apierror.ValidationError(
			"transfer_strategy PLATFORM_MANAGED is only available on the hosted platform; self-hosted servers must use INLINE or EXTERNAL_REFERENCE",
			validationDetail{Field: "transfer_strategy", Reason: "platform_managed_requires_hosted_platform"})
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid transfer_strategy: %q; must be one of INLINE, EXTERNAL_REFERENCE", c.TransferStrategy),
			validationDetail{Field: "transfer_strategy", Reason: "invalid_value"})
	}

	// External base URL required for EXTERNAL_REFERENCE
	if c.TransferStrategy == TransferExternalReference {
		if c.ExternalBaseURL == nil || *c.ExternalBaseURL == "" {
			return apierror.ValidationError("external_base_url is required when transfer_strategy is EXTERNAL_REFERENCE",
				validationDetail{Field: "external_base_url", Reason: "required_for_external_reference"})
		}
	}

	// Splitting strategy should be null for parameter_sweep
	if taskPattern == PatternParameterSweep && c.SplittingStrategy != nil {
		return apierror.ValidationError("splitting_strategy must be null for parameter_sweep task pattern",
			validationDetail{Field: "splitting_strategy", Reason: "not_applicable_for_parameter_sweep"})
	}

	// Monte Carlo: splitting_strategy must be nil (seed-based, not data-based).
	if taskPattern == PatternMonteCarlo {
		if c.SplittingStrategy != nil {
			return apierror.ValidationError("splitting_strategy must be null for monte_carlo task pattern (seed-based, not data-based)",
				validationDetail{Field: "splitting_strategy", Reason: "not_applicable_for_monte_carlo"})
		}
		if err := validateMonteCarloDataConfig(c); err != nil {
			return err
		}
	}

	// Custom: splitting_strategy must be nil (user handles splitting externally).
	if taskPattern == PatternCustom {
		if c.SplittingStrategy != nil {
			return apierror.ValidationError("splitting_strategy must be null for custom task pattern (user handles splitting externally)",
				validationDetail{Field: "splitting_strategy", Reason: "not_applicable_for_custom"})
		}
		if err := validateCustomAggregationConfig(c); err != nil {
			return err
		}
	}

	// Map-reduce requires splitting_strategy and validates splitting_config.
	if taskPattern == PatternMapReduce {
		if err := validateMapReduceDataConfig(c); err != nil {
			return err
		}
	}

	// Aggregation format
	switch c.AggregationFormat {
	case AggregationJSON, AggregationCSV, AggregationParquet, AggregationCustom:
		// valid
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid aggregation_format: %q; must be one of JSON, CSV, PARQUET, CUSTOM", c.AggregationFormat),
			validationDetail{Field: "aggregation_format", Reason: "invalid_value"})
	}

	// Size limits must be positive
	if c.MaxInputSizeBytes <= 0 {
		return apierror.ValidationError("max_input_size_bytes must be a positive integer",
			validationDetail{Field: "max_input_size_bytes", Reason: "must_be_positive"})
	}
	if c.MaxOutputSizeBytes <= 0 {
		return apierror.ValidationError("max_output_size_bytes must be a positive integer",
			validationDetail{Field: "max_output_size_bytes", Reason: "must_be_positive"})
	}

	// Lazy generation config validation.
	if err := validateLazyGenerationConfig(c, taskPattern); err != nil {
		return err
	}

	return nil
}

// validateLazyGenerationConfig validates generation_mode, lazy_threshold, and lazy_batch_size.
func validateLazyGenerationConfig(c *DataConfig, taskPattern TaskPattern) *apierror.APIError {
	switch c.GenerationMode {
	case GenerationModeEager, "":
		// valid (eager is default)
	case GenerationModeLazy:
		if taskPattern == PatternCustom {
			return apierror.ValidationError(
				"lazy generation is not supported for custom pattern; use the /work-units/bulk endpoint",
				validationDetail{Field: "generation_mode", Reason: "unsupported_for_custom"})
		}
		if c.LazyThreshold < 1 || c.LazyThreshold > 10000 {
			return apierror.ValidationError(
				"lazy_threshold must be between 1 and 10000",
				validationDetail{Field: "lazy_threshold", Reason: "out_of_range"})
		}
		if c.LazyBatchSize < 1 || c.LazyBatchSize > 100000 {
			return apierror.ValidationError(
				"lazy_batch_size must be between 1 and 100000",
				validationDetail{Field: "lazy_batch_size", Reason: "out_of_range"})
		}
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid generation_mode: %q; must be \"eager\" or \"lazy\"", c.GenerationMode),
			validationDetail{Field: "generation_mode", Reason: "invalid_value"})
	}
	return nil
}

// ValidateCreditConfig validates credit calculation configuration.
func ValidateCreditConfig(c *CreditConfig) *apierror.APIError {
	if c.CreditPerValidatedWorkUnit <= 0 {
		return apierror.ValidationError("credit_per_validated_work_unit must be a positive number",
			validationDetail{Field: "credit_per_validated_work_unit", Reason: "must_be_positive"})
	}
	return nil
}

// ValidateResourceRequirements validates minimum volunteer hardware requirements.
func ValidateResourceRequirements(r *ResourceRequirements) *apierror.APIError {
	if r.MinCPUCores < 1 {
		return apierror.ValidationError("min_cpu_cores must be at least 1",
			validationDetail{Field: "min_cpu_cores", Reason: "must_be_positive"})
	}
	if r.MinDiskMB <= 0 {
		return apierror.ValidationError("min_disk_mb must be a positive integer",
			validationDetail{Field: "min_disk_mb", Reason: "must_be_positive"})
	}
	if r.MinGPUVRAMMB < 0 {
		return apierror.ValidationError("min_gpu_vram_mb must be non-negative",
			validationDetail{Field: "min_gpu_vram_mb", Reason: "must_be_non_negative"})
	}
	if r.MinBandwidthMbps < 0 {
		return apierror.ValidationError("min_bandwidth_mbps must be non-negative",
			validationDetail{Field: "min_bandwidth_mbps", Reason: "must_be_non_negative"})
	}
	return nil
}

// validateMapReduceDataConfig validates data_config fields specific to map-reduce leafs.
func validateMapReduceDataConfig(c *DataConfig) *apierror.APIError {
	if c.SplittingStrategy == nil || *c.SplittingStrategy == "" {
		return apierror.ValidationError(
			"splitting_strategy is required for map_reduce task pattern; must be one of by_line_count, by_byte_size, by_record",
			validationDetail{Field: "splitting_strategy", Reason: "required_for_map_reduce"})
	}

	switch *c.SplittingStrategy {
	case "by_line_count":
		return validateByLineCountConfig(c.SplittingConfig)
	case "by_byte_size":
		return validateByByteSizeConfig(c.SplittingConfig)
	case "by_record":
		return validateByRecordConfig(c.SplittingConfig)
	default:
		return apierror.ValidationError(
			fmt.Sprintf("invalid splitting_strategy: %q; must be one of by_line_count, by_byte_size, by_record", *c.SplittingStrategy),
			validationDetail{Field: "splitting_strategy", Reason: "invalid_value"})
	}
}

func validateByLineCountConfig(config map[string]any) *apierror.APIError {
	if config == nil {
		return nil // defaults will be used
	}
	if v, ok := config["lines_per_chunk"]; ok {
		n, err := toFloat64ForValidation(v)
		if err != nil || n < 1 || n > 1_000_000 {
			return apierror.ValidationError(
				"lines_per_chunk must be an integer between 1 and 1000000",
				validationDetail{Field: "splitting_config.lines_per_chunk", Reason: "out_of_range"})
		}
	}
	return nil
}

func validateByByteSizeConfig(config map[string]any) *apierror.APIError {
	if config == nil {
		return nil
	}
	if v, ok := config["bytes_per_chunk"]; ok {
		n, err := toFloat64ForValidation(v)
		if err != nil || n < 1024 || n > 1_073_741_824 {
			return apierror.ValidationError(
				"bytes_per_chunk must be an integer between 1024 and 1073741824",
				validationDetail{Field: "splitting_config.bytes_per_chunk", Reason: "out_of_range"})
		}
	}
	return nil
}

func validateByRecordConfig(config map[string]any) *apierror.APIError {
	if config == nil {
		return nil
	}
	if v, ok := config["record_delimiter"]; ok {
		s, isStr := v.(string)
		if !isStr || s == "" {
			return apierror.ValidationError(
				"record_delimiter must be a non-empty string",
				validationDetail{Field: "splitting_config.record_delimiter", Reason: "invalid_value"})
		}
	}
	if v, ok := config["records_per_chunk"]; ok {
		n, err := toFloat64ForValidation(v)
		if err != nil || n < 1 || n > 1_000_000 {
			return apierror.ValidationError(
				"records_per_chunk must be an integer between 1 and 1000000",
				validationDetail{Field: "splitting_config.records_per_chunk", Reason: "out_of_range"})
		}
	}
	return nil
}

// validateMonteCarloDataConfig validates data_config fields specific to Monte Carlo leafs.
func validateMonteCarloDataConfig(c *DataConfig) *apierror.APIError {
	if c.SplittingConfig != nil {
		// num_trials: required, 1-10,000,000
		if v, ok := c.SplittingConfig["num_trials"]; ok {
			n, err := toFloat64ForValidation(v)
			if err != nil || n < 1 || n > 10_000_000 {
				return apierror.ValidationError(
					"num_trials must be an integer between 1 and 10000000",
					validationDetail{Field: "splitting_config.num_trials", Reason: "out_of_range"})
			}
		}

		// seed_strategy: optional, must be "sequential" or "hash"
		if v, ok := c.SplittingConfig["seed_strategy"]; ok {
			s, isStr := v.(string)
			if !isStr {
				return apierror.ValidationError(
					"seed_strategy must be a string",
					validationDetail{Field: "splitting_config.seed_strategy", Reason: "invalid_type"})
			}
			switch s {
			case "sequential", "hash":
				// valid
			default:
				return apierror.ValidationError(
					fmt.Sprintf("invalid seed_strategy: %q; must be \"sequential\" or \"hash\"", s),
					validationDetail{Field: "splitting_config.seed_strategy", Reason: "invalid_value"})
			}
		}

		// seed_offset: optional, must be >= 0
		if v, ok := c.SplittingConfig["seed_offset"]; ok {
			n, err := toFloat64ForValidation(v)
			if err != nil || n < 0 {
				return apierror.ValidationError(
					"seed_offset must be a non-negative integer",
					validationDetail{Field: "splitting_config.seed_offset", Reason: "must_be_non_negative"})
			}
		}
	}

	// aggregation_config validation
	if c.AggregationConfig != nil {
		if v, ok := c.AggregationConfig["aggregator_type"]; ok {
			s, isStr := v.(string)
			if !isStr {
				return apierror.ValidationError(
					"aggregator_type must be a string",
					validationDetail{Field: "aggregation_config.aggregator_type", Reason: "invalid_type"})
			}
			switch s {
			case "mean", "variance", "confidence_interval", "all":
				// valid
			default:
				return apierror.ValidationError(
					fmt.Sprintf("invalid aggregator_type: %q; must be one of mean, variance, confidence_interval, all", s),
					validationDetail{Field: "aggregation_config.aggregator_type", Reason: "invalid_value"})
			}
		}

		if v, ok := c.AggregationConfig["confidence_level"]; ok {
			n, err := toFloat64ForValidation(v)
			if err != nil || n < 0.5 || n > 0.999 {
				return apierror.ValidationError(
					"confidence_level must be between 0.5 and 0.999",
					validationDetail{Field: "aggregation_config.confidence_level", Reason: "out_of_range"})
			}
		}
	}

	return nil
}

// validateCustomAggregationConfig validates aggregation_config fields specific to custom leafs.
func validateCustomAggregationConfig(c *DataConfig) *apierror.APIError {
	if c.AggregationConfig == nil {
		return nil
	}
	if v, ok := c.AggregationConfig["reducer_type"]; ok {
		if v == nil {
			return nil // null is valid (no auto-aggregation)
		}
		s, isStr := v.(string)
		if !isStr {
			return apierror.ValidationError(
				"reducer_type must be a string",
				validationDetail{Field: "aggregation_config.reducer_type", Reason: "invalid_type"})
		}
		switch s {
		case "sum", "average", "concatenate", "merge":
			// valid
		default:
			return apierror.ValidationError(
				fmt.Sprintf("invalid reducer_type: %q; must be one of sum, average, concatenate, merge", s),
				validationDetail{Field: "aggregation_config.reducer_type", Reason: "invalid_value"})
		}
	}
	return nil
}

// toFloat64ForValidation converts a JSON-decoded number to float64 for validation.
func toFloat64ForValidation(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
