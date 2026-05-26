package leaf

import (
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func assertValidationResult(t *testing.T, err *apierror.APIError, wantErr bool, errMsg string) {
	t.Helper()
	if wantErr && err == nil {
		t.Fatal("expected error, got nil")
	}
	if !wantErr && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wantErr && err != nil && errMsg != "" {
		if !strings.Contains(strings.ToLower(err.Message), strings.ToLower(errMsg)) {
			t.Errorf("error message %q should contain %q", err.Message, errMsg)
		}
	}
}

// --- Metadata Validation Tests ---

func TestValidateMetadata(t *testing.T) {
	validID := types.NewID()

	validProject := func() *Leaf {
		return &Leaf{
			Name:        "Test Project",
			Description: "A valid leaf description for testing purposes.",
			TaskPattern: PatternParameterSweep,
			Visibility:  VisibilityPrivate,
			CreatorID:   &validID,
		}
	}

	tests := []struct {
		name    string
		modify  func(p *Leaf)
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid project",
			modify:  func(p *Leaf) {},
			wantErr: false,
		},
		{
			name:    "valid public project with research area",
			modify:  func(p *Leaf) { p.Visibility = VisibilityPublic; p.ResearchArea = []string{"biology"} },
			wantErr: false,
		},
		{
			name:    "valid project with creator public key",
			modify:  func(p *Leaf) { p.CreatorID = nil; p.CreatorPublicKey = []byte("some-key-bytes") },
			wantErr: false,
		},
		{
			name:    "name too short",
			modify:  func(p *Leaf) { p.Name = "ab" },
			wantErr: true,
			errMsg:  "name",
		},
		{
			name:    "name too long",
			modify:  func(p *Leaf) { p.Name = strings.Repeat("a", 101) },
			wantErr: true,
			errMsg:  "name",
		},
		{
			name:    "description too short",
			modify:  func(p *Leaf) { p.Description = "short" },
			wantErr: true,
			errMsg:  "description",
		},
		{
			name:    "description too long",
			modify:  func(p *Leaf) { p.Description = strings.Repeat("a", 10001) },
			wantErr: true,
			errMsg:  "description",
		},
		{
			name:    "research area empty when public",
			modify:  func(p *Leaf) { p.Visibility = VisibilityPublic; p.ResearchArea = nil },
			wantErr: true,
			errMsg:  "research_area",
		},
		{
			name:    "supported task pattern MAP_REDUCE",
			modify:  func(p *Leaf) { p.TaskPattern = PatternMapReduce },
			wantErr: false,
		},
		{
			name:    "supported task pattern MONTE_CARLO",
			modify:  func(p *Leaf) { p.TaskPattern = PatternMonteCarlo },
			wantErr: false,
		},
		{
			name:    "supported task pattern CUSTOM",
			modify:  func(p *Leaf) { p.TaskPattern = PatternCustom },
			wantErr: false,
		},
		{
			name:    "invalid task pattern",
			modify:  func(p *Leaf) { p.TaskPattern = "INVALID" },
			wantErr: true,
			errMsg:  "task_pattern",
		},
		{
			name:    "invalid visibility",
			modify:  func(p *Leaf) { p.Visibility = "INVALID" },
			wantErr: true,
			errMsg:  "visibility",
		},
		{
			name: "both creator_id and creator_public_key set",
			modify: func(p *Leaf) {
				p.CreatorID = &validID
				p.CreatorPublicKey = []byte("some-key")
			},
			wantErr: true,
			errMsg:  "mutually exclusive",
		},
		{
			name:    "neither creator_id nor creator_public_key set",
			modify:  func(p *Leaf) { p.CreatorID = nil; p.CreatorPublicKey = nil },
			wantErr: true,
			errMsg:  "creator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProject()
			tt.modify(p)
			err := ValidateMetadata(p)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- ExecutionConfig Validation Tests ---

func TestValidateExecutionConfig(t *testing.T) {
	validConfig := func() *ExecutionConfig {
		return &ExecutionConfig{
			Runtime:         "NATIVE",
			Binaries:        map[string]string{"linux_amd64": "https://example.com/bin"},
			BinaryChecksums: map[string]string{"linux_amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
			GPURequired:     false,
			GPUType:         "ANY",
			MaxMemoryMB:     4096,
			MaxDiskMB:       10240,
			MaxCPUSeconds:   86400,
		}
	}

	tests := []struct {
		name    string
		modify  func(c *ExecutionConfig)
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid native config",
			modify:  func(c *ExecutionConfig) {},
			wantErr: false,
		},
		{
			name:    "missing runtime",
			modify:  func(c *ExecutionConfig) { c.Runtime = "" },
			wantErr: true,
			errMsg:  "runtime",
		},
		{
			name: "valid container with image",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "nginx:latest"
				c.Image = &img
			},
			wantErr: false,
		},
		{
			name: "valid container with dockerfile",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				df := "storage://dockerfiles/my-project"
				c.Dockerfile = &df
			},
			wantErr: false,
		},
		{
			name: "container without image or dockerfile",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
			},
			wantErr: true,
			errMsg:  "image or dockerfile",
		},
		{
			name: "container with invalid OCI image reference",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "INVALID IMAGE!"
				c.Image = &img
			},
			wantErr: true,
			errMsg:  "OCI image reference",
		},
		{
			name: "container with valid registry image",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "registry.io/org/repo:v1"
				c.Image = &img
			},
			wantErr: false,
		},
		{
			name: "container with digest reference",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "repo@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4"
				c.Image = &img
			},
			wantErr: false,
		},
		{
			name: "GPU with native runtime",
			modify: func(c *ExecutionConfig) {
				c.GPURequired = true
				c.GPUType = "NVIDIA"
			},
			wantErr: true,
			errMsg:  "GPU tasks require container or WASM runtime",
		},
		{
			name: "GPU with container runtime valid",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "nvidia/cuda:12.0-base"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = "NVIDIA"
			},
			wantErr: false,
		},
		{
			name: "GPU with empty gpu_type valid (defaults applied separately)",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "myimage:latest"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = ""
			},
			wantErr: false,
		},
		{
			name: "GPU with negative min_vram_gb",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "myimage:latest"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = "NVIDIA"
				c.MinVRAMGB = -1
			},
			wantErr: true,
			errMsg:  "min_vram_gb",
		},
		{
			name: "GPU with AMD type valid",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "rocm/pytorch:latest"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = "AMD"
			},
			wantErr: false,
		},
		{
			name: "valid WASM leaf",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
			},
			wantErr: false,
		},
		{
			name:    "unsupported runtime script",
			modify:  func(c *ExecutionConfig) { c.Runtime = "SCRIPT" },
			wantErr: true,
			errMsg:  "not yet supported",
		},
		{
			name:    "invalid runtime value",
			modify:  func(c *ExecutionConfig) { c.Runtime = "invalid" },
			wantErr: true,
			errMsg:  "runtime",
		},
		{
			name:    "missing binaries for native",
			modify:  func(c *ExecutionConfig) { c.Binaries = nil },
			wantErr: true,
			errMsg:  "binaries",
		},
		{
			name:    "empty binaries map for native",
			modify:  func(c *ExecutionConfig) { c.Binaries = map[string]string{} },
			wantErr: true,
			errMsg:  "binaries",
		},
		// --- Native binary checksum (C2) validation tests ---
		{
			name:    "native missing checksum rejected",
			modify:  func(c *ExecutionConfig) { c.BinaryChecksums = nil },
			wantErr: true,
			errMsg:  "binary_checksums",
		},
		{
			name: "native checksum missing for one platform rejected",
			modify: func(c *ExecutionConfig) {
				c.Binaries = map[string]string{
					"linux_amd64":  "https://example.com/bin-amd64",
					"darwin_arm64": "https://example.com/bin-arm64",
				}
				// Only one of the two platforms has a checksum.
				c.BinaryChecksums = map[string]string{
					"linux_amd64": "0000000000000000000000000000000000000000000000000000000000000000",
				}
			},
			wantErr: true,
			errMsg:  "binary_checksums",
		},
		{
			name: "native checksum malformed (too short) rejected",
			modify: func(c *ExecutionConfig) {
				c.BinaryChecksums = map[string]string{"linux_amd64": "deadbeef"}
			},
			wantErr: true,
			errMsg:  "64-character lowercase hex",
		},
		{
			name: "native checksum malformed (uppercase) rejected",
			modify: func(c *ExecutionConfig) {
				c.BinaryChecksums = map[string]string{"linux_amd64": "ABCDEF0000000000000000000000000000000000000000000000000000000000"}
			},
			wantErr: true,
			errMsg:  "64-character lowercase hex",
		},
		{
			name: "native with valid checksums for all platforms",
			modify: func(c *ExecutionConfig) {
				c.Binaries = map[string]string{
					"linux_amd64":  "https://example.com/bin-amd64",
					"darwin_arm64": "https://example.com/bin-arm64",
				}
				c.BinaryChecksums = map[string]string{
					"linux_amd64":  "0000000000000000000000000000000000000000000000000000000000000000",
					"darwin_arm64": "1111111111111111111111111111111111111111111111111111111111111111",
				}
			},
			wantErr: false,
		},
		{
			name: "wasm with malformed checksum rejected",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.BinaryChecksums = map[string]string{"wasm": "not-hex"}
			},
			wantErr: true,
			errMsg:  "64-character lowercase hex",
		},
		{
			name: "wasm without checksum allowed",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.BinaryChecksums = nil
			},
			wantErr: false,
		},
		{
			name: "invalid gpu_type",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "myimage:latest"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = "intel"
			},
			wantErr: true,
			errMsg:  "gpu_type",
		},
		{
			name:    "invalid gpu_type with native",
			modify:  func(c *ExecutionConfig) { c.GPURequired = true; c.GPUType = "NVIDIA" },
			wantErr: true,
			errMsg:  "GPU tasks require container or WASM runtime",
		},
		{
			name:    "max_memory_mb zero",
			modify:  func(c *ExecutionConfig) { c.MaxMemoryMB = 0 },
			wantErr: true,
			errMsg:  "max_memory_mb",
		},
		{
			name:    "max_disk_mb zero",
			modify:  func(c *ExecutionConfig) { c.MaxDiskMB = 0 },
			wantErr: true,
			errMsg:  "max_disk_mb",
		},
		{
			name:    "max_cpu_seconds zero",
			modify:  func(c *ExecutionConfig) { c.MaxCPUSeconds = 0 },
			wantErr: true,
			errMsg:  "max_cpu_seconds",
		},
		// --- WASM-specific validation tests ---
		{
			name: "WASM missing binaries[wasm]",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{}
			},
			wantErr: true,
			errMsg:  "binaries[\"wasm\"]",
		},
		{
			name: "WASM binaries[wasm] not ending in .wasm",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.js"}
			},
			wantErr: true,
			errMsg:  "must end in .wasm",
		},
		{
			name: "WASM with image set",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				img := "nginx:latest"
				c.Image = &img
			},
			wantErr: true,
			errMsg:  "image must be empty",
		},
		{
			name: "WASM with dockerfile set",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				df := "storage://dockerfiles/my-project"
				c.Dockerfile = &df
			},
			wantErr: true,
			errMsg:  "dockerfile must be empty",
		},
		{
			name: "WASM with language set",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				lang := "PYTHON"
				c.Language = &lang
			},
			wantErr: true,
			errMsg:  "language must be empty",
		},
		{
			name: "WASM with entry_point set",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				ep := "main.py"
				c.EntryPoint = &ep
			},
			wantErr: true,
			errMsg:  "entry_point must be empty",
		},
		{
			name: "WASM with dependencies set",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				deps := "requirements.txt"
				c.Dependencies = &deps
			},
			wantErr: true,
			errMsg:  "dependencies must be empty",
		},
		{
			name: "WASM with nil binaries map",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = nil
			},
			wantErr: true,
			errMsg:  "binaries[\"wasm\"]",
		},
		{
			name: "WASM with gpu_required and empty gpu_type valid",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.GPURequired = true
				c.GPUType = ""
			},
			wantErr: false,
		},
		{
			name: "WASM with network_access true",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.NetworkAccess = true
			},
			wantErr: true,
			errMsg:  "network_access must be false",
		},
		{
			name: "WASM with gpu_required and gpu_type WEBGPU",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{
					"wasm": "https://example.com/compute.wasm",
					"wgsl": "https://example.com/shader.wgsl",
				}
				c.GPURequired = true
				c.GPUType = "WEBGPU"
			},
			wantErr: false,
		},
		{
			name: "WASM with gpu_required and gpu_type NVIDIA rejected",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.GPURequired = true
				c.GPUType = "NVIDIA"
			},
			wantErr: true,
			errMsg:  "WASM leafs only support gpu_type WEBGPU or ANY",
		},
		{
			name: "WASM with gpu_required and gpu_type AMD rejected",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.GPURequired = true
				c.GPUType = "AMD"
			},
			wantErr: true,
			errMsg:  "WASM leafs only support gpu_type WEBGPU or ANY",
		},
		{
			name: "WASM with gpu_required and gpu_type ANY valid",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.GPURequired = true
				c.GPUType = "ANY"
			},
			wantErr: false,
		},
		{
			name: "WASM with MaxMemoryMB > 4096 passes (warn only)",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "WASM"
				c.Binaries = map[string]string{"wasm": "https://example.com/compute.wasm"}
				c.MaxMemoryMB = 8192
			},
			wantErr: false,
		},
		{
			name: "WEBGPU gpu_type accepted for container runtime too",
			modify: func(c *ExecutionConfig) {
				c.Runtime = "CONTAINER"
				c.Binaries = nil
				img := "myimage:latest"
				c.Image = &img
				c.GPURequired = true
				c.GPUType = "WEBGPU"
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.modify(c)
			err := ValidateExecutionConfig(c)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- ValidationConfig Tests ---

func TestValidateValidationConfig(t *testing.T) {
	validConfig := func() *ValidationConfig {
		return &ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		}
	}

	tolerance := 0.001
	zeroTolerance := 0.0
	comparatorRef := "custom-comparator-v1"

	tests := []struct {
		name    string
		modify  func(c *ValidationConfig)
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid defaults",
			modify:  func(c *ValidationConfig) {},
			wantErr: false,
		},
		{
			name:    "redundancy_factor zero",
			modify:  func(c *ValidationConfig) { c.RedundancyFactor = 0 },
			wantErr: true,
			errMsg:  "redundancy_factor",
		},
		{
			name:    "redundancy_factor too high",
			modify:  func(c *ValidationConfig) { c.RedundancyFactor = 6 },
			wantErr: true,
			errMsg:  "redundancy_factor",
		},
		{
			name:    "agreement_threshold negative",
			modify:  func(c *ValidationConfig) { c.AgreementThreshold = -0.1 },
			wantErr: true,
			errMsg:  "agreement_threshold",
		},
		{
			name:    "agreement_threshold greater than 1",
			modify:  func(c *ValidationConfig) { c.AgreementThreshold = 1.1 },
			wantErr: true,
			errMsg:  "agreement_threshold",
		},
		{
			name:    "invalid comparison_mode",
			modify:  func(c *ValidationConfig) { c.ComparisonMode = "fuzzy" },
			wantErr: true,
			errMsg:  "comparison_mode",
		},
		{
			name: "numeric_tolerance missing when mode is numeric_tolerance",
			modify: func(c *ValidationConfig) {
				c.ComparisonMode = "NUMERIC_TOLERANCE"
				c.NumericTolerance = nil
			},
			wantErr: true,
			errMsg:  "numeric_tolerance",
		},
		{
			name: "numeric_tolerance zero when mode is numeric_tolerance",
			modify: func(c *ValidationConfig) {
				c.ComparisonMode = "NUMERIC_TOLERANCE"
				c.NumericTolerance = &zeroTolerance
			},
			wantErr: true,
			errMsg:  "numeric_tolerance",
		},
		{
			name: "valid numeric_tolerance config",
			modify: func(c *ValidationConfig) {
				c.ComparisonMode = "NUMERIC_TOLERANCE"
				c.NumericTolerance = &tolerance
			},
			wantErr: false,
		},
		{
			name: "custom_comparator_ref missing when mode is custom",
			modify: func(c *ValidationConfig) {
				c.ComparisonMode = "CUSTOM"
				c.CustomComparatorRef = nil
			},
			wantErr: true,
			errMsg:  "custom_comparator_ref",
		},
		{
			name: "valid custom comparator config",
			modify: func(c *ValidationConfig) {
				c.ComparisonMode = "CUSTOM"
				c.CustomComparatorRef = &comparatorRef
			},
			wantErr: false,
		},
		{
			name:    "max_retries zero",
			modify:  func(c *ValidationConfig) { c.MaxRetries = 0 },
			wantErr: true,
			errMsg:  "max_retries",
		},
		{
			name:    "max_retries too high",
			modify:  func(c *ValidationConfig) { c.MaxRetries = 11 },
			wantErr: true,
			errMsg:  "max_retries",
		},
		// Spot-check tests
		{
			name: "spot_check_enabled with redundancy_factor=1",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 1
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 5.0
			},
			wantErr: false,
		},
		{
			name: "spot_check_enabled rejected when redundancy_factor > 1",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 2
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 5.0
			},
			wantErr: true,
			errMsg:  "spot-check validation is only for redundancy_factor=1",
		},
		{
			name: "spot_check_percentage too low",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 1
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 0.5
			},
			wantErr: true,
			errMsg:  "spot_check_percentage",
		},
		{
			name: "spot_check_percentage too high",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 1
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 25.0
			},
			wantErr: true,
			errMsg:  "spot_check_percentage",
		},
		{
			name: "spot_check_percentage at boundaries",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 1
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 1.0
			},
			wantErr: false,
		},
		{
			name: "spot_check_percentage at upper boundary",
			modify: func(c *ValidationConfig) {
				c.RedundancyFactor = 1
				c.SpotCheckEnabled = true
				c.SpotCheckPercentage = 20.0
			},
			wantErr: false,
		},
		{
			name: "spot_check_disabled ignores percentage",
			modify: func(c *ValidationConfig) {
				c.SpotCheckEnabled = false
				c.SpotCheckPercentage = 999.0 // invalid but ignored
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.modify(c)
			err := ValidateValidationConfig(c)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- FaultToleranceConfig Tests ---

func TestValidateFaultToleranceConfig(t *testing.T) {
	validConfig := func() *FaultToleranceConfig {
		return &FaultToleranceConfig{
			HeartbeatIntervalSeconds:  300,
			MissedHeartbeatsThreshold: 3,
			DeadlineMultiplier:        3.0,
			MaxReassignments:          3,
			CheckpointingEnabled:      false,
		}
	}

	tests := []struct {
		name    string
		modify  func(c *FaultToleranceConfig)
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid defaults",
			modify:  func(c *FaultToleranceConfig) {},
			wantErr: false,
		},
		{
			name:    "heartbeat too low",
			modify:  func(c *FaultToleranceConfig) { c.HeartbeatIntervalSeconds = 59 },
			wantErr: true,
			errMsg:  "heartbeat_interval_seconds",
		},
		{
			name:    "heartbeat too high",
			modify:  func(c *FaultToleranceConfig) { c.HeartbeatIntervalSeconds = 3601 },
			wantErr: true,
			errMsg:  "heartbeat_interval_seconds",
		},
		{
			name:    "missed threshold too low",
			modify:  func(c *FaultToleranceConfig) { c.MissedHeartbeatsThreshold = 0 },
			wantErr: true,
			errMsg:  "missed_heartbeats_threshold",
		},
		{
			name:    "missed threshold too high",
			modify:  func(c *FaultToleranceConfig) { c.MissedHeartbeatsThreshold = 11 },
			wantErr: true,
			errMsg:  "missed_heartbeats_threshold",
		},
		{
			name:    "deadline multiplier too low",
			modify:  func(c *FaultToleranceConfig) { c.DeadlineMultiplier = 0.5 },
			wantErr: true,
			errMsg:  "deadline_multiplier",
		},
		{
			name:    "deadline multiplier too high",
			modify:  func(c *FaultToleranceConfig) { c.DeadlineMultiplier = 11.0 },
			wantErr: true,
			errMsg:  "deadline_multiplier",
		},
		{
			name:    "max_reassignments too low",
			modify:  func(c *FaultToleranceConfig) { c.MaxReassignments = 0 },
			wantErr: true,
			errMsg:  "max_reassignments",
		},
		{
			name:    "max_reassignments too high",
			modify:  func(c *FaultToleranceConfig) { c.MaxReassignments = 11 },
			wantErr: true,
			errMsg:  "max_reassignments",
		},
		{
			name: "checkpointing enabled with valid interval",
			modify: func(c *FaultToleranceConfig) {
				c.CheckpointingEnabled = true
				interval := 300
				c.CheckpointIntervalSeconds = &interval
			},
			wantErr: false,
		},
		{
			name: "checkpointing enabled without interval",
			modify: func(c *FaultToleranceConfig) {
				c.CheckpointingEnabled = true
			},
			wantErr: true,
			errMsg:  "checkpoint_interval_seconds",
		},
		{
			name: "checkpointing enabled with interval too low",
			modify: func(c *FaultToleranceConfig) {
				c.CheckpointingEnabled = true
				interval := 59
				c.CheckpointIntervalSeconds = &interval
			},
			wantErr: true,
			errMsg:  "checkpoint_interval_seconds",
		},
		{
			name: "checkpointing enabled with interval at minimum",
			modify: func(c *FaultToleranceConfig) {
				c.CheckpointingEnabled = true
				interval := 60
				c.CheckpointIntervalSeconds = &interval
			},
			wantErr: false,
		},
		{
			name:    "negative max_checkpoint_size_bytes",
			modify:  func(c *FaultToleranceConfig) { c.MaxCheckpointSizeBytes = -1 },
			wantErr: true,
			errMsg:  "max_checkpoint_size_bytes",
		},
		{
			name: "checkpointing disabled with interval set (no error)",
			modify: func(c *FaultToleranceConfig) {
				interval := 30
				c.CheckpointIntervalSeconds = &interval
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.modify(c)
			err := ValidateFaultToleranceConfig(c)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- DataConfig Tests ---

func TestValidateDataConfig(t *testing.T) {
	validConfig := func() *DataConfig {
		return &DataConfig{
			TransferStrategy:   "INLINE",
			AggregationFormat:  "JSON",
			MaxInputSizeBytes:  1048576,
			MaxOutputSizeBytes: 104857600,
		}
	}

	bucket := "my-bucket"
	extURL := "https://example.com/data"
	splitStrategy := "by_line_count"

	tests := []struct {
		name        string
		taskPattern TaskPattern
		modify      func(c *DataConfig)
		wantErr     bool
		errMsg      string
	}{
		{
			name:        "valid defaults",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) {},
			wantErr:     false,
		},
		{
			name:        "invalid transfer_strategy",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.TransferStrategy = "carrier_pigeon" },
			wantErr:     true,
			errMsg:      "transfer_strategy",
		},
		{
			name:        "platform_managed rejected for self-hosted",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.TransferStrategy = "PLATFORM_MANAGED" },
			wantErr:     true,
			errMsg:      "hosted platform",
		},
		{
			name:        "platform_managed rejected even with storage_bucket",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.TransferStrategy = "PLATFORM_MANAGED"; c.StorageBucket = &bucket },
			wantErr:     true,
			errMsg:      "hosted platform",
		},
		{
			name:        "missing external_base_url for external_reference",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.TransferStrategy = "EXTERNAL_REFERENCE" },
			wantErr:     true,
			errMsg:      "external_base_url",
		},
		{
			name:        "valid external_reference config",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.TransferStrategy = "EXTERNAL_REFERENCE"; c.ExternalBaseURL = &extURL },
			wantErr:     false,
		},
		{
			name:        "splitting_strategy should be null for parameter_sweep",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.SplittingStrategy = &splitStrategy },
			wantErr:     true,
			errMsg:      "splitting_strategy",
		},
		{
			name:        "invalid aggregation_format",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.AggregationFormat = "xml" },
			wantErr:     true,
			errMsg:      "aggregation_format",
		},
		{
			name:        "valid csv aggregation format",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.AggregationFormat = "CSV" },
			wantErr:     false,
		},
		{
			name:        "max_input_size_bytes zero",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.MaxInputSizeBytes = 0 },
			wantErr:     true,
			errMsg:      "max_input_size_bytes",
		},
		{
			name:        "max_output_size_bytes zero",
			taskPattern: PatternParameterSweep,
			modify:      func(c *DataConfig) { c.MaxOutputSizeBytes = 0 },
			wantErr:     true,
			errMsg:      "max_output_size_bytes",
		},
		// --- MAP_REDUCE data_config validation ---
		{
			name:        "map_reduce missing splitting_strategy",
			taskPattern: PatternMapReduce,
			modify:      func(c *DataConfig) { c.SplittingStrategy = nil },
			wantErr:     true,
			errMsg:      "splitting_strategy",
		},
		{
			name:        "map_reduce valid by_line_count",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"lines_per_chunk": float64(500)}
			},
			wantErr: false,
		},
		{
			name:        "map_reduce valid by_byte_size",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_byte_size"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"bytes_per_chunk": float64(1048576)}
			},
			wantErr: false,
		},
		{
			name:        "map_reduce valid by_record",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_record"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"record_delimiter": "\n---\n", "records_per_chunk": float64(50)}
			},
			wantErr: false,
		},
		{
			name:        "map_reduce invalid splitting_strategy",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_magic"
				c.SplittingStrategy = &s
			},
			wantErr: true,
			errMsg:  "splitting_strategy",
		},
		{
			name:        "map_reduce by_line_count invalid lines_per_chunk",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"lines_per_chunk": float64(0)}
			},
			wantErr: true,
			errMsg:  "lines_per_chunk",
		},
		{
			name:        "map_reduce by_byte_size invalid bytes_per_chunk",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_byte_size"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"bytes_per_chunk": float64(100)}
			},
			wantErr: true,
			errMsg:  "bytes_per_chunk",
		},
		{
			name:        "map_reduce by_record empty delimiter",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_record"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"record_delimiter": ""}
			},
			wantErr: true,
			errMsg:  "record_delimiter",
		},
		{
			name:        "map_reduce by_record invalid records_per_chunk",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_record"
				c.SplittingStrategy = &s
				c.SplittingConfig = map[string]any{"records_per_chunk": float64(-1)}
			},
			wantErr: true,
			errMsg:  "records_per_chunk",
		},
		{
			name:        "map_reduce defaults when no splitting_config",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
				c.SplittingConfig = nil
			},
			wantErr: false,
		},
		// --- MONTE_CARLO data_config validation ---
		{
			name:        "monte_carlo valid minimal config",
			taskPattern: PatternMonteCarlo,
			modify:      func(c *DataConfig) {},
			wantErr:     false,
		},
		{
			name:        "monte_carlo splitting_strategy must be nil",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
			},
			wantErr: true,
			errMsg:  "splitting_strategy",
		},
		{
			name:        "monte_carlo valid splitting_config num_trials",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{"num_trials": float64(1000)}
			},
			wantErr: false,
		},
		{
			name:        "monte_carlo num_trials zero",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{"num_trials": float64(0)}
			},
			wantErr: true,
			errMsg:  "num_trials",
		},
		{
			name:        "monte_carlo num_trials exceeds max",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{"num_trials": float64(10_000_001)}
			},
			wantErr: true,
			errMsg:  "num_trials",
		},
		{
			name:        "monte_carlo valid seed_strategy sequential",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{
					"num_trials":    float64(100),
					"seed_strategy": "sequential",
				}
			},
			wantErr: false,
		},
		{
			name:        "monte_carlo valid seed_strategy hash",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{
					"num_trials":    float64(100),
					"seed_strategy": "hash",
				}
			},
			wantErr: false,
		},
		{
			name:        "monte_carlo invalid seed_strategy",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{
					"num_trials":    float64(100),
					"seed_strategy": "random",
				}
			},
			wantErr: true,
			errMsg:  "seed_strategy",
		},
		{
			name:        "monte_carlo seed_offset negative",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{
					"num_trials":  float64(100),
					"seed_offset": float64(-1),
				}
			},
			wantErr: true,
			errMsg:  "seed_offset",
		},
		{
			name:        "monte_carlo valid seed_offset",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.SplittingConfig = map[string]any{
					"num_trials":  float64(100),
					"seed_offset": float64(500),
				}
			},
			wantErr: false,
		},
		{
			name:        "monte_carlo valid aggregation_config",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{
					"aggregator_type":  "all",
					"confidence_level": float64(0.95),
				}
			},
			wantErr: false,
		},
		{
			name:        "monte_carlo invalid aggregator_type",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"aggregator_type": "median"}
			},
			wantErr: true,
			errMsg:  "aggregator_type",
		},
		{
			name:        "monte_carlo confidence_level too low",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"confidence_level": float64(0.1)}
			},
			wantErr: true,
			errMsg:  "confidence_level",
		},
		{
			name:        "monte_carlo confidence_level too high",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"confidence_level": float64(1.0)}
			},
			wantErr: true,
			errMsg:  "confidence_level",
		},
		// --- CUSTOM data_config validation ---
		{
			name:        "custom valid minimal config",
			taskPattern: PatternCustom,
			modify:      func(c *DataConfig) {},
			wantErr:     false,
		},
		{
			name:        "custom splitting_strategy must be nil",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
			},
			wantErr: true,
			errMsg:  "splitting_strategy",
		},
		{
			name:        "custom valid reducer_type sum",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": "sum"}
			},
			wantErr: false,
		},
		{
			name:        "custom valid reducer_type average",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": "average"}
			},
			wantErr: false,
		},
		{
			name:        "custom valid reducer_type concatenate",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": "concatenate"}
			},
			wantErr: false,
		},
		{
			name:        "custom valid reducer_type merge",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": "merge"}
			},
			wantErr: false,
		},
		{
			name:        "custom valid reducer_type null",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": nil}
			},
			wantErr: false,
		},
		{
			name:        "custom invalid reducer_type",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": "median"}
			},
			wantErr: true,
			errMsg:  "reducer_type",
		},
		{
			name:        "custom reducer_type non-string type",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = map[string]any{"reducer_type": float64(42)}
			},
			wantErr: true,
			errMsg:  "reducer_type",
		},
		{
			name:        "custom no aggregation_config valid",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.AggregationConfig = nil
			},
			wantErr: false,
		},
		// --- Lazy generation config validation ---
		{
			name:        "valid lazy generation config",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 100
				c.LazyBatchSize = 1000
			},
			wantErr: false,
		},
		{
			name:        "valid eager generation mode (default)",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "eager"
			},
			wantErr: false,
		},
		{
			name:        "invalid generation_mode",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "turbo"
			},
			wantErr: true,
			errMsg:  "generation_mode",
		},
		{
			name:        "lazy not supported for custom pattern",
			taskPattern: PatternCustom,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 100
				c.LazyBatchSize = 1000
			},
			wantErr: true,
			errMsg:  "lazy generation is not supported for custom pattern",
		},
		{
			name:        "lazy threshold too low",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 0
				c.LazyBatchSize = 1000
			},
			wantErr: true,
			errMsg:  "lazy_threshold",
		},
		{
			name:        "lazy threshold too high",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 10001
				c.LazyBatchSize = 1000
			},
			wantErr: true,
			errMsg:  "lazy_threshold",
		},
		{
			name:        "lazy batch_size too low",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 100
				c.LazyBatchSize = 0
			},
			wantErr: true,
			errMsg:  "lazy_batch_size",
		},
		{
			name:        "lazy batch_size too high",
			taskPattern: PatternParameterSweep,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 100
				c.LazyBatchSize = 100001
			},
			wantErr: true,
			errMsg:  "lazy_batch_size",
		},
		{
			name:        "lazy valid for monte_carlo",
			taskPattern: PatternMonteCarlo,
			modify: func(c *DataConfig) {
				c.GenerationMode = "lazy"
				c.LazyThreshold = 50
				c.LazyBatchSize = 500
			},
			wantErr: false,
		},
		{
			name:        "lazy valid for map_reduce",
			taskPattern: PatternMapReduce,
			modify: func(c *DataConfig) {
				s := "by_line_count"
				c.SplittingStrategy = &s
				c.GenerationMode = "lazy"
				c.LazyThreshold = 50
				c.LazyBatchSize = 500
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.modify(c)
			err := ValidateDataConfig(c, tt.taskPattern)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- CreditConfig Tests ---

func TestValidateCreditConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  CreditConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid default",
			config:  CreditConfig{CreditPerValidatedWorkUnit: 1.0},
			wantErr: false,
		},
		{
			name:    "valid custom credit",
			config:  CreditConfig{CreditPerValidatedWorkUnit: 5.5},
			wantErr: false,
		},
		{
			name:    "credit zero",
			config:  CreditConfig{CreditPerValidatedWorkUnit: 0},
			wantErr: true,
			errMsg:  "credit_per_validated_work_unit",
		},
		{
			name:    "credit negative",
			config:  CreditConfig{CreditPerValidatedWorkUnit: -1.0},
			wantErr: true,
			errMsg:  "credit_per_validated_work_unit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreditConfig(&tt.config)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- ResourceRequirements Tests ---

func TestValidateResourceRequirements(t *testing.T) {
	validReqs := func() *ResourceRequirements {
		return &ResourceRequirements{
			MinCPUCores:      1,
			MinDiskMB:        1024,
			MinGPUVRAMMB:     0,
			GPURequired:      false,
			MinBandwidthMbps: 0,
		}
	}

	tests := []struct {
		name    string
		modify  func(r *ResourceRequirements)
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid defaults",
			modify:  func(r *ResourceRequirements) {},
			wantErr: false,
		},
		{
			name:    "min_cpu_cores zero",
			modify:  func(r *ResourceRequirements) { r.MinCPUCores = 0 },
			wantErr: true,
			errMsg:  "min_cpu_cores",
		},
		{
			name:    "min_disk_mb zero",
			modify:  func(r *ResourceRequirements) { r.MinDiskMB = 0 },
			wantErr: true,
			errMsg:  "min_disk_mb",
		},
		{
			name:    "min_gpu_vram_mb negative",
			modify:  func(r *ResourceRequirements) { r.MinGPUVRAMMB = -1 },
			wantErr: true,
			errMsg:  "min_gpu_vram_mb",
		},
		{
			name:    "min_bandwidth_mbps negative",
			modify:  func(r *ResourceRequirements) { r.MinBandwidthMbps = -1 },
			wantErr: true,
			errMsg:  "min_bandwidth_mbps",
		},
		{
			name:    "valid with gpu required",
			modify:  func(r *ResourceRequirements) { r.GPURequired = true; r.MinGPUVRAMMB = 4096 },
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validReqs()
			tt.modify(r)
			err := ValidateResourceRequirements(r)
			assertValidationResult(t, err, tt.wantErr, tt.errMsg)
		})
	}
}

