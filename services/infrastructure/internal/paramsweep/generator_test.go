package paramsweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- ParseParameterSpace unit tests ---

func TestParseParameterSpace_ExplicitListIntegers(t *testing.T) {
	raw := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200), float64(300)},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["temperature"]) != 3 {
		t.Errorf("expected 3 values, got %d", len(result["temperature"]))
	}
}

func TestParseParameterSpace_ExplicitListFloats(t *testing.T) {
	raw := map[string]interface{}{
		"ratio": []interface{}{0.1, 0.5, 0.9},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["ratio"]) != 3 {
		t.Errorf("expected 3 values, got %d", len(result["ratio"]))
	}
}

func TestParseParameterSpace_ExplicitListStrings(t *testing.T) {
	raw := map[string]interface{}{
		"method": []interface{}{"euler", "rk4"},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["method"]) != 2 {
		t.Errorf("expected 2 values, got %d", len(result["method"]))
	}
	if result["method"][0] != "euler" || result["method"][1] != "rk4" {
		t.Errorf("unexpected values: %v", result["method"])
	}
}

func TestParseParameterSpace_RangeObject(t *testing.T) {
	raw := map[string]interface{}{
		"pressure": map[string]interface{}{"min": 1.0, "max": 5.0, "step": 1.0},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []float64{1.0, 2.0, 3.0, 4.0, 5.0}
	if len(result["pressure"]) != len(expected) {
		t.Fatalf("expected %d values, got %d: %v", len(expected), len(result["pressure"]), result["pressure"])
	}
	for i, v := range expected {
		if result["pressure"][i] != v {
			t.Errorf("index %d: expected %v, got %v", i, v, result["pressure"][i])
		}
	}
}

func TestParseParameterSpace_RangeNonDivisibleStep(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 0.0, "max": 10.0, "step": 3.0},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0, 3, 6, 9 (12 > 10, so 10 not included)
	expected := []float64{0, 3, 6, 9}
	if len(result["x"]) != len(expected) {
		t.Fatalf("expected %d values, got %d: %v", len(expected), len(result["x"]), result["x"])
	}
	for i, v := range expected {
		if result["x"][i] != v {
			t.Errorf("index %d: expected %v, got %v", i, v, result["x"][i])
		}
	}
}

func TestParseParameterSpace_RangeExactFit(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 0.0, "max": 9.0, "step": 3.0},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []float64{0, 3, 6, 9}
	if len(result["x"]) != len(expected) {
		t.Fatalf("expected %d values, got %d: %v", len(expected), len(result["x"]), result["x"])
	}
}

func TestParseParameterSpace_RangeFloatPrecision(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 0.0, "max": 1.0, "step": 0.1},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0.0, 0.1, 0.2, ..., 1.0 = 11 values
	if len(result["x"]) != 11 {
		t.Fatalf("expected 11 values, got %d: %v", len(result["x"]), result["x"])
	}
	// Check that 1.0 is included (the last value).
	last := result["x"][10].(float64)
	if last != 1.0 {
		t.Errorf("expected last value to be 1.0, got %v", last)
	}
}

func TestParseParameterSpace_Mixed(t *testing.T) {
	raw := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200)},
		"pressure":    map[string]interface{}{"min": 1.0, "max": 3.0, "step": 1.0},
	}
	result, err := ParseParameterSpace(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["temperature"]) != 2 {
		t.Errorf("expected 2 temperature values, got %d", len(result["temperature"]))
	}
	if len(result["pressure"]) != 3 {
		t.Errorf("expected 3 pressure values, got %d", len(result["pressure"]))
	}
}

func TestParseParameterSpace_ErrorEmpty(t *testing.T) {
	_, err := ParseParameterSpace(map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for empty parameter space")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 400 {
		t.Errorf("expected HTTP 400, got %d", apiErr.HTTPStatus)
	}
}

