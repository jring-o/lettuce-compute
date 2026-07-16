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
func (r *mockWorkUnitRepo) CountProbationLiveCopies(context.Context, types.ID) (int, error) {
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

// fakeGenStore is a GenerationStore over the in-memory mock repos: it persists batches through a
// RepoBatchSink and applies cursor advances / exhaustion stamps to the mock leaf repo, guarded on
// total_generated exactly like the production pgx store (so the unit tests exercise the same
// advance/guard/exhaustion logic without a database).
type fakeGenStore struct {
	inner    *workunit.RepoBatchSink
	leafRepo *mockLeafRepo
}

func newFakeGenStore(wuRepo workunit.WorkUnitRepository, batchRepo workunit.BatchRepository, leafRepo *mockLeafRepo) *fakeGenStore {
	return &fakeGenStore{inner: workunit.NewRepoBatchSink(wuRepo, batchRepo), leafRepo: leafRepo}
}

func (s *fakeGenStore) NextSequenceNumber(ctx context.Context, leafID types.ID) (int, error) {
	return s.inner.NextSequenceNumber(ctx, leafID)
}

func (s *fakeGenStore) PersistBatch(ctx context.Context, batch *workunit.Batch, wus []*workunit.WorkUnit, cursor *workunit.GenerationCursorAdvance) error {
	if err := s.inner.PersistBatch(ctx, batch, wus, nil); err != nil {
		return err
	}
	if cursor != nil {
		ok, err := s.applyCursor(cursor.LeafID, cursor.Cursor, cursor.ExpectedPrevTotalGenerated)
		if err != nil {
			return err
		}
		if !ok {
			return ErrCursorConflict
		}
	}
	return nil
}

func (s *fakeGenStore) UpdateGenerationCursor(ctx context.Context, leafID types.ID, cursor []byte, expectedPrevTotalGenerated int64) (bool, error) {
	return s.applyCursor(leafID, cursor, expectedPrevTotalGenerated)
}

// applyCursor mirrors the guarded UPDATE: it writes only when the leaf's CURRENT cursor
// total_generated equals expectedPrev.
func (s *fakeGenStore) applyCursor(leafID types.ID, cursor []byte, expectedPrev int64) (bool, error) {
	p, ok := s.leafRepo.leafs[leafID]
	if !ok {
		return false, nil
	}
	current := loadCursor(p.GenerationCursor)
	if int64(current.TotalGenerated) != expectedPrev {
		return false, nil
	}
	p.GenerationCursor = append([]byte(nil), cursor...)
	return true, nil
}

// emittingGenerator is a GenerateFunc that emits exactly `count` work units through the sink in
// one batch (so the wrapping cursor-advancing sink actually advances the cursor). count 0 emits
// nothing (no batch), modelling an exhausted window.
func emittingGenerator(name string, count int) workunit.GenerateFunc {
	return func(ctx context.Context, proj *leaf.Leaf, parameterSpace map[string]interface{}, batchSize int, sink workunit.BatchSink) (*workunit.GenerateResult, error) {
		if count == 0 {
			return &workunit.GenerateResult{Status: name}, nil
		}
		seq, err := sink.NextSequenceNumber(ctx, proj.ID)
		if err != nil {
			return nil, err
		}
		batch := &workunit.Batch{LeafID: proj.ID, SequenceNumber: seq, TotalWorkUnits: count}
		wus := make([]*workunit.WorkUnit, count)
		for i := range wus {
			wus[i] = &workunit.WorkUnit{LeafID: proj.ID, State: workunit.WorkUnitStateCreated}
		}
		if err := sink.PersistBatch(ctx, batch, wus, nil); err != nil {
			return nil, err
		}
		return &workunit.GenerateResult{BatchIDs: []types.ID{batch.ID}, WorkUnitsCreated: count, Status: name}, nil
	}
}

func newTestLazyManager(wuRepo workunit.WorkUnitRepository, batchRepo workunit.BatchRepository, leafRepo *mockLeafRepo) *LazyManager {
	router := NewRouter(emittingGenerator("param_sweep", 1), emittingGenerator("map_reduce", 1), emittingGenerator("monte_carlo", 1), emittingGenerator("custom", 1), slog.Default())
	store := newFakeGenStore(wuRepo, batchRepo, leafRepo)
	return NewLazyManager(router, wuRepo, store, leafRepo, slog.Default())
}

// --- Tests ---

// cursorOf reads the leaf's durable generation_cursor column (where the reworked manager now
// persists progress — no longer inside splitting_config).
func cursorOf(leafRepo *mockLeafRepo, id types.ID) *GenerationCursor {
	return loadCursor(leafRepo.leafs[id].GenerationCursor)
}

func TestCheckAndGenerate_FiniteMonteCarlo(t *testing.T) {
	// 1000 trials, batch_size=100, threshold=50.
	proj := makeMonteCarloProject(false, 1000)

	leafRepo := newMockLeafRepo()
	leafRepo.leafs[proj.ID] = proj
	wuRepo := newMockWURepo()
	batchRepo := &mockBatchRepo{}
	mgr := newTestLazyManager(wuRepo, batchRepo, leafRepo)

	// The emitting generator returns 1 work unit, which is < lazy_batch_size (100), so the leaf
	// exhausts (it is finite / non-ongoing).
	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 1 {
		t.Errorf("expected 1 generated, got %d", generated)
	}

	cursor := cursorOf(leafRepo, proj.ID)
	if cursor.TotalGenerated != 1 {
		t.Errorf("expected total_generated=1, got %d", cursor.TotalGenerated)
	}
	if cursor.LastSeedOffset != 1 {
		t.Errorf("expected last_seed_offset=1, got %d", cursor.LastSeedOffset)
	}
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

	// Ongoing leaf: even though the tick produced < batch_size, it is NOT exhausted.
	cursor := cursorOf(leafRepo, proj.ID)
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

	// Verify the parameter-sweep offset advances.
	cursor := cursorOf(leafRepo, proj.ID)
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

func TestCheckAndGenerate_LazyMapReduceSkipped(t *testing.T) {
	// A pre-migration lazy MAP_REDUCE row (create/update now rejects the config) must be
	// skipped-and-WARNed by the manager rather than generating (design §4.10, BG-22b).
	strategy := "by_line_count"
	proj := &leaf.Leaf{
		ID:          types.NewID(),
		State:       leaf.StateActive,
		TaskPattern: leaf.PatternMapReduce,
		DataConfig: leaf.DataConfig{
			GenerationMode:    leaf.GenerationModeLazy,
			LazyThreshold:     50,
			LazyBatchSize:     100,
			SplittingStrategy: &strategy,
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
		t.Errorf("expected 0 generated for lazy map_reduce, got %d", generated)
	}
	if cursorOf(leafRepo, proj.ID).TotalGenerated != 0 {
		t.Error("expected no cursor advance for skipped lazy map_reduce")
	}
}

// TestCheckAndGenerate_FiniteMonteCarloNoNumTrialsSkipped (★BG-22e): a finite (non-ongoing)
// lazy Monte Carlo leaf with no readable num_trials has no total to exhaust against —
// pre-fix, perTickNumTrials silently fell back to a full batch and the leaf generated forever
// (GenerationExhausted could never trip). Create/update validation rejects the state; a row
// that reached it anyway (the pre-fix is_ongoing-flip bypass) must be skipped-and-WARNed, the
// lazy MAP_REDUCE posture, not silently generated.
func TestCheckAndGenerate_FiniteMonteCarloNoNumTrialsSkipped(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	delete(proj.DataConfig.SplittingConfig, "num_trials")

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
		t.Errorf("expected 0 generated for a finite MC leaf with no num_trials (undecidable exhaustion), got %d", generated)
	}
	if cursorOf(leafRepo, proj.ID).TotalGenerated != 0 {
		t.Error("expected no cursor advance for the skipped leaf")
	}

	// The ONGOING flavor of the same config is legal and generates a full-batch tick.
	ongoing := makeMonteCarloProject(true, 1000)
	delete(ongoing.DataConfig.SplittingConfig, "num_trials")
	leafRepo.leafs[ongoing.ID] = ongoing
	generated, err = mgr.CheckAndGenerate(context.Background(), ongoing.ID)
	if err != nil {
		t.Fatalf("unexpected error (ongoing): %v", err)
	}
	if generated == 0 {
		t.Error("ongoing MC with no num_trials must still generate (num_trials bounds nothing there)")
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

	// CheckAndGenerate itself doesn't check threshold — scanProjects does — so a direct call
	// still generates.
	generated, err := mgr.CheckAndGenerate(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated != 1 {
		t.Errorf("expected 1 generated, got %d", generated)
	}
}

func TestCheckAndGenerate_ExhaustedSkip(t *testing.T) {
	proj := makeMonteCarloProject(false, 1000)
	// Pre-set the durable cursor as exhausted.
	proj.GenerationCursor = []byte(`{"generation_exhausted":true,"total_generated":1000,"last_seed_offset":1000}`)

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

func TestCheckAndGenerate_MonteCarloPreCheckExhausts(t *testing.T) {
	// The cursor already covers the declared total N: the manager stamps exhausted WITHOUT
	// invoking the generator (design §4.6 pre-check).
	proj := makeMonteCarloProject(false, 200)
	proj.GenerationCursor = []byte(`{"total_generated":200,"last_seed_offset":200}`)

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
		t.Errorf("expected 0 generated when N already covered, got %d", generated)
	}
	if !cursorOf(leafRepo, proj.ID).GenerationExhausted {
		t.Error("expected generation_exhausted=true after pre-check")
	}
	if wuRepo.bulkCreated != 0 {
		t.Errorf("expected the generator NOT to run, but %d units were created", wuRepo.bulkCreated)
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
	if c := loadCursor(nil); c.TotalGenerated != 0 || c.GenerationExhausted {
		t.Error("expected empty cursor from nil column")
	}
	if c := loadCursor([]byte(`{}`)); c.TotalGenerated != 0 || c.GenerationExhausted {
		t.Error("expected empty cursor from {} column")
	}
}

func TestLoadCursor_FromColumn(t *testing.T) {
	// Unknown keys (e.g. an obsolete field from a migrated pre-fix cursor) are ignored.
	raw := []byte(`{"last_generated_offset":500,"last_seed_offset":300,"total_generated":800,"generation_exhausted":true,"obsolete_migrated_key":7}`)
	cursor := loadCursor(raw)
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

func TestBuildParameterSpace_MonteCarlo(t *testing.T) {
	// num_trials is NO LONGER overwritten with lazy_batch_size; it becomes the per-tick window
	// min(LazyBatchSize, remaining). With 950 already generated of 1000, remaining is 50.
	proj := makeMonteCarloProject(false, 1000)
	cursor := &GenerationCursor{LastSeedOffset: 950}

	params := buildParameterSpace(proj, cursor)
	if params["seed_offset"] != 950 {
		t.Errorf("expected seed_offset=950, got %v", params["seed_offset"])
	}
	if params["num_trials"] != 50 { // min(lazy_batch_size=100, remaining=50)
		t.Errorf("expected num_trials=50 (min of batch, remaining), got %v", params["num_trials"])
	}

	// Mid-run, remaining exceeds the batch: request a full batch.
	full := buildParameterSpace(proj, &GenerationCursor{LastSeedOffset: 100})
	if full["num_trials"] != 100 {
		t.Errorf("expected num_trials=100 when remaining >= batch, got %v", full["num_trials"])
	}
}

func TestBuildParameterSpace_MonteCarloOngoing(t *testing.T) {
	// An ongoing MC leaf requests a full batch every tick (num_trials bounds nothing).
	proj := makeMonteCarloProject(true, 0)
	params := buildParameterSpace(proj, &GenerationCursor{LastSeedOffset: 999999})
	if params["num_trials"] != 100 {
		t.Errorf("expected num_trials=100 for ongoing leaf, got %v", params["num_trials"])
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

	// Only the lazy leaf should have advanced its cursor (meaning generation ran).
	if cursorOf(leafRepo, lazyProj.ID).TotalGenerated == 0 {
		t.Error("expected lazy leaf to have generated work units")
	}
	if cursorOf(leafRepo, eagerProj.ID).TotalGenerated != 0 {
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

	// Leaf should NOT have been generated (above threshold).
	if cursorOf(leafRepo, proj.ID).TotalGenerated != 0 {
		t.Errorf("expected no generation when above threshold, got total_generated=%d",
			cursorOf(leafRepo, proj.ID).TotalGenerated)
	}
}