// --- Default-Filling Tests ---

func TestApplyExecutionConfigDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		c := &ExecutionConfig{}
		ApplyExecutionConfigDefaults(c)
		if c.GPUType != "ANY" {
			t.Errorf("expected gpu_type 'ANY', got %q", c.GPUType)
		}
		if c.MaxMemoryMB != 4096 {
			t.Errorf("expected max_memory_mb 4096, got %d", c.MaxMemoryMB)
		}
		if c.MaxDiskMB != 10240 {
			t.Errorf("expected max_disk_mb 10240, got %d", c.MaxDiskMB)
		}
		if c.MaxCPUSeconds != 86400 {
			t.Errorf("expected max_cpu_seconds 86400, got %d", c.MaxCPUSeconds)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		c := &ExecutionConfig{
			GPUType:       "NVIDIA",
			MaxMemoryMB:   8192,
			MaxDiskMB:     20480,
			MaxCPUSeconds: 172800,
		}
		ApplyExecutionConfigDefaults(c)
		if c.GPUType != "NVIDIA" {
			t.Errorf("expected gpu_type 'NVIDIA', got %q", c.GPUType)
		}
		if c.MaxMemoryMB != 8192 {
			t.Errorf("expected max_memory_mb 8192, got %d", c.MaxMemoryMB)
		}
		if c.MaxDiskMB != 20480 {
			t.Errorf("expected max_disk_mb 20480, got %d", c.MaxDiskMB)
		}
		if c.MaxCPUSeconds != 172800 {
			t.Errorf("expected max_cpu_seconds 172800, got %d", c.MaxCPUSeconds)
		}
	})
}

func TestApplyValidationConfigDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		c := &ValidationConfig{}
		ApplyValidationConfigDefaults(c)
		if c.RedundancyFactor != 2 {
			t.Errorf("expected redundancy_factor 2, got %d", c.RedundancyFactor)
		}
		if c.AgreementThreshold != 1.0 {
			t.Errorf("expected agreement_threshold 1.0, got %f", c.AgreementThreshold)
		}
		if c.ComparisonMode != "EXACT" {
			t.Errorf("expected comparison_mode 'EXACT', got %q", c.ComparisonMode)
		}
		if c.MaxRetries != 3 {
			t.Errorf("expected max_retries 3, got %d", c.MaxRetries)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		c := &ValidationConfig{
			RedundancyFactor:   4,
			AgreementThreshold: 0.67,
			ComparisonMode:     "NUMERIC_TOLERANCE",
			MaxRetries:         5,
		}
		ApplyValidationConfigDefaults(c)
		if c.RedundancyFactor != 4 {
			t.Errorf("expected redundancy_factor 4, got %d", c.RedundancyFactor)
		}
		if c.AgreementThreshold != 0.67 {
			t.Errorf("expected agreement_threshold 0.67, got %f", c.AgreementThreshold)
		}
		if c.ComparisonMode != "NUMERIC_TOLERANCE" {
			t.Errorf("expected comparison_mode 'NUMERIC_TOLERANCE', got %q", c.ComparisonMode)
		}
		if c.MaxRetries != 5 {
			t.Errorf("expected max_retries 5, got %d", c.MaxRetries)
		}
	})

	t.Run("spot_check_percentage defaults to 5.0 when enabled", func(t *testing.T) {
		c := &ValidationConfig{
			SpotCheckEnabled: true,
		}
		ApplyValidationConfigDefaults(c)
		if c.SpotCheckPercentage != 5.0 {
			t.Errorf("expected spot_check_percentage 5.0, got %f", c.SpotCheckPercentage)
		}
	})

	t.Run("spot_check_percentage not overwritten when set", func(t *testing.T) {
		c := &ValidationConfig{
			SpotCheckEnabled:    true,
			SpotCheckPercentage: 10.0,
		}
		ApplyValidationConfigDefaults(c)
		if c.SpotCheckPercentage != 10.0 {
			t.Errorf("expected spot_check_percentage 10.0, got %f", c.SpotCheckPercentage)
		}
	})

	t.Run("spot_check_percentage not set when disabled", func(t *testing.T) {
		c := &ValidationConfig{
			SpotCheckEnabled: false,
		}
		ApplyValidationConfigDefaults(c)
		if c.SpotCheckPercentage != 0 {
			t.Errorf("expected spot_check_percentage 0 when disabled, got %f", c.SpotCheckPercentage)
		}
	})
}

func TestApplyFaultToleranceConfigDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		c := &FaultToleranceConfig{}
		ApplyFaultToleranceConfigDefaults(c)
		if c.HeartbeatIntervalSeconds != 300 {
			t.Errorf("expected heartbeat_interval_seconds 300, got %d", c.HeartbeatIntervalSeconds)
		}
		if c.MissedHeartbeatsThreshold != 3 {
			t.Errorf("expected missed_heartbeats_threshold 3, got %d", c.MissedHeartbeatsThreshold)
		}
		if c.DeadlineMultiplier != 3.0 {
			t.Errorf("expected deadline_multiplier 3.0, got %f", c.DeadlineMultiplier)
		}
		if c.MaxReassignments != 3 {
			t.Errorf("expected max_reassignments 3, got %d", c.MaxReassignments)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		c := &FaultToleranceConfig{
			HeartbeatIntervalSeconds:  600,
			MissedHeartbeatsThreshold: 5,
			DeadlineMultiplier:        5.0,
			MaxReassignments:          5,
		}
		ApplyFaultToleranceConfigDefaults(c)
		if c.HeartbeatIntervalSeconds != 600 {
			t.Errorf("expected heartbeat_interval_seconds 600, got %d", c.HeartbeatIntervalSeconds)
		}
		if c.MissedHeartbeatsThreshold != 5 {
			t.Errorf("expected missed_heartbeats_threshold 5, got %d", c.MissedHeartbeatsThreshold)
		}
		if c.DeadlineMultiplier != 5.0 {
			t.Errorf("expected deadline_multiplier 5.0, got %f", c.DeadlineMultiplier)
		}
		if c.MaxReassignments != 5 {
			t.Errorf("expected max_reassignments 5, got %d", c.MaxReassignments)
		}
	})
}

func TestApplyDataConfigDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		c := &DataConfig{}
		ApplyDataConfigDefaults(c)
		if c.TransferStrategy != "INLINE" {
			t.Errorf("expected transfer_strategy 'INLINE', got %q", c.TransferStrategy)
		}
		if c.AggregationFormat != "JSON" {
			t.Errorf("expected aggregation_format 'JSON', got %q", c.AggregationFormat)
		}
		if c.MaxInputSizeBytes != 1048576 {
			t.Errorf("expected max_input_size_bytes 1048576, got %d", c.MaxInputSizeBytes)
		}
		if c.MaxOutputSizeBytes != 104857600 {
			t.Errorf("expected max_output_size_bytes 104857600, got %d", c.MaxOutputSizeBytes)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		c := &DataConfig{
			TransferStrategy:   "PLATFORM_MANAGED",
			AggregationFormat:  "CSV",
			MaxInputSizeBytes:  2097152,
			MaxOutputSizeBytes: 209715200,
		}
		ApplyDataConfigDefaults(c)
		if c.TransferStrategy != "PLATFORM_MANAGED" {
			t.Errorf("expected transfer_strategy 'PLATFORM_MANAGED', got %q", c.TransferStrategy)
		}
		if c.AggregationFormat != "CSV" {
			t.Errorf("expected aggregation_format 'CSV', got %q", c.AggregationFormat)
		}
		if c.MaxInputSizeBytes != 2097152 {
			t.Errorf("expected max_input_size_bytes 2097152, got %d", c.MaxInputSizeBytes)
		}
		if c.MaxOutputSizeBytes != 209715200 {
			t.Errorf("expected max_output_size_bytes 209715200, got %d", c.MaxOutputSizeBytes)
		}
	})

	t.Run("defaults generation_mode to eager", func(t *testing.T) {
		c := &DataConfig{}
		ApplyDataConfigDefaults(c)
		if c.GenerationMode != "eager" {
			t.Errorf("expected generation_mode 'eager', got %q", c.GenerationMode)
		}
	})

	t.Run("lazy defaults threshold and batch_size", func(t *testing.T) {
		c := &DataConfig{GenerationMode: "lazy"}
		ApplyDataConfigDefaults(c)
		if c.LazyThreshold != 100 {
			t.Errorf("expected lazy_threshold 100, got %d", c.LazyThreshold)
		}
		if c.LazyBatchSize != 1000 {
			t.Errorf("expected lazy_batch_size 1000, got %d", c.LazyBatchSize)
		}
	})

	t.Run("lazy does not overwrite non-zero values", func(t *testing.T) {
		c := &DataConfig{GenerationMode: "lazy", LazyThreshold: 50, LazyBatchSize: 500}
		ApplyDataConfigDefaults(c)
		if c.LazyThreshold != 50 {
			t.Errorf("expected lazy_threshold 50, got %d", c.LazyThreshold)
		}
		if c.LazyBatchSize != 500 {
			t.Errorf("expected lazy_batch_size 500, got %d", c.LazyBatchSize)
		}
	})
}

func TestApplyCreditConfigDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		c := &CreditConfig{}
		ApplyCreditConfigDefaults(c)
		if c.CreditPerValidatedWorkUnit != 1.0 {
			t.Errorf("expected credit_per_validated_work_unit 1.0, got %f", c.CreditPerValidatedWorkUnit)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		c := &CreditConfig{CreditPerValidatedWorkUnit: 5.0}
		ApplyCreditConfigDefaults(c)
		if c.CreditPerValidatedWorkUnit != 5.0 {
			t.Errorf("expected credit_per_validated_work_unit 5.0, got %f", c.CreditPerValidatedWorkUnit)
		}
	})
}

func TestApplyResourceRequirementsDefaults(t *testing.T) {
	t.Run("fills zero values", func(t *testing.T) {
		r := &ResourceRequirements{}
		ApplyResourceRequirementsDefaults(r)
		if r.MinCPUCores != 1 {
			t.Errorf("expected min_cpu_cores 1, got %d", r.MinCPUCores)
		}
		if r.MinDiskMB != 1024 {
			t.Errorf("expected min_disk_mb 1024, got %d", r.MinDiskMB)
		}
	})

	t.Run("does not overwrite non-zero values", func(t *testing.T) {
		r := &ResourceRequirements{
			MinCPUCores: 4,
			MinDiskMB:   4096,
		}
		ApplyResourceRequirementsDefaults(r)
		if r.MinCPUCores != 4 {
			t.Errorf("expected min_cpu_cores 4, got %d", r.MinCPUCores)
		}
		if r.MinDiskMB != 4096 {
			t.Errorf("expected min_disk_mb 4096, got %d", r.MinDiskMB)
		}
	})
}