func TestParseParameterSpace_ErrorEmptyList(t *testing.T) {
	raw := map[string]interface{}{
		"temperature": []interface{}{},
	}
	_, err := ParseParameterSpace(raw)
	if err == nil {
		t.Fatal("expected error for empty list")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
}

func TestParseParameterSpace_ErrorMinGreaterThanMax(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 10.0, "max": 1.0, "step": 1.0},
	}
	_, err := ParseParameterSpace(raw)
	if err == nil {
		t.Fatal("expected error for min > max")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
}

func TestParseParameterSpace_ErrorStepZero(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 0.0, "max": 10.0, "step": 0.0},
	}
	_, err := ParseParameterSpace(raw)
	if err == nil {
		t.Fatal("expected error for step = 0")
	}
}

func TestParseParameterSpace_ErrorMissingField(t *testing.T) {
	raw := map[string]interface{}{
		"x": map[string]interface{}{"min": 0.0, "max": 10.0},
	}
	_, err := ParseParameterSpace(raw)
	if err == nil {
		t.Fatal("expected error for missing step field")
	}
}

func TestParseParameterSpace_ErrorUnrecognizedFormat(t *testing.T) {
	raw := map[string]interface{}{
		"x": "not a list or range",
	}
	_, err := ParseParameterSpace(raw)
	if err == nil {
		t.Fatal("expected error for unrecognized format")
	}
}

// --- CartesianProduct unit tests ---

func TestCartesianProduct_SingleParam(t *testing.T) {
	params := map[string][]interface{}{
		"x": {1, 2, 3},
	}
	result := CartesianProduct(params)
	if len(result) != 3 {
		t.Fatalf("expected 3 combinations, got %d", len(result))
	}
	for i, combo := range result {
		if combo["x"] != params["x"][i] {
			t.Errorf("index %d: expected x=%v, got %v", i, params["x"][i], combo["x"])
		}
	}
}

func TestCartesianProduct_TwoParams(t *testing.T) {
	params := map[string][]interface{}{
		"a": {1, 2},
		"b": {"x", "y", "z"},
	}
	result := CartesianProduct(params)
	if len(result) != 6 {
		t.Fatalf("expected 6 combinations, got %d", len(result))
	}
	// With sorted keys: a, b. Should enumerate a first, then b.
	expected := []map[string]interface{}{
		{"a": 1, "b": "x"},
		{"a": 1, "b": "y"},
		{"a": 1, "b": "z"},
		{"a": 2, "b": "x"},
		{"a": 2, "b": "y"},
		{"a": 2, "b": "z"},
	}
	for i, combo := range result {
		if combo["a"] != expected[i]["a"] || combo["b"] != expected[i]["b"] {
			t.Errorf("index %d: expected %v, got %v", i, expected[i], combo)
		}
	}
}

func TestCartesianProduct_ThreeParams(t *testing.T) {
	params := map[string][]interface{}{
		"x": {1, 2},
		"y": {3, 4},
		"z": {5, 6},
	}
	result := CartesianProduct(params)
	if len(result) != 8 {
		t.Fatalf("expected 8 combinations, got %d", len(result))
	}
}

func TestCartesianProduct_SingleValueParam(t *testing.T) {
	params := map[string][]interface{}{
		"constant": {42},
		"x":        {1, 2},
	}
	result := CartesianProduct(params)
	if len(result) != 2 {
		t.Fatalf("expected 2 combinations, got %d", len(result))
	}
	for _, combo := range result {
		if combo["constant"] != 42 {
			t.Errorf("expected constant=42, got %v", combo["constant"])
		}
	}
}

func TestCartesianProduct_LargeSpace(t *testing.T) {
	params := map[string][]interface{}{
		"a": make([]interface{}, 10),
		"b": make([]interface{}, 10),
		"c": make([]interface{}, 10),
	}
	for i := 0; i < 10; i++ {
		params["a"][i] = i
		params["b"][i] = i + 10
		params["c"][i] = i + 20
	}
	result := CartesianProduct(params)
	if len(result) != 1000 {
		t.Fatalf("expected 1000 combinations, got %d", len(result))
	}
}

