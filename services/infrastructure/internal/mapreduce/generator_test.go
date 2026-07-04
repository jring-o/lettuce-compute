package mapreduce

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Mock repos (same pattern as paramsweep) ---

type mockWorkUnitRepo struct {
	bulkCreateFn            func(ctx context.Context, wus []*workunit.WorkUnit) error
	bulkTransitionByBatchFn func(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error)
	bulkCreated             []*workunit.WorkUnit
}

func (m *mockWorkUnitRepo) Create(context.Context, *workunit.WorkUnit) error { return nil }
func (m *mockWorkUnitRepo) GetByID(context.Context, types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) List(context.Context, workunit.WorkUnitListFilters, types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockWorkUnitRepo) UpdateState(_ context.Context, id types.ID, _, to workunit.WorkUnitState) (*workunit.WorkUnit, error) {
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
func (m *mockWorkUnitRepo) FindNextAssignable(context.Context, workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ReserveNextAssignable(context.Context, workunit.AssignmentOptions, time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) Assign(context.Context, types.ID, types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindDispatchableBatch(context.Context, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClaimDispatchableBatch(context.Context, types.ID, time.Duration, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClearExpiredDispatchClaims(context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) ReleaseStaleBufferedCopies(context.Context, types.ID, []types.ID, time.Time) ([]types.ID, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FlushReservations(context.Context, []workunit.FlushReservation, types.ID, time.Duration) ([]workunit.FlushedCopy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CountActiveByVolunteer(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) Reassign(context.Context, types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (m *mockWorkUnitRepo) CountByLeafAndState(context.Context, types.ID, workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) MarkSpotCheck(context.Context, types.ID) error  { return nil }
func (m *mockWorkUnitRepo) EnsureWorkUnitHRClass(_ context.Context, _ types.ID, class string) (string, error) {
	return class, nil
}
func (m *mockWorkUnitRepo) ClearSpotCheck(context.Context, types.ID) error { return nil }
func (m *mockWorkUnitRepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ReserveCopy(context.Context, types.ID, types.ID, *types.ID, time.Time, int) (*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CountActiveByHost(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CloseCopy(context.Context, types.ID, string) error {
	return nil
}
func (m *mockWorkUnitRepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID) error {
	return nil
}
func (m *mockWorkUnitRepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountProbationLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountTotalCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountErrorCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (m *mockWorkUnitRepo) MarkCompleted(context.Context, types.ID) error           { return nil }
func (m *mockWorkUnitRepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	return false, nil
}

type mockBatchRepo struct {
	batches []*workunit.Batch
}

func (m *mockBatchRepo) Create(_ context.Context, b *workunit.Batch) error {
	b.ID = types.NewID()
	m.batches = append(m.batches, b)
	return nil
}
func (m *mockBatchRepo) GetByID(_ context.Context, id types.ID) (*workunit.Batch, error) {
	for _, b := range m.batches {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, apierror.NotFound("batch", id.String())
}
func (m *mockBatchRepo) ListByLeaf(_ context.Context, leafID types.ID, _ types.PaginationRequest) ([]*workunit.Batch, types.PaginationResponse, error) {
	var result []*workunit.Batch
	for _, b := range m.batches {
		if b.LeafID == leafID {
			result = append(result, b)
		}
	}
	return result, types.PaginationResponse{}, nil
}
func (m *mockBatchRepo) IncrementCompleted(context.Context, types.ID) error { return nil }

func newTestLeaf() *leaf.Leaf {
	strategy := "by_line_count"
	return &leaf.Leaf{
		ID:          types.NewID(),
		TaskPattern: leaf.PatternMapReduce,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:  "CONTAINER",
			Binaries: map[string]string{"linux-amd64": "sha256:abc123"},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 3.0,
			MaxReassignments:   3,
		},
		DataConfig: leaf.DataConfig{
			SplittingStrategy: &strategy,
			SplittingConfig:   map[string]any{"lines_per_chunk": float64(3)},
		},
	}
}

// --- Generate tests ---

func TestGenerate_InlineByLineCount(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 10 lines, 3 per chunk → 4 chunks (3,3,3,1)
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line"
	}
	inputData := strings.Join(lines, "\n") + "\n"

	params := map[string]interface{}{
		"input_data": inputData,
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 4 {
		t.Errorf("expected 4 work units, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 1 {
		t.Errorf("expected 1 batch, got %d", len(result.BatchIDs))
	}
	if result.Status != "complete" {
		t.Errorf("expected status 'complete', got %q", result.Status)
	}

	// Verify work unit fields.
	if len(wuRepo.bulkCreated) != 4 {
		t.Fatalf("expected 4 work units, got %d", len(wuRepo.bulkCreated))
	}
	for _, wu := range wuRepo.bulkCreated {
		if wu.LeafID != proj.ID {
			t.Errorf("wrong leaf_id")
		}
		if wu.State != workunit.WorkUnitStateCreated {
			t.Errorf("wrong state: %v", wu.State)
		}
		if wu.InputData == nil {
			t.Error("expected inline input_data, got nil")
		}
		if wu.InputDataRef != nil {
			t.Errorf("expected nil input_data_ref, got %v", *wu.InputDataRef)
		}
		if wu.Parameters == nil {
			t.Error("expected non-nil parameters (chunk metadata)")
		}
	}
}

func TestGenerate_ExternalReference(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data_ref": "https://storage.example.com/dataset.csv",
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 1 {
		t.Errorf("expected 1 work unit, got %d", result.WorkUnitsCreated)
	}
	if len(wuRepo.bulkCreated) != 1 {
		t.Fatalf("expected 1 work unit, got %d", len(wuRepo.bulkCreated))
	}

	wu := wuRepo.bulkCreated[0]
	if wu.InputDataRef == nil {
		t.Fatal("expected input_data_ref, got nil")
	}
	if *wu.InputDataRef != "https://storage.example.com/dataset.csv" {
		t.Errorf("unexpected ref: %s", *wu.InputDataRef)
	}
}

func TestGenerate_ExternalRefWithInlineData(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 6 lines, 3 per chunk → 2 chunks with refs.
	params := map[string]interface{}{
		"input_data":     "a\nb\nc\nd\ne\nf\n",
		"input_data_ref": "https://storage.example.com/dataset.csv",
	}

	result, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 2 {
		t.Errorf("expected 2 work units, got %d", result.WorkUnitsCreated)
	}

	for _, wu := range wuRepo.bulkCreated {
		if wu.InputDataRef == nil {
			t.Error("expected input_data_ref for ref+inline mode")
		}
		if wu.InputData != nil {
			t.Error("expected nil input_data for ref+inline mode")
		}
	}
}

func TestGenerate_MissingInputData(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for missing input data")
	}
}

func TestGenerate_NotMapReduce(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	proj.TaskPattern = leaf.PatternParameterSweep
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "data\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for non-MAP_REDUCE project")
	}
}

func TestGenerate_BatchSizing(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	// Set 1 line per chunk to get exactly 50 chunks.
	proj.DataConfig.SplittingConfig = map[string]any{"lines_per_chunk": float64(1)}
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	// 50 lines → 50 chunks, batch_size 20 → 3 batches (20, 20, 10).
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = "line"
	}
	inputData := strings.Join(lines, "\n") + "\n"

	params := map[string]interface{}{
		"input_data": inputData,
	}

	result, err := Generate(ctx, proj, params, 20, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 50 {
		t.Errorf("expected 50 work units, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 3 {
		t.Errorf("expected 3 batches, got %d", len(result.BatchIDs))
	}

	// Verify batch sizes.
	if len(batchRepo.batches) != 3 {
		t.Fatalf("expected 3 batches in repo, got %d", len(batchRepo.batches))
	}
	sizes := []int{
		batchRepo.batches[0].TotalWorkUnits,
		batchRepo.batches[1].TotalWorkUnits,
		batchRepo.batches[2].TotalWorkUnits,
	}
	if sizes[0] != 20 || sizes[1] != 20 || sizes[2] != 10 {
		t.Errorf("expected batch sizes [20, 20, 10], got %v", sizes)
	}
}

func TestGenerate_MissingSplittingStrategy(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	proj.DataConfig.SplittingStrategy = nil
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "data\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for missing splitting_strategy")
	}
}

func TestGenerate_InvalidSplittingStrategy(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	badStrategy := "unknown_strategy"
	proj.DataConfig.SplittingStrategy = &badStrategy
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "data\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for invalid splitting_strategy")
	}
}

func TestGenerate_EmptySplittingStrategy(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	empty := ""
	proj.DataConfig.SplittingStrategy = &empty
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "data\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for empty splitting_strategy")
	}
}

func TestGenerate_InputDataRefNonString(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data_ref": 12345, // not a string
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for non-string input_data_ref")
	}
}

func TestGenerate_BulkCreateError(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{
		bulkCreateFn: func(ctx context.Context, wus []*workunit.WorkUnit) error {
			return apierror.Internal("db error", nil)
		},
	}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "line1\nline2\nline3\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when BulkCreate fails")
	}
}

func TestGenerate_TransitionError(t *testing.T) {
	ctx := context.Background()
	proj := newTestLeaf()
	wuRepo := &mockWorkUnitRepo{
		bulkTransitionByBatchFn: func(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
			return 0, apierror.Internal("transition failed", nil)
		},
	}
	batchRepo := &mockBatchRepo{}

	params := map[string]interface{}{
		"input_data": "line1\nline2\n",
	}

	_, err := Generate(ctx, proj, params, 10000, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when transition fails")
	}
}

// --- extractRawData tests ---

func TestExtractRawData_String(t *testing.T) {
	data, err := extractRawData("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}
}

func TestExtractRawData_Bytes(t *testing.T) {
	input := []byte("raw bytes")
	data, err := extractRawData(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "raw bytes" {
		t.Errorf("expected 'raw bytes', got %q", string(data))
	}
}

func TestExtractRawData_JSON(t *testing.T) {
	input := map[string]any{"key": "value"}
	data, err := extractRawData(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != `{"key":"value"}` {
		t.Errorf("unexpected JSON: %s", string(data))
	}
}
