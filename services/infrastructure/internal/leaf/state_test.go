package leaf

import (
	"context"
	"errors"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- ValidateTransition tests ---

func TestValidateTransition_ValidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from LeafState
		to   LeafState
	}{
		{"DRAFT -> CONFIGURING", StateDraft, StateConfiguring},
		{"CONFIGURING -> ACTIVE", StateConfiguring, StateActive},
		{"ACTIVE -> PAUSED", StateActive, StatePaused},
		{"PAUSED -> ACTIVE (resume)", StatePaused, StateActive},
		{"ACTIVE -> CONFIGURING", StateActive, StateConfiguring},
		{"PAUSED -> CONFIGURING", StatePaused, StateConfiguring},
		{"ACTIVE -> COMPLETED", StateActive, StateCompleted},
		{"COMPLETED -> ARCHIVED", StateCompleted, StateArchived},
		{"PAUSED -> ARCHIVED", StatePaused, StateArchived},
		{"DRAFT -> ARCHIVED", StateDraft, StateArchived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid transition %s -> %s, got error: %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestValidateTransition_InvalidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from LeafState
		to   LeafState
	}{
		{"DRAFT -> ACTIVE (skip CONFIGURING)", StateDraft, StateActive},
		{"DRAFT -> PAUSED", StateDraft, StatePaused},
		{"DRAFT -> COMPLETED", StateDraft, StateCompleted},
		{"ARCHIVED -> ACTIVE (no resurrection)", StateArchived, StateActive},
		{"ARCHIVED -> DRAFT", StateArchived, StateDraft},
		{"COMPLETED -> ACTIVE (no reactivation)", StateCompleted, StateActive},
		{"COMPLETED -> CONFIGURING", StateCompleted, StateConfiguring},
		{"CONFIGURING -> PAUSED (must activate first)", StateConfiguring, StatePaused},
		{"CONFIGURING -> COMPLETED", StateConfiguring, StateCompleted},
		{"CONFIGURING -> DRAFT (no going back)", StateConfiguring, StateDraft},
		{"ACTIVE -> ACTIVE (same state)", StateActive, StateActive},
		{"DRAFT -> DRAFT (same state)", StateDraft, StateDraft},
		{"CONFIGURING -> CONFIGURING (same state)", StateConfiguring, StateConfiguring},
		{"PAUSED -> PAUSED (same state)", StatePaused, StatePaused},
		{"COMPLETED -> COMPLETED (same state)", StateCompleted, StateCompleted},
		{"ARCHIVED -> ARCHIVED (same state)", StateArchived, StateArchived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTransition(tt.from, tt.to)
			if err == nil {
				t.Errorf("expected error for invalid transition %s -> %s, got nil", tt.from, tt.to)
				return
			}
			var apiErr *apierror.APIError
			if !errors.As(err, &apiErr) {
				t.Errorf("expected *apierror.APIError, got %T", err)
				return
			}
			if apiErr.HTTPStatus != 409 {
				t.Errorf("expected HTTP 409, got %d", apiErr.HTTPStatus)
			}
			details, ok := apiErr.Details.(map[string]string)
			if !ok {
				t.Errorf("expected map[string]string details, got %T", apiErr.Details)
				return
			}
			if details["code"] != "INVALID_STATE_TRANSITION" {
				t.Errorf("expected code INVALID_STATE_TRANSITION, got %s", details["code"])
			}
			if details["from"] != string(tt.from) {
				t.Errorf("expected from=%s, got %s", tt.from, details["from"])
			}
			if details["to"] != string(tt.to) {
				t.Errorf("expected to=%s, got %s", tt.to, details["to"])
			}
		})
	}
}

// --- CanActivate tests ---