func TestCartesianProduct_DeterministicOrdering(t *testing.T) {
	params := map[string][]interface{}{
		"x": {1, 2},
		"y": {3, 4},
	}
	result1 := CartesianProduct(params)
	result2 := CartesianProduct(params)

	if len(result1) != len(result2) {
		t.Fatal("different lengths on repeated calls")
	}
	for i := range result1 {
		if fmt.Sprint(result1[i]) != fmt.Sprint(result2[i]) {
			t.Errorf("index %d: results differ: %v vs %v", i, result1[i], result2[i])
		}
	}
}

func TestCartesianProduct_Empty(t *testing.T) {
	result := CartesianProduct(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}

	result = CartesianProduct(map[string][]interface{}{})
	if result != nil {
		t.Errorf("expected nil for empty map, got %v", result)
	}
}

// --- Helper tests (shared helpers in generate package) ---

func TestResolveCodeArtifactRef_WithBinaries(t *testing.T) {
	proj := &leaf.Leaf{
		ExecutionConfig: leaf.ExecutionConfig{
			Binaries: map[string]string{
				"linux-amd64":  "sha256:linux123",
				"darwin-arm64": "sha256:darwin456",
			},
		},
	}
	ref := generate.ResolveCodeArtifactRef(proj)
	// Sorted alphabetically: darwin-arm64 comes first.
	if ref != "sha256:darwin456" {
		t.Errorf("expected sha256:darwin456, got %s", ref)
	}
}

func TestResolveCodeArtifactRef_NoBinaries(t *testing.T) {
	proj := &leaf.Leaf{
		ExecutionConfig: leaf.ExecutionConfig{},
	}
	ref := generate.ResolveCodeArtifactRef(proj)
	if ref != generate.PlaceholderArtifactRef {
		t.Errorf("expected placeholder, got %s", ref)
	}
}

func TestResolveDeadlineSeconds(t *testing.T) {
	proj := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 3.0,
		},
	}
	deadline := generate.ResolveDeadlineSeconds(proj)
	if deadline != 10800 { // 3600 * 3.0
		t.Errorf("expected 10800, got %d", deadline)
	}
}

func TestResolveDeadlineSeconds_ZeroMultiplier(t *testing.T) {
	proj := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 0,
		},
	}
	deadline := generate.ResolveDeadlineSeconds(proj)
	if deadline != 3600 { // fallback: 3600 * 1.0
		t.Errorf("expected 3600, got %d", deadline)
	}
}

func TestResolveDeadlineSeconds_NoDeadline(t *testing.T) {
	proj := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			NoDeadline:         true,
			DeadlineMultiplier: 3.0, // ignored when NoDeadline is set
		},
	}
	deadline := generate.ResolveDeadlineSeconds(proj)
	// Post-heartbeat-removal: NoDeadline stamps a synthetic reclaim ceiling (not 0)
	// so a unit on a vanished volunteer is always reclaimed by FindExpiredWorkUnits.
	if deadline != generate.NoDeadlineCeilingSeconds {
		t.Errorf("expected synthetic ceiling %d, got %d", generate.NoDeadlineCeilingSeconds, deadline)
	}
}

// --- Mock-based Generate tests (unit level) ---

// mockWorkUnitRepo implements workunit.WorkUnitRepository for testing.
type mockWorkUnitRepo struct {
	bulkCreateFn            func(ctx context.Context, wus []*workunit.WorkUnit) error
	updateStateFn           func(ctx context.Context, id types.ID, from, to workunit.WorkUnitState) (*workunit.WorkUnit, error)
	listFn                  func(ctx context.Context, filters workunit.WorkUnitListFilters, page types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error)
	bulkTransitionByBatchFn func(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error)

	bulkCreated []*workunit.WorkUnit // accumulates all BulkCreate calls
}

