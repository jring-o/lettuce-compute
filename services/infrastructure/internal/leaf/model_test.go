package leaf

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeafStateValues(t *testing.T) {
	tests := []struct {
		state LeafState
		want  string
	}{
		{StateDraft, "DRAFT"},
		{StateConfiguring, "CONFIGURING"},
		{StateActive, "ACTIVE"},
		{StatePaused, "PAUSED"},
		{StateCompleted, "COMPLETED"},
		{StateArchived, "ARCHIVED"},
	}
	for _, tt := range tests {
		if string(tt.state) != tt.want {
			t.Errorf("LeafState = %q, want %q", tt.state, tt.want)
		}
	}
}

func TestTaskPatternValues(t *testing.T) {
	tests := []struct {
		pattern TaskPattern
		want    string
	}{
		{PatternParameterSweep, "PARAMETER_SWEEP"},
		{PatternMapReduce, "MAP_REDUCE"},
		{PatternMonteCarlo, "MONTE_CARLO"},
		{PatternCustom, "CUSTOM"},
	}
	for _, tt := range tests {
		if string(tt.pattern) != tt.want {
			t.Errorf("TaskPattern = %q, want %q", tt.pattern, tt.want)
		}
	}
}

func TestLeafVisibilityValues(t *testing.T) {
	tests := []struct {
		vis  LeafVisibility
		want string
	}{
		{VisibilityPublic, "PUBLIC"},
		{VisibilityUnlisted, "UNLISTED"},
		{VisibilityPrivate, "PRIVATE"},
	}
	for _, tt := range tests {
		if string(tt.vis) != tt.want {
			t.Errorf("LeafVisibility = %q, want %q", tt.vis, tt.want)
		}
	}
}