func validProject() *Leaf {
	return &Leaf{
		TaskPattern: PatternParameterSweep,
		ExecutionConfig: ExecutionConfig{
			Runtime:         "NATIVE",
			Binaries:        map[string]string{"linux-amd64": "https://example.com/bin"},
			BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
			GPUType:         "ANY",
			MaxMemoryMB:     4096,
			MaxDiskMB:       10240,
			MaxCPUSeconds:   86400,
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
	}
}

func TestCanActivate_AllConfigsValid(t *testing.T) {
	p := validProject()
	if err := CanActivate(p); err != nil {
		t.Errorf("expected no error for valid config, got: %v", err)
	}
}

func TestCanActivate_InvalidExecutionConfig(t *testing.T) {
	p := validProject()
	p.ExecutionConfig.Runtime = "" // invalid

	err := CanActivate(p)
	assertConfigIncomplete(t, err, "execution_config")
}

func TestCanActivate_InvalidValidationConfig(t *testing.T) {
	p := validProject()
	p.ValidationConfig.RedundancyFactor = 0 // invalid

	err := CanActivate(p)
	assertConfigIncomplete(t, err, "validation_config")
}

func TestCanActivate_InvalidFaultToleranceConfig(t *testing.T) {
	p := validProject()
	p.FaultToleranceConfig.HeartbeatIntervalSeconds = 0 // invalid

	err := CanActivate(p)
	assertConfigIncomplete(t, err, "fault_tolerance_config")
}

func TestCanActivate_InvalidDataConfig(t *testing.T) {
	p := validProject()
	p.DataConfig.TransferStrategy = "bogus" // invalid

	err := CanActivate(p)
	assertConfigIncomplete(t, err, "data_config")
}

func TestCanActivate_MultipleConfigsInvalid(t *testing.T) {
	p := validProject()
	p.ExecutionConfig.Runtime = ""                       // invalid
	p.FaultToleranceConfig.HeartbeatIntervalSeconds = 0  // invalid

	err := CanActivate(p)
	if err == nil {
		t.Fatal("expected error for multiple invalid configs, got nil")
	}

	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.Code != "CONFIGURATION_INCOMPLETE" {
		t.Errorf("expected code CONFIGURATION_INCOMPLETE, got %s", apiErr.Code)
	}

	failures, ok := apiErr.Details.([]configFailure)
	if !ok {
		t.Fatalf("expected []configFailure details, got %T", apiErr.Details)
	}
	if len(failures) != 2 {
		t.Errorf("expected 2 failures, got %d", len(failures))
	}

	configs := map[string]bool{}
	for _, f := range failures {
		configs[f.Config] = true
	}
	if !configs["execution_config"] {
		t.Error("expected execution_config in failures")
	}
	if !configs["fault_tolerance_config"] {
		t.Error("expected fault_tolerance_config in failures")
	}
}

func assertConfigIncomplete(t *testing.T, err error, expectedConfig string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error for invalid %s, got nil", expectedConfig)
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.Code != "CONFIGURATION_INCOMPLETE" {
		t.Errorf("expected code CONFIGURATION_INCOMPLETE, got %s", apiErr.Code)
	}
	if apiErr.HTTPStatus != 400 {
		t.Errorf("expected HTTP 400, got %d", apiErr.HTTPStatus)
	}
	failures, ok := apiErr.Details.([]configFailure)
	if !ok {
		t.Fatalf("expected []configFailure details, got %T", apiErr.Details)
	}
	found := false
	for _, f := range failures {
		if f.Config == expectedConfig {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %s in failure list, got %v", expectedConfig, failures)
	}
}

// --- CanDelete tests (unit-level, no DB) ---
// CanDelete requires a pgxpool.Pool, so full tests are integration tests.
// Here we test the state guard (ACTIVE rejection) without a DB connection.

func TestCanDelete_ActiveProjectRejected(t *testing.T) {
	// CanDelete checks state first, before DB query, so this works without a pool.
	err := CanDelete(context.Background(), nil, types.NewID(), StateActive)
	if err == nil {
		t.Fatal("expected error deleting active project, got nil")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("expected HTTP 409, got %d", apiErr.HTTPStatus)
	}
	if apiErr.Message != "cannot delete active leaf; pause and archive first" {
		t.Errorf("unexpected message: %s", apiErr.Message)
	}
}

// --- Ongoing project tests ---

func TestOngoingProject_CannotComplete(t *testing.T) {
	p := validProject()
	p.State = StateActive
	p.IsOngoing = true

	err := ValidateTransition(p.State, StateCompleted)
	if err != nil {
		t.Fatalf("transition should be structurally valid: %v", err)
	}

	// TransitionLeaf should catch the ongoing constraint
	repo := &mockRepository{}
	err = TransitionLeaf(context.Background(), repo, p, StateCompleted)
	if err == nil {
		t.Fatal("expected error completing ongoing project, got nil")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("expected HTTP 409, got %d", apiErr.HTTPStatus)
	}
	if apiErr.Message != "ongoing leafs cannot be completed" {
		t.Errorf("unexpected message: %s", apiErr.Message)
	}
	if repo.updateCalled {
		t.Error("repo.Update should not have been called")
	}
}

func TestNonOngoingProject_CanComplete(t *testing.T) {
	p := validProject()
	p.State = StateActive
	p.IsOngoing = false

	repo := &mockRepository{}
	err := TransitionLeaf(context.Background(), repo, p, StateCompleted)
	if err != nil {
		t.Errorf("expected success completing non-ongoing project, got: %v", err)
	}
	if !repo.updateCalled {
		t.Error("repo.Update should have been called")
	}
	if p.State != StateCompleted {
		t.Errorf("expected state COMPLETED, got %s", p.State)
	}
}

// --- TransitionLeaf orchestration tests ---

func TestTransitionLeaf_HappyPath_ConfiguringToActive(t *testing.T) {
	p := validProject()
	p.State = StateConfiguring

	repo := &mockRepository{}
	err := TransitionLeaf(context.Background(), repo, p, StateActive)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if p.State != StateActive {
		t.Errorf("expected state ACTIVE, got %s", p.State)
	}
	if !repo.updateCalled {
		t.Error("repo.Update should have been called")
	}
}

func TestTransitionLeaf_InvalidTransition(t *testing.T) {
	p := validProject()
	p.State = StateDraft

	repo := &mockRepository{}
	err := TransitionLeaf(context.Background(), repo, p, StateActive)
	if err == nil {
		t.Fatal("expected error for invalid transition, got nil")
	}
	if repo.updateCalled {
		t.Error("repo.Update should not have been called for invalid transition")
	}
	if p.State != StateDraft {
		t.Errorf("state should not have changed, got %s", p.State)
	}
}

func TestTransitionLeaf_ConfigIncomplete(t *testing.T) {
	p := validProject()
	p.State = StateConfiguring
	p.ExecutionConfig.Runtime = "" // invalid

	repo := &mockRepository{}
	err := TransitionLeaf(context.Background(), repo, p, StateActive)
	if err == nil {
		t.Fatal("expected error for incomplete config, got nil")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.Code != "CONFIGURATION_INCOMPLETE" {
		t.Errorf("expected code CONFIGURATION_INCOMPLETE, got %s", apiErr.Code)
	}
	if repo.updateCalled {
		t.Error("repo.Update should not have been called for incomplete config")
	}
	if p.State != StateConfiguring {
		t.Errorf("state should not have changed, got %s", p.State)
	}
}

func TestTransitionLeaf_ResumeSkipsActivationCheck(t *testing.T) {
	// PAUSED -> ACTIVE should NOT re-check configs.
	// Use invalid config to prove it doesn't check.
	p := validProject()
	p.State = StatePaused
	p.ExecutionConfig.Runtime = "" // would fail CanActivate

	repo := &mockRepository{}
	err := TransitionLeaf(context.Background(), repo, p, StateActive)
	if err != nil {
		t.Fatalf("resume should skip activation check, got: %v", err)
	}
	if p.State != StateActive {
		t.Errorf("expected state ACTIVE, got %s", p.State)
	}
	if !repo.updateCalled {
		t.Error("repo.Update should have been called")
	}
}

func TestTransitionLeaf_RepoUpdateError_RestoresState(t *testing.T) {
	p := validProject()
	p.State = StateActive

	repo := &mockRepository{updateErr: errors.New("db connection lost")}
	err := TransitionLeaf(context.Background(), repo, p, StatePaused)
	if err == nil {
		t.Fatal("expected error when repo.Update fails, got nil")
	}
	if p.State != StateActive {
		t.Errorf("state should be rolled back to ACTIVE on Update failure, got %s", p.State)
	}
}

// --- Mock repository ---

type mockRepository struct {
	updateCalled bool
	updateErr    error
}

func (m *mockRepository) Create(_ context.Context, _ *Leaf) error {
	return nil
}

func (m *mockRepository) GetByID(_ context.Context, _ types.ID) (*Leaf, error) {
	return nil, nil
}

func (m *mockRepository) GetBySlug(_ context.Context, _ string, _ *types.ID) (*Leaf, error) {
	return nil, nil
}

func (m *mockRepository) GetBySlugPublic(_ context.Context, _ string) (*Leaf, error) {
	return nil, nil
}

func (m *mockRepository) List(_ context.Context, _ LeafListFilters, _ types.PaginationRequest) ([]*Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

func (m *mockRepository) Update(_ context.Context, _ *Leaf) error {
	m.updateCalled = true
	return m.updateErr
}

func (m *mockRepository) Delete(_ context.Context, _ types.ID) error {
	return nil
}