func (m *mockWorkUnitRepo) Create(ctx context.Context, wu *workunit.WorkUnit) error {
	return nil
}

func (m *mockWorkUnitRepo) GetByID(ctx context.Context, id types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}

func (m *mockWorkUnitRepo) List(ctx context.Context, filters workunit.WorkUnitListFilters, page types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filters, page)
	}
	// Default: return empty list (no work units to transition).
	return nil, types.PaginationResponse{}, nil
}

func (m *mockWorkUnitRepo) UpdateState(ctx context.Context, id types.ID, from, to workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	if m.updateStateFn != nil {
		return m.updateStateFn(ctx, id, from, to)
	}
	return &workunit.WorkUnit{ID: id, State: to}, nil
}

func (m *mockWorkUnitRepo) BulkCreate(ctx context.Context, wus []*workunit.WorkUnit) error {
	m.bulkCreated = append(m.bulkCreated, wus...)
	if m.bulkCreateFn != nil {
		return m.bulkCreateFn(ctx, wus)
	}
	return nil
}

func (m *mockWorkUnitRepo) BulkTransitionByBatch(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
	if m.bulkTransitionByBatchFn != nil {
		return m.bulkTransitionByBatchFn(ctx, batchID, from, to)
	}
	return 0, nil
}

func (m *mockWorkUnitRepo) FindNextAssignable(ctx context.Context, opts workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}

func (m *mockWorkUnitRepo) ReserveNextAssignable(ctx context.Context, opts workunit.AssignmentOptions, lease time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}

func (m *mockWorkUnitRepo) StampReservation(ctx context.Context, id, volunteerID types.ID, lease time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}

func (m *mockWorkUnitRepo) ClearReservation(ctx context.Context, id, volunteerID types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}

