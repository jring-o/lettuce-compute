package generate

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// mockWorkUnitRepo implements workunit.WorkUnitRepository for lazy manager tests.
type mockWorkUnitRepo struct {
	queuedCounts map[types.ID]int64 // leaf_id -> queued count
	bulkCreated  int
}

func newMockWURepo() *mockWorkUnitRepo {
	return &mockWorkUnitRepo{
		queuedCounts: make(map[types.ID]int64),
	}
}

func (r *mockWorkUnitRepo) CountByLeafAndState(_ context.Context, leafID types.ID, state workunit.WorkUnitState) (int64, error) {
	if state == workunit.WorkUnitStateQueued {
		return r.queuedCounts[leafID], nil
	}
	return 0, nil
}

func (r *mockWorkUnitRepo) Create(_ context.Context, _ *workunit.WorkUnit) error { return nil }
func (r *mockWorkUnitRepo) EnsureWorkUnitHRClass(_ context.Context, _ types.ID, class string) (string, error) {
	return class, nil
}
func (r *mockWorkUnitRepo) GetByID(_ context.Context, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) List(_ context.Context, _ workunit.WorkUnitListFilters, _ types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (r *mockWorkUnitRepo) UpdateState(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) BulkCreate(_ context.Context, wus []*workunit.WorkUnit) error {
	r.bulkCreated += len(wus)
	return nil
}
func (r *mockWorkUnitRepo) BulkTransitionByBatch(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (r *mockWorkUnitRepo) FindNextAssignable(_ context.Context, _ workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) ReserveNextAssignable(_ context.Context, _ workunit.AssignmentOptions, _ time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) Assign(_ context.Context, _, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) FindDispatchableBatch(_ context.Context, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) ClaimDispatchableBatch(_ context.Context, _ types.ID, _ time.Duration, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	return 0, nil
}
func (r *mockWorkUnitRepo) ReleaseStaleBufferedCopies(context.Context, types.ID, []types.ID, time.Time) ([]types.ID, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) FlushReservations(_ context.Context, _ []workunit.FlushReservation, _ types.ID, _ time.Duration) ([]workunit.FlushedCopy, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) Reassign(_ context.Context, _ types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (r *mockWorkUnitRepo) MarkSpotCheck(_ context.Context, _ types.ID) error  { return nil }
func (r *mockWorkUnitRepo) ClearSpotCheck(_ context.Context, _ types.ID) error { return nil }
func (r *mockWorkUnitRepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) ReserveCopy(context.Context, types.ID, types.ID, *types.ID, time.Time, int) (*workunit.Copy, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) CountActiveByHost(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *mockWorkUnitRepo) CloseCopy(context.Context, types.ID, string) error {
	return nil
}
func (r *mockWorkUnitRepo) CountErrorCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (r *mockWorkUnitRepo) MarkCompleted(context.Context, types.ID) error           { return nil }
func (r *mockWorkUnitRepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID) error {
	return nil
}
func (r *mockWorkUnitRepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}
func (r *mockWorkUnitRepo) CountLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (r *mockWorkUnitRepo) CountTotalCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (r *mockWorkUnitRepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	return false, nil
}

// mockBatchRepo implements workunit.BatchRepository for lazy manager tests.
type mockBatchRepo struct {
	batches []*workunit.Batch
}

func (r *mockBatchRepo) Create(_ context.Context, b *workunit.Batch) error {
	b.ID = types.NewID()
	r.batches = append(r.batches, b)
	return nil
}
func (r *mockBatchRepo) GetByID(_ context.Context, _ types.ID) (*workunit.Batch, error) {
	return nil, nil
}
func (r *mockBatchRepo) ListByLeaf(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*workunit.Batch, types.PaginationResponse, error) {
	return r.batches, types.PaginationResponse{}, nil
}
func (r *mockBatchRepo) IncrementCompleted(_ context.Context, _ types.ID) error { return nil }

// mockLeafRepo implements leaf.Repository for lazy manager tests.
type mockLeafRepo struct {
	leafs map[types.ID]*leaf.Leaf
}

func newMockLeafRepo() *mockLeafRepo {
	return &mockLeafRepo{
		leafs: make(map[types.ID]*leaf.Leaf),
	}
}

func (r *mockLeafRepo) Create(_ context.Context, p *leaf.Leaf) error {
	r.leafs[p.ID] = p
	return nil
}
func (r *mockLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	p, ok := r.leafs[id]
	if !ok {
		return nil, nil
	}
	return p, nil
}
func (r *mockLeafRepo) GetBySlug(_ context.Context, _ string, _ *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (r *mockLeafRepo) GetBySlugPublic(_ context.Context, _ string) (*leaf.Leaf, error) {
	return nil, nil
}
func (r *mockLeafRepo) List(_ context.Context, _ leaf.LeafListFilters, _ types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	var result []*leaf.Leaf
	for _, p := range r.leafs {
		result = append(result, p)
	}
	return result, types.PaginationResponse{}, nil
}
func (r *mockLeafRepo) Update(_ context.Context, p *leaf.Leaf) error {
	r.leafs[p.ID] = p
	return nil
}
func (r *mockLeafRepo) Delete(_ context.Context, _ types.ID) error { return nil }

// --- Helper ---

func makeMonteCarloProject(isOngoing bool, numTrials int) *leaf.Leaf {
	return &leaf.Leaf{
		ID:          types.NewID(),
		State:       leaf.StateActive,
		TaskPattern: leaf.PatternMonteCarlo,
		IsOngoing:   isOngoing,
		DataConfig: leaf.DataConfig{
			GenerationMode: leaf.GenerationModeLazy,
			LazyThreshold:  50,
			LazyBatchSize:  100,
			SplittingConfig: map[string]any{
				"num_trials":    float64(numTrials),
				"seed_strategy": "sequential",
			},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			MaxReassignments:  3,
			DeadlineMultiplier: 3.0,
		},
		ExecutionConfig: leaf.ExecutionConfig{
			Binaries: map[string]string{"linux_amd64": "https://example.com/bin"},
		},
	}
}

func makeParamSweepProject() *leaf.Leaf {
	return &leaf.Leaf{
		ID:          types.NewID(),
		State:       leaf.StateActive,
		TaskPattern: leaf.PatternParameterSweep,
		IsOngoing:   false,
		DataConfig: leaf.DataConfig{
			GenerationMode: leaf.GenerationModeLazy,
			LazyThreshold:  50,
			LazyBatchSize:  100,
			SplittingConfig: map[string]any{
				"x": []interface{}{1.0, 2.0, 3.0, 4.0, 5.0},
				"y": []interface{}{10.0, 20.0, 30.0, 40.0, 50.0},
			},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			MaxReassignments:  3,
			DeadlineMultiplier: 3.0,
		},
		ExecutionConfig: leaf.ExecutionConfig{
			Binaries: map[string]string{"linux_amd64": "https://example.com/bin"},
		},
	}
}

func newTestLazyManager(wuRepo workunit.WorkUnitRepository, batchRepo workunit.BatchRepository, leafRepo leaf.Repository) *LazyManager {
	router := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	return NewLazyManager(router, wuRepo, batchRepo, leafRepo, slog.Default())
}

// --- Tests ---

func TestCheckAndGenerate_FiniteMonteCarlo(t *testing.T) {
	// 1000 trials, batch_size=100, threshold=50.
	proj := makeMonteCarloProject(false, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	// Generate first batch. Since stub returns 1 work unit, it will be < lazy_batch_size,
	// meaning exhausted should be set to true (since not ongoing).
	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 1 { // stub generator returns 1
		t.Errorf("expected 1 generated, got %d", generated)
	}

	// Verify cursor was updated.
	updated := leafRepo.leafs[proj.ID]
	cursor := loadCursor(updated.DataConfig.SplittingConfig)
	if cursor.TotalGenerated != 1 {
		t.Errorf("expected total_generated=1, got %d", cursor.TotalGenerated)
	}
	if cursor.LastSeedOffset != 1 {
		t.Errorf("expected last_seed_offset=1, got %d", cursor.LastSeedOffset)
	}
	// Stub returns 1, which is < lazy_batch_size=100, so exhausted for finite leaf.
	if !cursor.GenerationExhausted {
		t.Error("expected generation_exhausted=true for finite leaf")
	}
}

func TestCheckAndGenerate_OngoingMonteCarlo(t *testing.T) {
	proj := makeMonteCarloProject(true, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 1 {
		t.Errorf("expected 1 generated, got %d", generated)
	}

	// Ongoing leaf: even though stub returns < batch_size, NOT exhausted.
	updated := leafRepo.leafs[proj.ID]
	cursor := loadCursor(updated.DataConfig.SplittingConfig)
	if cursor.GenerationExhausted {
		t.Error("expected generation_exhausted=false for ongoing leaf")
	}
}

func TestCheckAndGenerate_ParamSweepCursor(t *testing.T) {
	proj := makeParamSweepProject()

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 1 {
		t.Errorf("expected 1 generated, got %d", generated)
	}

	// Verify cursor advances for param sweep.
	updated := leafRepo.leafs[proj.ID]
	cursor := loadCursor(updated.DataConfig.SplittingConfig)
	if cursor.LastGeneratedOffset != 1 {
		t.Errorf("expected last_generated_offset=1, got %d", cursor.LastGeneratedOffset)
	}
}

func TestCheckAndGenerate_CustomSkipped(t *testing.T) {
	proj := &leaf.Leaf{
		ID:          types.NewID(),
		State:       leaf.StateActive,
		TaskPattern: leaf.PatternCustom,
		DataConfig: leaf.DataConfig{
			GenerationMode: leaf.GenerationModeLazy,
			LazyThreshold:  50,
			LazyBatchSize:  100,
		},
	}

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 0 {
		t.Errorf("expected 0 generated for custom pattern, got %d", generated)
	}
}

func TestCheckAndGenerate_NotActive(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	proj.State = leaf.StatePaused

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 0 {
		t.Errorf("expected 0 generated for non-ACTIVE leaf, got %d", generated)
	}
}

func TestCheckAndGenerate_AboveThreshold(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	// Set queued count above threshold (50).
	wuRepo.queuedCounts[proj.ID] = 100
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	// scanProjects won't call CheckAndGenerate since count >= threshold.
	// But CheckAndGenerate itself doesn't check threshold — scanProjects does.
	// Verify CheckAndGenerate works regardless.
	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// CheckAndGenerate generates even when called directly (threshold check is in scanProjects).
	if generated != 1 {
		t.Errorf("expected 1 generated, got %d", generated)
	}
}

func TestCheckAndGenerate_ExhaustedSkip(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	// Pre-set cursor as exhausted.
	proj.DataConfig.SplittingConfig["_cursor"] = map[string]any{
		"generation_exhausted": true,
		"total_generated":      1000,
	}

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 0 {
		t.Errorf("expected 0 generated for exhausted leaf, got %d", generated)
	}
}

func TestCheckAndGenerate_EagerProjectSkipped(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	proj.DataConfig.GenerationMode = leaf.GenerationModeEager

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 0 {
		t.Errorf("expected 0 generated for eager leaf, got %d", generated)
	}
}

func TestLoadCursor_Empty(t *testing.T) {
	cursor := loadCursor(nil)
	if cursor.TotalGenerated != 0 || cursor.GenerationExhausted {
		t.Error("expected empty cursor from nil config")
	}
}

func TestLoadCursor_FromConfig(t *testing.T) {
	config := map[string]any{
		"_cursor": map[string]any{
			"last_generated_offset": float64(500),
			"last_seed_offset":      float64(300),
			"total_generated":       float64(800),
			"generation_exhausted":  true,
		},
	}
	cursor := loadCursor(config)
	if cursor.LastGeneratedOffset != 500 {
		t.Errorf("expected last_generated_offset=500, got %d", cursor.LastGeneratedOffset)
	}
	if cursor.LastSeedOffset != 300 {
		t.Errorf("expected last_seed_offset=300, got %d", cursor.LastSeedOffset)
	}
	if cursor.TotalGenerated != 800 {
		t.Errorf("expected total_generated=800, got %d", cursor.TotalGenerated)
	}
	if !cursor.GenerationExhausted {
		t.Error("expected generation_exhausted=true")
	}
}

func TestSaveCursor_RoundTrip(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	cursor := &GenerationCursor{
		LastSeedOffset:      500,
		TotalGenerated:      500,
		GenerationExhausted: false,
	}

	saveCursor(proj, cursor)
	loaded := loadCursor(proj.DataConfig.SplittingConfig)

	if loaded.LastSeedOffset != 500 {
		t.Errorf("expected last_seed_offset=500, got %d", loaded.LastSeedOffset)
	}
	if loaded.TotalGenerated != 500 {
		t.Errorf("expected total_generated=500, got %d", loaded.TotalGenerated)
	}
	if loaded.GenerationExhausted {
		t.Error("expected generation_exhausted=false")
	}
}

func TestBuildParameterSpace_MonteCarlo(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	cursor := &GenerationCursor{LastSeedOffset: 500}

	params := buildParameterSpace(proj, cursor)
	if params["seed_offset"] != 500 {
		t.Errorf("expected seed_offset=500, got %v", params["seed_offset"])
	}
	if params["num_trials"] != 100 { // lazy_batch_size
		t.Errorf("expected num_trials=100, got %v", params["num_trials"])
	}
}

func TestBuildParameterSpace_ParamSweep(t *testing.T) {
	proj := makeParamSweepProject()
	cursor := &GenerationCursor{LastGeneratedOffset: 10}

	params := buildParameterSpace(proj, cursor)
	if params["_offset"] != 10 {
		t.Errorf("expected _offset=10, got %v", params["_offset"])
	}
	// Original params should be preserved.
	if params["x"] == nil {
		t.Error("expected x parameter to be preserved")
	}
}

func TestCheckAndGenerate_ProjectNotFound(t *testing.T) {
	leafRepo := newMockLeafRepo()
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	// Unknown leaf ID should return 0, nil (not panic).
	generated, err := mgr.CheckAndGenerate(context.Background(), types.NewID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 0 {
		t.Errorf("expected 0 generated for unknown leaf, got %d", generated)
	}
}

func TestScanProjects_FiltersLazyOnly(t *testing.T) {
	// Create one eager and one lazy leaf.
	eagerProj := makeMonteCarloProject(false, 1000)
	eagerProj.DataConfig.GenerationMode = leaf.GenerationModeEager

	lazyProj := makeMonteCarloProject(false, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[eagerProj.ID] = eagerProj
	leafRepo.leafs[lazyProj.ID] = lazyProj

	wuRepo := newMockWURepo()
	// Set both below threshold.
	wuRepo.queuedCounts[eagerProj.ID] = 0
	wuRepo.queuedCounts[lazyProj.ID] = 0

	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	// Run scanProjects directly.
	mgr.scanProjects(context.Background())

	// Only the lazy leaf should have a cursor (meaning generation ran).
	updatedLazy := leafRepo.leafs[lazyProj.ID]
	cursor := loadCursor(updatedLazy.DataConfig.SplittingConfig)
	if cursor.TotalGenerated == 0 {
		t.Error("expected lazy leaf to have generated work units")
	}

	updatedEager := leafRepo.leafs[eagerProj.ID]
	eagerCursor := loadCursor(updatedEager.DataConfig.SplittingConfig)
	if eagerCursor.TotalGenerated != 0 {
		t.Error("expected eager leaf to NOT have generated work units")
	}
}

func TestScanProjects_ThresholdGating(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj

	wuRepo := newMockWURepo()
	// Set queued count above threshold (50).
	wuRepo.queuedCounts[proj.ID] = 100

	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	mgr.scanProjects(context.Background())

	// Project should NOT have been generated (above threshold).
	updated := leafRepo.leafs[proj.ID]
	cursor := loadCursor(updated.DataConfig.SplittingConfig)
	if cursor.TotalGenerated != 0 {
		t.Errorf("expected no generation when above threshold, got total_generated=%d", cursor.TotalGenerated)
	}
}