func TestExecutionConfigJSON(t *testing.T) {
	cfg := ExecutionConfig{
		Runtime:       "NATIVE",
		Binaries:      map[string]string{"linux_amd64": "https://example.com/bin"},
		GPURequired:   false,
		GPUType:       "ANY",
		NetworkAccess: false,
		MaxMemoryMB:   4096,
		MaxDiskMB:     10240,
		MaxCPUSeconds: 86400,
		EnvVars:       map[string]string{"FOO": "bar"},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExecutionConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Runtime != cfg.Runtime {
		t.Errorf("Runtime = %q, want %q", got.Runtime, cfg.Runtime)
	}
	if got.MaxMemoryMB != cfg.MaxMemoryMB {
		t.Errorf("MaxMemoryMB = %d, want %d", got.MaxMemoryMB, cfg.MaxMemoryMB)
	}
	if got.Binaries["linux_amd64"] != cfg.Binaries["linux_amd64"] {
		t.Errorf("Binaries mismatch")
	}
	if got.EnvVars["FOO"] != "bar" {
		t.Errorf("EnvVars mismatch")
	}
}

func TestExecutionConfigNullableFieldsJSON(t *testing.T) {
	// Nullable fields should round-trip correctly.
	image := "ubuntu:22.04"
	cfg := ExecutionConfig{
		Runtime: "CONTAINER",
		Image:   &image,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExecutionConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Image == nil || *got.Image != image {
		t.Errorf("Image = %v, want %q", got.Image, image)
	}
	if got.Dockerfile != nil {
		t.Errorf("Dockerfile should be nil, got %v", got.Dockerfile)
	}
}

func TestValidationConfigJSON(t *testing.T) {
	tol := 0.001
	cfg := ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "NUMERIC_TOLERANCE",
		NumericTolerance:   &tol,
		MaxRetries:         3,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ValidationConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.RedundancyFactor != 2 {
		t.Errorf("RedundancyFactor = %d, want 2", got.RedundancyFactor)
	}
	if got.NumericTolerance == nil || *got.NumericTolerance != tol {
		t.Errorf("NumericTolerance = %v, want %v", got.NumericTolerance, tol)
	}
	if got.CustomComparatorRef != nil {
		t.Errorf("CustomComparatorRef should be nil")
	}
}

func TestFaultToleranceConfigJSON(t *testing.T) {
	interval := 600
	cfg := FaultToleranceConfig{
		HeartbeatIntervalSeconds:  300,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
		CheckpointingEnabled:      true,
		CheckpointIntervalSeconds: &interval,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got FaultToleranceConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.HeartbeatIntervalSeconds != 300 {
		t.Errorf("HeartbeatIntervalSeconds = %d, want 300", got.HeartbeatIntervalSeconds)
	}
	if got.CheckpointIntervalSeconds == nil || *got.CheckpointIntervalSeconds != 600 {
		t.Errorf("CheckpointIntervalSeconds = %v, want 600", got.CheckpointIntervalSeconds)
	}
}

func TestDataConfigJSON(t *testing.T) {
	bucket := "my-bucket"
	cfg := DataConfig{
		TransferStrategy:   "s3",
		StorageBucket:      &bucket,
		AggregationFormat:  "JSON",
		SplittingConfig:    map[string]any{"chunk_size": float64(1000)},
		AggregationConfig:  map[string]any{},
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got DataConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.StorageBucket == nil || *got.StorageBucket != bucket {
		t.Errorf("StorageBucket = %v, want %q", got.StorageBucket, bucket)
	}
	if got.MaxInputSizeBytes != 1048576 {
		t.Errorf("MaxInputSizeBytes = %d, want 1048576", got.MaxInputSizeBytes)
	}
}

func TestCreditConfigJSON(t *testing.T) {
	cfg := CreditConfig{CreditPerValidatedWorkUnit: 1.5}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got CreditConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.CreditPerValidatedWorkUnit != 1.5 {
		t.Errorf("CreditPerValidatedWorkUnit = %f, want 1.5", got.CreditPerValidatedWorkUnit)
	}
}

func TestResourceRequirementsJSON(t *testing.T) {
	cap := "7.0"
	cfg := ResourceRequirements{
		MinCPUCores:          4,
		MinDiskMB:            20480,
		MinGPUVRAMMB:         8192,
		GPURequired:          true,
		GPUComputeCapability: &cap,
		MinBandwidthMbps:     10,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ResourceRequirements
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.MinCPUCores != 4 {
		t.Errorf("MinCPUCores = %d, want 4", got.MinCPUCores)
	}
	if !got.GPURequired {
		t.Errorf("GPURequired = false, want true")
	}
	if got.GPUComputeCapability == nil || *got.GPUComputeCapability != "7.0" {
		t.Errorf("GPUComputeCapability = %v, want 7.0", got.GPUComputeCapability)
	}
}

func TestProjectJSONRoundTrip(t *testing.T) {
	id := uuid.New()
	creatorID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)

	p := Leaf{
		ID:          id,
		Name:        "Test Project",
		Slug:        "test-project",
		Description: "A test leaf for unit testing",
		ResearchArea: []string{"physics", "ml-ai"},
		CreatorID:   &creatorID,
		State:       StateDraft,
		TaskPattern: PatternParameterSweep,
		ExecutionConfig: ExecutionConfig{
			Runtime:       "NATIVE",
			MaxMemoryMB:   4096,
			MaxDiskMB:     10240,
			MaxCPUSeconds: 86400,
			GPUType:       "ANY",
		},
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		FaultToleranceConfig: FaultToleranceConfig{
			HeartbeatIntervalSeconds:  300,
			MissedHeartbeatsThreshold: 3,
			DeadlineMultiplier:        3.0,
			MaxReassignments:          3,
		},
		DataConfig: DataConfig{
			TransferStrategy:   "INLINE",
			AggregationFormat:  "JSON",
			MaxInputSizeBytes:  1048576,
			MaxOutputSizeBytes: 104857600,
		},
		CreditConfig: CreditConfig{
			CreditPerValidatedWorkUnit: 1.0,
		},
		ResourceRequirements: ResourceRequirements{
			MinCPUCores: 1,
			MinDiskMB:   1024,
		},
		IsOngoing:  false,
		Visibility: VisibilityPublic,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify snake_case field names in JSON.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	for _, key := range []string{
		"id", "name", "slug", "description", "research_area",
		"creator_id", "state", "task_pattern",
		"execution_config", "validation_config", "fault_tolerance_config",
		"data_config", "credit_config", "resource_requirements",
		"is_ongoing", "visibility",
		"created_at", "updated_at",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("JSON missing snake_case key %q", key)
		}
	}

	// Round-trip back to struct.
	var got Leaf
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID = %v, want %v", got.ID, p.ID)
	}
	if got.Name != p.Name {
		t.Errorf("Name = %q, want %q", got.Name, p.Name)
	}
	if got.State != p.State {
		t.Errorf("State = %q, want %q", got.State, p.State)
	}
	if got.TaskPattern != p.TaskPattern {
		t.Errorf("TaskPattern = %q, want %q", got.TaskPattern, p.TaskPattern)
	}
}