func (m *mockWorkUnitRepo) Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindExpiredWorkUnits(_ context.Context, _ int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindLapsedReservations(_ context.Context, _ int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindDispatchableBatch(_ context.Context, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClaimDispatchableBatch(_ context.Context, _ types.ID, _ time.Duration, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) FlushReservations(_ context.Context, _ []workunit.FlushReservation, _ types.ID, _ time.Duration) ([]types.ID, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) TransitionToExpired(_ context.Context, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) Reassign(_ context.Context, _ types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (m *mockWorkUnitRepo) CountByLeafAndState(_ context.Context, _ types.ID, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) MarkSpotCheck(_ context.Context, _ types.ID) error  { return nil }
func (m *mockWorkUnitRepo) ClearSpotCheck(_ context.Context, _ types.ID) error { return nil }
func (m *mockWorkUnitRepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}

// mockBatchRepo implements workunit.BatchRepository for testing.
type mockBatchRepo struct {
	batches []*workunit.Batch
}

func (m *mockBatchRepo) Create(ctx context.Context, b *workunit.Batch) error {
	b.ID = types.NewID()
	m.batches = append(m.batches, b)
	return nil
}

func (m *mockBatchRepo) GetByID(ctx context.Context, id types.ID) (*workunit.Batch, error) {
	for _, b := range m.batches {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, apierror.NotFound("batch", id.String())
}

func (m *mockBatchRepo) ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*workunit.Batch, types.PaginationResponse, error) {
	var result []*workunit.Batch
	for _, b := range m.batches {
		if b.LeafID == leafID {
			result = append(result, b)
		}
	}
	return result, types.PaginationResponse{}, nil
}

func (m *mockBatchRepo) IncrementCompleted(ctx context.Context, batchID types.ID) error {
	return nil
}

func newTestProject() *leaf.Leaf {
	return &leaf.Leaf{
		ID: types.NewID(),
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:  "NATIVE",
			Binaries: map[string]string{"linux-amd64": "sha256:abc123"},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 3.0,
			MaxReassignments:   3,
		},
	}
}

func TestGenerate_BasicSweep(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"temperature": []interface{}{float64(100), float64(200)},
		"pressure":    []interface{}{1.0, 2.0, 3.0},
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.WorkUnitsCreated != 6 {
		t.Errorf("expected 6 work units, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 1 {
		t.Errorf("expected 1 batch, got %d", len(result.BatchIDs))
	}
	if result.Status != "complete" {
		t.Errorf("expected status 'complete', got %q", result.Status)
	}
	// Verify work unit fields.
	if len(wuRepo.bulkCreated) != 6 {
		t.Fatalf("expected 6 bulk created, got %d", len(wuRepo.bulkCreated))
	}
	for _, wu := range wuRepo.bulkCreated {
		if wu.LeafID != proj.ID {
			t.Errorf("wrong leaf_id: %v", wu.LeafID)
		}
		if wu.State != workunit.WorkUnitStateCreated {
			t.Errorf("wrong state: %v", wu.State)
		}
		if wu.Priority != workunit.WorkUnitPriorityNormal {
			t.Errorf("wrong priority: %v", wu.Priority)
		}
		if wu.CodeArtifactRef != "sha256:abc123" {
			t.Errorf("wrong code_artifact_ref: %s", wu.CodeArtifactRef)
		}
		if wu.DeadlineSeconds != 10800 { // 3600 * 3.0
			t.Errorf("wrong deadline_seconds: %d", wu.DeadlineSeconds)
		}
		if wu.MaxReassignments != 3 {
			t.Errorf("wrong max_reassignments: %d", wu.MaxReassignments)
		}
		if wu.Parameters == nil {
			t.Error("parameters should not be nil")
		}
	}
}

func TestGenerate_BatchSplitting(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 5 x 5 = 25 work units, batch_size = 10 → 3 batches (10, 10, 5).
	vals := make([]interface{}, 5)
	for i := range vals {
		vals[i] = float64(i)
	}
	params := map[string]interface{}{
		"x": vals,
		"y": vals,
	}

	result, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.WorkUnitsCreated != 25 {
		t.Errorf("expected 25, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 3 {
		t.Errorf("expected 3 batches, got %d", len(result.BatchIDs))
	}

	// Verify batch sizes.
	if len(batchRepo.batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batchRepo.batches))
	}
	sizes := []int{
		batchRepo.batches[0].TotalWorkUnits,
		batchRepo.batches[1].TotalWorkUnits,
		batchRepo.batches[2].TotalWorkUnits,
	}
	sort.Ints(sizes)
	if sizes[0] != 5 || sizes[1] != 10 || sizes[2] != 10 {
		t.Errorf("expected batch sizes [5, 10, 10], got %v", sizes)
	}

	// Verify sequence numbers are sequential.
	seqNums := []int{
		batchRepo.batches[0].SequenceNumber,
		batchRepo.batches[1].SequenceNumber,
		batchRepo.batches[2].SequenceNumber,
	}
	if seqNums[0] != 1 || seqNums[1] != 2 || seqNums[2] != 3 {
		t.Errorf("expected sequence numbers [1, 2, 3], got %v", seqNums)
	}
}

func TestGenerate_LargeSpaceWarning(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 320 x 320 = 102400 > 100000.
	vals := make([]interface{}, 320)
	for i := range vals {
		vals[i] = float64(i)
	}
	params := map[string]interface{}{
		"x": vals,
		"y": vals,
	}

	result, err := Generate(ctx, proj, params, generate.MaxBatchSize, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Warning is now logged (not in result struct), so just verify count.
	if result.WorkUnitsCreated != 102400 {
		t.Errorf("expected 102400, got %d", result.WorkUnitsCreated)
	}
}

func TestGenerate_DefaultBatchSize(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}

	result, err := Generate(ctx, proj, params, 0, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 2 {
		t.Errorf("expected 2, got %d", result.WorkUnitsCreated)
	}
}

func TestGenerate_EmptyParams(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	_, err := Generate(ctx, proj, map[string]interface{}{}, 10, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for empty params")
	}
}

func TestGenerate_BulkCreateError(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{
		bulkCreateFn: func(ctx context.Context, wus []*workunit.WorkUnit) error {
			return apierror.Internal("simulated DB error", fmt.Errorf("connection lost"))
		},
	}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}

	_, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when BulkCreate fails")
	}
}

func TestGenerate_ParametersSerialized(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"alpha": []interface{}{1.0},
		"beta":  []interface{}{"yes"},
	}

	_, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(wuRepo.bulkCreated) != 1 {
		t.Fatalf("expected 1 work unit, got %d", len(wuRepo.bulkCreated))
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(wuRepo.bulkCreated[0].Parameters, &parsed); err != nil {
		t.Fatalf("failed to unmarshal parameters: %v", err)
	}
	if parsed["alpha"] != 1.0 {
		t.Errorf("expected alpha=1.0, got %v", parsed["alpha"])
	}
	if parsed["beta"] != "yes" {
		t.Errorf("expected beta=yes, got %v", parsed["beta"])
	}
}

func TestGenerate_TransitionsCreatedToQueued(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()

	var bulkTransitionCalled int
	wuRepo := &mockWorkUnitRepo{
		bulkTransitionByBatchFn: func(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
			if from != workunit.WorkUnitStateCreated || to != workunit.WorkUnitStateQueued {
				t.Errorf("unexpected transition: %s → %s", from, to)
			}
			bulkTransitionCalled++
			return 2, nil
		},
	}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"x": []interface{}{1.0, 2.0},
	}

	result, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 2 {
		t.Errorf("expected 2, got %d", result.WorkUnitsCreated)
	}
	if bulkTransitionCalled != 1 {
		t.Errorf("expected BulkTransitionByBatch called once, got %d", bulkTransitionCalled)
	}
}

func TestGenerate_TransitionError(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()

	wuRepo := &mockWorkUnitRepo{
		bulkTransitionByBatchFn: func(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
			return 0, apierror.Internal("simulated transition failure", fmt.Errorf("db down"))
		},
	}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"x": []interface{}{1.0},
	}

	_, err := Generate(ctx, proj, params, 10, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when transition fails")
	}
}

