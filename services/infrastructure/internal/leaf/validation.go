package leaf

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/netguard"
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
	// Reject internal IP ranges. Delegate the whole classification to netguard, so
	// this covers 0.0.0.0/unspecified (BG-14), CGNAT 100.64/10, NAT64, multicast and
	// both IPv4/IPv6 forms — a strict superset of the old IsPrivate/IsLoopback/
	// IsLinkLocalUnicast check. This screens IP LITERALS only; a hostname that
	// RESOLVES to an internal address (or DNS rebinding) is closed at connect time by
	// the volunteer daemon's netguard dial screen, which is the load-bearing layer.
	if ip := net.ParseIP(host); ip != nil {
		if reason := netguard.DisallowedIPReason(ip); reason != "" {
			return fmt.Errorf("internal IP addresses are not allowed (%s)", reason)
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

// validateImageRegistryHost rejects a container image reference whose registry
// authority is an internal address (BG-14d). The registry is the first
// '/'-separated component, but only when it looks like a registry authority — it
// contains '.' or ':' (a domain or host:port) or is "localhost". A bare first
// component (e.g. "ubuntu", "library/ubuntu") means the default public registry
// (docker.io), which has nothing internal to screen. This screens IP LITERALS
// only; a registry hostname that RESOLVES to an internal address is closed by the
// daemon's pre-pull screen, mirroring validateBinaryURL's head/daemon split.
func validateImageRegistryHost(imageRef string) error {
	if binaryURLAllowInsecure {
		return nil // local-dev registries (localhost:5000) are allowed in this mode
	}
	slash := strings.IndexByte(imageRef, '/')
	if slash < 0 {
		return nil // no registry component — single-name image on the default registry
	}
	first := imageRef[:slash]
	if first != "localhost" && !strings.ContainsAny(first, ".:") {
		return nil // a plain path component, not a registry authority
	}
	// first is the registry authority: "host", "host:port", or "[ipv6]:port".
	host := first
	if h, _, err := net.SplitHostPort(first); err == nil {
		host = h
	} else if strings.HasPrefix(first, "[") && strings.HasSuffix(first, "]") {
		host = first[1 : len(first)-1] // bracketed IPv6 literal with no port
	}
	if host == "localhost" {
		return fmt.Errorf("registry host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if reason := netguard.DisallowedIPReason(ip); reason != "" {
			return fmt.Errorf("registry host %s points at an internal address (%s)", ip, reason)
		}
	}
	return nil
}

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

	// SSRF defense-in-depth (BG-14 / ★BG-14c): screen EVERY leaf-supplied URL in
	// Binaries, for EVERY runtime — the platform binaries, the "wasm" module, AND the
	// "viz"/"wgsl" bundle URLs, which were previously screened only for NATIVE leaves
	// (the native loop below) and left unscreened for WASM/CONTAINER. Validating the
	// whole map here means a newly added key can never slip through unscreened. This
	// is head-side defense-in-depth; the volunteer daemon's connect-time netguard
	// screen remains the load-bearing layer against hostname-resolves-to-internal.
	for key, binURL := range c.Binaries {
		if binURL == "" {
			continue
		}
		if err := validateBinaryURL(binURL); err != nil {
			return apierror.ValidationError(
				fmt.Sprintf("binaries[%q]: %s", key, err),
				validationDetail{Field: "binaries." + key, Reason: "invalid_url"})
		}
	}

	// Binaries required when runtime = NATIVE
	if c.Runtime == RuntimeNative {
		if len(c.Binaries) == 0 {
			return apierror.ValidationError("binaries is required when runtime is NATIVE",
				validationDetail{Field: "binaries", Reason: "required_for_native"})
		}
		for platform := range c.Binaries {
			// URLs are already screened by the SSRF loop above. Native code runs
			// directly on volunteer hosts, so integrity is mandatory too: every
			// binary must carry a SHA-256 checksum the volunteer verifies before
			// execution (fail-closed).
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
		if c.Image != nil {
			if !ociImageRefRegex.MatchString(*c.Image) {
				return apierror.ValidationError(
					fmt.Sprintf("invalid OCI image reference: %q", *c.Image),
					validationDetail{Field: "image", Reason: "invalid_oci_reference"})
			}
			// BG-14d: the container image pull egresses through the Docker/Podman engine,
			// OUTSIDE the volunteer daemon's netguard dial screen, so a registry authority
			// like 169.254.169.254/repo would let the engine dial cloud metadata. Screen
			// the registry host here (IP literals) as the head-side layer; a registry
			// hostname that RESOLVES to an internal address is closed by the daemon's
			// pre-pull screen, same defense-in-depth split as validateBinaryURL (BG-14).
			if err := validateImageRegistryHost(*c.Image); err != nil {
				return apierror.ValidationError(
					fmt.Sprintf("image registry: %s", err),
					validationDetail{Field: "image", Reason: "registry_internal_address"})
			}
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
		// The URL itself is SSRF-screened by the Binaries loop above.
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

// RejectRemovedValidationConfigKeys returns a ValidationError when raw — the caller's
// validation_config JSON block — carries a field that has been removed from ValidationConfig.
// It must read the RAW bytes: the typed ValidationConfig no longer has the field, and leaf
// create/update decodes validation_config with a plain json.Unmarshal (no DisallowUnknownFields),
// so an unknown key is silently dropped before any typed validation could see it. Callers pass
// the raw block through this before the typed merge.
//
// The probe is KEY-PRESENCE over map[string]json.RawMessage, deliberately not a typed pointer
// (hardening note (d)): a typed probe like `*int` fails the whole unmarshal on a type-mismatched
// value ({"max_success_copies":"5"}) and the guard would fail OPEN — key silently dropped, 200
// returned, accepted-and-ignored (the exact E1-C state this guard exists to prevent). Presence
// of the key rejects regardless of its value's type, null included: whatever the value, the
// client asked for a field that no longer exists.
//
// max_success_copies was removed in migration 00025: a success ceiling has no coherent semantics
// (design §4.9) and was read by nothing, so accepting it would be dishonest config surface. raw
// may be empty (no block supplied), which rejects nothing; a malformed block is surfaced by the
// caller's own typed unmarshal, so a parse error here is ignored rather than double-reported.
func RejectRemovedValidationConfigKeys(raw json.RawMessage) *apierror.APIError {
	if len(raw) == 0 {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil
	}
	if _, present := keys["max_success_copies"]; present {
		return apierror.ValidationError(
			"max_success_copies is no longer supported; success ceilings have no coherent semantics (see release notes)",
			validationDetail{Field: "max_success_copies", Reason: "removed"})
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

	// Explicit target/quorum split (TODO #50). Each is optional (0 = derive from
	// redundancy_factor); when set it must be >= 1, and the resolved min_quorum must not
	// exceed the resolved target_copies. Validating the RESOLVED values (not the raw
	// fields) means a leaf that sets only target_copies, or only min_quorum, or only
	// redundancy_factor, is all checked consistently against the same effective numbers.
	if c.TargetCopies < 0 {
		return apierror.ValidationError("target_copies must be at least 1",
			validationDetail{Field: "target_copies", Reason: "out_of_range"})
	}
	if c.MinQuorum < 0 {
		return apierror.ValidationError("min_quorum must be at least 1",
			validationDetail{Field: "min_quorum", Reason: "out_of_range"})
	}
	effTarget := c.EffectiveTargetCopies()
	effQuorum := c.MinQuorum
	if effQuorum <= 0 {
		effQuorum = c.RedundancyFactor
	}
	if effQuorum > effTarget {
		return apierror.ValidationError(
			"min_quorum must be less than or equal to target_copies",
			validationDetail{Field: "min_quorum", Reason: "exceeds_target"})
	}

	// Hard caps (TODO #50, reconciling #40). Each optional (0 = default); when set, a cap
	// below the redundancy it bounds would make the unit un-validatable, so reject it.
	if c.MaxTotalCopies < 0 {
		return apierror.ValidationError("max_total_copies must be at least target_copies",
			validationDetail{Field: "max_total_copies", Reason: "out_of_range"})
	}
	if c.MaxTotalCopies > 0 && c.MaxTotalCopies < effTarget {
		return apierror.ValidationError(
			"max_total_copies must be at least target_copies (a lower ceiling can never reach redundancy)",
			validationDetail{Field: "max_total_copies", Reason: "below_target"})
	}
	if c.MaxErrorCopies < 0 {
		return apierror.ValidationError("max_error_copies must be at least 1",
			validationDetail{Field: "max_error_copies", Reason: "out_of_range"})
	}
	// Error cap floor (design §4.9, BG-27): a cap below target_copies can be tripped by honest
	// churn alone (one expiry per target slot) — that is never what a poison-unit-stopping owner
	// means, so reject it and keep the stored value honest rather than silently ineffective. The
	// cap stays opt-in (0 = unlimited). This mirrors the max_total_copies floor above.
	if c.MaxErrorCopies > 0 && c.MaxErrorCopies < effTarget {
		return apierror.ValidationError(
			"max_error_copies must be at least target_copies (a lower cap can be tripped by honest churn alone)",
			validationDetail{Field: "max_error_copies", Reason: "below_target"})
	}

	// Agreement threshold: 0.0-1.0.
	if c.AgreementThreshold < 0 || c.AgreementThreshold > 1 {
		return apierror.ValidationError("agreement_threshold must be between 0.0 and 1.0",
			validationDetail{Field: "agreement_threshold", Reason: "out_of_range"})
	}
	// On a redundant leaf (effective target_copies >= 2) a threshold of 0.5 or below would let
	// a minority or bare plurality of results validate a unit, so require a strict majority.
	// Exactly 0 is the "unset" sentinel (ApplyValidationConfigDefaults rewrites it to 1.0);
	// leave it to that path so the sentinel keeps working.
	if effTarget >= 2 && c.AgreementThreshold > 0 && c.AgreementThreshold <= 0.5 {
		return apierror.ValidationError(
			"agreement_threshold must be greater than 0.5 for redundant leafs (target_copies >= 2); a lower threshold would let a minority of results validate a unit",
			validationDetail{Field: "agreement_threshold", Reason: "not_strict_majority"})
	}

	// Trust gate per-leaf overrides (see internal/trust). Both are optional (0 = inherit
	// the head default) and must be non-negative. A required distinct-trusted-corroborator
	// count above the effective quorum size would defeat validate-at-quorum: a quorum-sized
	// agreeing group contains at most min_quorum distinct subjects, so the unit could not
	// validate until extra copies arrived — and never when target equals quorum. The policy
	// resolution also clamps to min_quorum at runtime; rejecting the config here keeps the
	// stored value honest rather than silently clamped.
	if c.MinTrustedCorroborators < 0 {
		return apierror.ValidationError("min_trusted_corroborators must be >= 0",
			validationDetail{Field: "min_trusted_corroborators", Reason: "out_of_range"})
	}
	if c.TrustFloor < 0 {
		return apierror.ValidationError("trust_floor must be >= 0",
			validationDetail{Field: "trust_floor", Reason: "out_of_range"})
	}
	if c.MinTrustedCorroborators > 0 && c.MinTrustedCorroborators > c.EffectiveMinQuorum() {
		return apierror.ValidationError(
			"min_trusted_corroborators must be <= min_quorum (a quorum cannot contain more distinct trusted subjects than its size)",
			validationDetail{Field: "min_trusted_corroborators", Reason: "exceeds_quorum"})
	}

	// Comparison mode. CUSTOM is a recognized mode but has no runtime comparator (the engine
	// errors on it and the unit would park forever), so it is rejected here until implemented.
	switch c.ComparisonMode {
	case ComparisonExact, ComparisonNumericTolerance:
		// valid
	case ComparisonCustom:
		return apierror.ValidationError(
			"comparison_mode CUSTOM is not yet supported; use EXACT or NUMERIC_TOLERANCE",
			validationDetail{Field: "comparison_mode", Reason: "not_yet_supported"})
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
		// Comparison scoping required whenever results are actually compared (PB-10).
		// The NUMERIC_TOLERANCE comparator flattens the ENTIRE output JSON and compares
		// every leaf, so an unscoped config makes honest volunteers DISAGREE whenever
		// nondeterministic runtime metadata (a wall-clock field like compute_time_ms)
		// differs between them — proven live in the Phase 3 campaign: 3/6 units on the
		// guide's own walkthrough leaf were rejected purely on a 7-vs-8ms timing field,
		// wasting the redundant compute and re-dispatching futilely. A refusal at
		// configure time is the only enforcement that reaches the leaf author before
		// volunteers burn compute; a silent default (e.g. the aggregation output_field)
		// was rejected because it would NARROW validation to one field behind the
		// author's back — un-compared fields would validate no matter what they carry.
		// Only refused when redundant comparison can occur: target_copies >= 2, or
		// spot-check (which forces a 2-of-2 corroboration on a single-copy leaf).
		if (effTarget >= 2 || c.SpotCheckEnabled) && len(c.CompareFields) == 0 && len(c.IgnoreFields) == 0 {
			return apierror.ValidationError(
				"NUMERIC_TOLERANCE on a redundant leaf requires compare_fields or ignore_fields: the comparator flattens the entire output JSON, so nondeterministic fields (e.g. timing metadata like compute_time_ms) make honest results disagree. Set compare_fields to the science output (recommended), or ignore_fields for the nondeterministic fields.",
				validationDetail{Field: "compare_fields", Reason: "comparison_scope_required"})
		}
	}

	// (CUSTOM mode, and therefore custom_comparator_ref, is rejected above until a runtime
	// comparator exists.)

	// Max retries: 1-10
	if c.MaxRetries < 1 || c.MaxRetries > 10 {
		return apierror.ValidationError("max_retries must be between 1 and 10",
			validationDetail{Field: "max_retries", Reason: "out_of_range"})
	}

	// Spot-check validation
	if c.SpotCheckEnabled {
		// Spot-check forces a corroborator (effective target/quorum 2) only on a
		// single-copy leaf, so it is valid only when the resolved target is 1 (== the old
		// redundancy_factor==1 rule, now also rejecting an explicit target_copies > 1).
		if c.EffectiveTargetCopies() > 1 {
			return apierror.ValidationError(
				"spot-check validation is only for single-copy leafs (target_copies/redundancy_factor = 1); use target_copies >= 2 instead",
				validationDetail{Field: "spot_check_enabled", Reason: "requires_redundancy_1"})
		}
		if c.SpotCheckPercentage < 1.0 || c.SpotCheckPercentage > 20.0 {
			return apierror.ValidationError(
				"spot_check_percentage must be between 1.0 and 20.0",
				validationDetail{Field: "spot_check_percentage", Reason: "out_of_range"})
		}
	}

	// Post-hoc audit-rate override: a fraction in [0, 1]. 0 = no override. The effective
	// rate is max(this, head default) — raise-only, so no bound below the head rate needs
	// rejecting here; the overlay makes a low value simply inert.
	if c.AuditRate < 0 || c.AuditRate > 1 || math.IsNaN(c.AuditRate) || math.IsInf(c.AuditRate, 0) {
		return apierror.ValidationError("audit_rate must be a fraction between 0 and 1",
			validationDetail{Field: "audit_rate", Reason: "out_of_range"})
	}

	// External output URL allowlist (D10, design §10.3). The opt-in, the comparison mode,
	// and the allowlist shape are cross-checked here so both create and update are covered
	// (the update handler re-merges + re-validates). An opted-in leaf MUST declare a
	// non-empty host allowlist (an empty list matches nothing at submit, so the leaf could
	// never accept a reference); NUMERIC_TOLERANCE is refused because reference bytes are
	// hashed and discarded, leaving nothing to compare numerically; and an allowlist without
	// the opt-in is vestigial config, refused under the alpha no-vestigial policy.
	if c.AllowExternalOutput {
		if len(c.ExternalOutputHosts) == 0 {
			return apierror.ValidationError(
				"allow_external_output requires a non-empty external_output_hosts allowlist",
				validationDetail{Field: "external_output_hosts", Reason: "required_non_empty"})
		}
		if c.ComparisonMode == ComparisonNumericTolerance {
			return apierror.ValidationError(
				"allow_external_output is not permitted with comparison_mode NUMERIC_TOLERANCE: external reference bytes are hashed and discarded, so numeric comparison is impossible",
				validationDetail{Field: "allow_external_output", Reason: "incompatible_with_numeric_tolerance"})
		}
		if err := validateExternalOutputHosts(c.ExternalOutputHosts); err != nil {
			return err
		}
	} else if len(c.ExternalOutputHosts) > 0 {
		return apierror.ValidationError(
			"external_output_hosts requires allow_external_output to be set (no vestigial config)",
			validationDetail{Field: "external_output_hosts", Reason: "requires_opt_in"})
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

// validateExternalOutputHosts checks the external output URL host allowlist (D10,
// design §10.3): 1-16 entries, each a bare lowercase FQDN. Exact-match semantics
// (no subdomain wildcarding), so entries must already be lowercase — the submit and
// fetch gates compare a lowercased URL host against them with no further normalization.
func validateExternalOutputHosts(hosts []string) *apierror.APIError {
	if len(hosts) < 1 || len(hosts) > 16 {
		return apierror.ValidationError(
			"external_output_hosts must have between 1 and 16 entries",
			validationDetail{Field: "external_output_hosts", Reason: "out_of_range"})
	}
	for _, h := range hosts {
		if err := validateExternalOutputHost(h); err != nil {
			return err
		}
	}
	return nil
}

// validateExternalOutputHost checks a single allowlist entry is a bare lowercase
// FQDN: <= 253 chars, >= 2 non-empty dot-separated labels of [a-z0-9-] with no
// hyphen at a label boundary, and carrying no scheme, port, path, wildcard,
// userinfo, whitespace, or IP literal (v4 or v6). All of those are refusable now and
// relaxable later; an IP literal in particular would sidestep the FQDN posture the
// fetch client's dial guard relies on.
func validateExternalOutputHost(h string) *apierror.APIError {
	reject := func(reason string) *apierror.APIError {
		return apierror.ValidationError(
			fmt.Sprintf("external_output_hosts entry %q must be a bare lowercase FQDN (no scheme, port, path, wildcard, userinfo, or IP literal)", h),
			validationDetail{Field: "external_output_hosts", Reason: reason})
	}
	if h == "" {
		return reject("empty_entry")
	}
	if len(h) > 253 {
		return reject("too_long")
	}
	if strings.ToLower(h) != h {
		return reject("not_lowercase")
	}
	// Any URL structure or userinfo marker (scheme/port ":", path "/", wildcard "*",
	// userinfo "@", whitespace) disqualifies a bare FQDN. The ":" also catches every
	// IPv6 literal, bracketed or not.
	if strings.ContainsAny(h, "/:*@ \t\r\n") {
		return reject("not_bare_fqdn")
	}
	// An IPv4 dotted-quad passes the label checks below (all-numeric labels are valid
	// [a-z0-9-]), so refuse IP literals explicitly. IPv6 is already refused by the ":".
	if net.ParseIP(h) != nil {
		return reject("ip_literal")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return reject("not_fqdn")
	}
	for _, label := range labels {
		if label == "" {
			return reject("empty_label")
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return reject("hyphen_boundary")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return reject("invalid_char")
			}
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

	// Explicit absolute deadline, when provided, must be positive. Use no_deadline
	// for "no hard deadline" rather than a zero/negative deadline_seconds.
	if c.DeadlineSeconds != nil && *c.DeadlineSeconds <= 0 {
		return apierror.ValidationError("deadline_seconds must be greater than 0 when set; use no_deadline for no hard deadline",
			validationDetail{Field: "deadline_seconds", Reason: "must_be_positive"})
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

// DeadlineAdequacyWarnings reports non-fatal concerns about a leaf's work-unit
// deadline relative to how long a unit is allowed to run. It returns
// human-readable warnings (empty when the deadline is adequate). These are
// advisory only — they never block activation — but they surface the most common
// footgun: a deadline shorter than the unit's own CPU budget, which guarantees a
// slow or paused volunteer loses its finished work to deadline reassignment.
//
// A no_deadline leaf has no hard deadline (its units run under the head reclaim
// ceiling), so it is never flagged.
func DeadlineAdequacyWarnings(p *Leaf) []string {
	if p.FaultToleranceConfig.NoDeadline {
		return nil
	}
	var warnings []string
	deadline := p.FaultToleranceConfig.ResolveDeadlineSeconds()
	maxRun := p.ExecutionConfig.MaxCPUSeconds
	if maxRun > 0 && deadline < maxRun {
		warnings = append(warnings, fmt.Sprintf(
			"work-unit deadline (%ds) is shorter than max_cpu_seconds (%ds): a unit that uses its full CPU budget cannot be returned before the deadline and will be reassigned. Set a longer fault_tolerance_config.deadline_seconds (or deadline_multiplier), or a smaller execution_config.max_cpu_seconds.",
			deadline, maxRun))
	}
	return warnings
}

// ValidateDataConfig validates data transfer and splitting configuration. isOngoing is the
// leaf's ongoing flag, needed to enforce the lazy-generation config-honesty rules (a finite —
// non-ongoing — lazy Monte Carlo leaf must declare its total; design §4.6, E1-C).
func ValidateDataConfig(c *DataConfig, taskPattern TaskPattern, isOngoing bool) *apierror.APIError {
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
		// SSRF defense-in-depth (BG-14 / ★BG-14c): the volunteer fetches input data
		// from this base URL, so screen it the same way as binary/module URLs — https,
		// FQDN, and no internal IP literal. The daemon's connect-time screen remains
		// the load-bearing layer against a hostname that resolves to an internal IP.
		if err := validateBinaryURL(*c.ExternalBaseURL); err != nil {
			return apierror.ValidationError(
				fmt.Sprintf("external_base_url: %s", err),
				validationDetail{Field: "external_base_url", Reason: "invalid_url"})
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
	if err := validateLazyGenerationConfig(c, taskPattern, isOngoing); err != nil {
		return err
	}

	return nil
}

// validateLazyGenerationConfig validates generation_mode, lazy_threshold, and lazy_batch_size,
// and enforces the lazy-generation config-honesty rules: map-reduce cannot be lazy (§4.10), and a
// finite lazy Monte Carlo leaf must declare its total (§4.6) so "finite" is falsifiable (E1-C).
func validateLazyGenerationConfig(c *DataConfig, taskPattern TaskPattern, isOngoing bool) *apierror.APIError {
	switch c.GenerationMode {
	case GenerationModeEager, "":
		// valid (eager is default)
	case GenerationModeLazy:
		if taskPattern == PatternCustom {
			return apierror.ValidationError(
				"lazy generation is not supported for custom pattern; use the /work-units/bulk endpoint",
				validationDetail{Field: "generation_mode", Reason: "unsupported_for_custom"})
		}
		// Map-reduce is eager-only (design §4.10, BG-22b): the full input is present at leaf
		// creation, so there is no not-yet-known tail for laziness to defer.
		if taskPattern == PatternMapReduce {
			return apierror.ValidationError(
				"lazy generation is not supported for MAP_REDUCE leaves: the full input is present at creation, so there is no not-yet-known tail for laziness to defer; use eager generation",
				validationDetail{Field: "generation_mode", Reason: "unsupported_for_map_reduce"})
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
		// A finite (non-ongoing) lazy Monte Carlo leaf must declare splitting_config.num_trials
		// (>= 1): it is the target N against which exhaustion is decided. Without it, "finite" is
		// unfalsifiable — the leaf would never exhaust (design §4.6, E1-C). Ongoing MC leaves
		// legitimately never exhaust and need no total.
		if taskPattern == PatternMonteCarlo && !isOngoing {
			n, ok := monteCarloNumTrials(c)
			if !ok || n < 1 {
				return apierror.ValidationError(
					"splitting_config.num_trials (>= 1) is required for a finite (non-ongoing) lazy monte_carlo leaf: it is the total against which generation exhaustion is decided",
					validationDetail{Field: "splitting_config.num_trials", Reason: "required_for_finite_lazy_monte_carlo"})
			}
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

// monteCarloNumTrials reads splitting_config.num_trials as an int, reporting whether it is present
// and numeric. Used by the finite-lazy-MC config-honesty check (design §4.6).
func monteCarloNumTrials(c *DataConfig) (int, bool) {
	if c.SplittingConfig == nil {
		return 0, false
	}
	v, ok := c.SplittingConfig["num_trials"]
	if !ok {
		return 0, false
	}
	n, err := toFloat64ForValidation(v)
	if err != nil {
		return 0, false
	}
	return int(n), true
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