func TestGenerate_WithOffset_SkipsCombinations(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 3 x 2 = 6 combinations. Offset 4 → should generate 2 work units.
	params := map[string]interface{}{
		"x":       []interface{}{1.0, 2.0, 3.0},
		"y":       []interface{}{10.0, 20.0},
		"_offset": float64(4),
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 2 {
		t.Errorf("expected 2 work units (6 total - 4 offset), got %d", result.WorkUnitsCreated)
	}
}

func TestGenerate_WithOffset_BeyondTotalReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 2 combinations. Offset 5 → should return 0 work units.
	params := map[string]interface{}{
		"x":       []interface{}{1.0, 2.0},
		"_offset": float64(5),
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 0 {
		t.Errorf("expected 0 work units for offset beyond total, got %d", result.WorkUnitsCreated)
	}
	if result.Status != "complete" {
		t.Errorf("expected status 'complete', got %q", result.Status)
	}
}

func TestGenerate_WithOffset_Zero_NoSkip(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"x":       []interface{}{1.0, 2.0, 3.0},
		"_offset": float64(0),
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 3 {
		t.Errorf("expected 3 work units with offset=0, got %d", result.WorkUnitsCreated)
	}
}

func TestGenerate_WithOffset_StrippedFromParams(t *testing.T) {
	ctx := context.Background()
	proj := newTestProject()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// _offset should be stripped and not passed to ParseParameterSpace.
	params := map[string]interface{}{
		"x":       []interface{}{1.0, 2.0},
		"_offset": float64(0),
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 2 {
		t.Errorf("expected 2 work units, got %d", result.WorkUnitsCreated)
	}
}
