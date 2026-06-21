package montecarlo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Mock repositories ---

type mockWURepo struct {
	created       []*workunit.WorkUnit
	bulkCreateErr error
	bulkTransErr  error
}

func (m *mockWURepo) Create(_ context.Context, wu *workunit.WorkUnit) error {
	wu.ID = types.NewID()
	m.created = append(m.created, wu)
	return nil
}
func (m *mockWURepo) BulkCreate(_ context.Context, wus []*workunit.WorkUnit) error {
	if m.bulkCreateErr != nil {
		return m.bulkCreateErr
	}
	for _, wu := range wus {
		wu.ID = types.NewID()
	}
	m.created = append(m.created, wus...)
	return nil
}
func (m *mockWURepo) BulkTransitionByBatch(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (int64, error) {
	if m.bulkTransErr != nil {
		return 0, m.bulkTransErr
	}
	return 0, nil
}
func (m *mockWURepo) GetByID(context.Context, types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) List(context.Context, workunit.WorkUnitListFilters, types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockWURepo) UpdateState(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindNextAssignable(context.Context, workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) ReserveNextAssignable(context.Context, workunit.AssignmentOptions, time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) Assign(_ context.Context, _ types.ID, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) FindDispatchableBatch(context.Context, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClaimDispatchableBatch(context.Context, types.ID, time.Duration, int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWURepo) ClearExpiredDispatchClaims(context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) ReleaseStaleBufferedCopies(context.Context, types.ID, []types.ID, time.Time) ([]types.ID, error) {
	return nil, nil
}
func (m *mockWURepo) FlushReservations(context.Context, []workunit.FlushReservation, types.ID, time.Duration) ([]workunit.FlushedCopy, error) {
	return nil, nil
}
func (m *mockWURepo) CountActiveByVolunteer(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWURepo) Reassign(context.Context, types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, nil
}
func (m *mockWURepo) CountByLeafAndState(_ context.Context, _ types.ID, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWURepo) MarkSpotCheck(_ context.Context, _ types.ID) error  { return nil }
func (m *mockWURepo) EnsureWorkUnitHRClass(_ context.Context, _ types.ID, class string) (string, error) {
	return class, nil
}
func (m *mockWURepo) ClearSpotCheck(_ context.Context, _ types.ID) error { return nil }
func (m *mockWURepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}
func (m *mockWURepo) ReserveCopy(context.Context, types.ID, types.ID, *types.ID, time.Time, int) (*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWURepo) CountActiveByHost(context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWURepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWURepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWURepo) CloseCopy(context.Context, types.ID, string) error {
	return nil
}
func (m *mockWURepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID) error {
	return nil
}
func (m *mockWURepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}
func (m *mockWURepo) CountLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWURepo) CountTotalCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWURepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	return false, nil
}

type mockBatchRepo struct {
	batches       []*workunit.Batch
	createErr     error
	listByProjErr error
}

func (m *mockBatchRepo) Create(_ context.Context, b *workunit.Batch) error {
	if m.createErr != nil {
		return m.createErr
	}
	b.ID = types.NewID()
	m.batches = append(m.batches, b)
	return nil
}
func (m *mockBatchRepo) GetByID(context.Context, types.ID) (*workunit.Batch, error) {
	return nil, nil
}
func (m *mockBatchRepo) ListByLeaf(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*workunit.Batch, types.PaginationResponse, error) {
	if m.listByProjErr != nil {
		return nil, types.PaginationResponse{}, m.listByProjErr
	}
	return nil, types.PaginationResponse{}, nil
}
func (m *mockBatchRepo) IncrementCompleted(context.Context, types.ID) error { return nil }

// --- Tests ---

func testProject() *leaf.Leaf {
	return &leaf.Leaf{
		ID:          types.NewID(),
		TaskPattern: leaf.PatternMonteCarlo,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:  "NATIVE",
			Binaries: map[string]string{"linux_amd64": "https://example.com/bin"},
		},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			DeadlineMultiplier: 3.0,
			MaxReassignments:   3,
		},
	}
}

func TestGenerate(t *testing.T) {
	tests := []struct {
		name           string
		paramSpace     map[string]interface{}
		projConfig     map[string]any
		batchSize      int
		wantBatches    int
		wantWUs        int
		wantErr        bool
		errContains    string
		checkSeeds     func(t *testing.T, wus []*workunit.WorkUnit)
		checkInputData func(t *testing.T, wus []*workunit.WorkUnit)
	}{
		{
			name:        "sequential 100 trials batch 30",
			paramSpace:  map[string]interface{}{"num_trials": float64(100)},
			batchSize:   30,
			wantBatches: 4,
			wantWUs:     100,
			checkSeeds: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				for i, wu := range wus {
					var p trialParams
					if err := json.Unmarshal(wu.Parameters, &p); err != nil {
						t.Fatalf("trial %d: unmarshal: %v", i, err)
					}
					if p.TrialIndex != i {
						t.Errorf("trial %d: expected trial_index %d, got %d", i, i, p.TrialIndex)
					}
					if p.Seed != int64(i) {
						t.Errorf("trial %d: expected seed %d, got %d", i, i, p.Seed)
					}
				}
			},
		},
		{
			name: "hash strategy 10 trials deterministic",
			paramSpace: map[string]interface{}{
				"num_trials":    float64(10),
				"seed_strategy": "hash",
			},
			batchSize:   100,
			wantBatches: 1,
			wantWUs:     10,
			checkSeeds: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				for i, wu := range wus {
					var p trialParams
					if err := json.Unmarshal(wu.Parameters, &p); err != nil {
						t.Fatalf("trial %d: unmarshal: %v", i, err)
					}
					if p.Seed < 0 {
						t.Errorf("trial %d: hash seed should be non-negative, got %d", i, p.Seed)
					}
				}
			},
		},
		{
			name: "seed offset applied",
			paramSpace: map[string]interface{}{
				"num_trials":  float64(50),
				"seed_offset": float64(1000),
			},
			batchSize:   100,
			wantBatches: 1,
			wantWUs:     50,
			checkSeeds: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				for i, wu := range wus {
					var p trialParams
					if err := json.Unmarshal(wu.Parameters, &p); err != nil {
						t.Fatalf("trial %d: unmarshal: %v", i, err)
					}
					expectedSeed := int64(1000 + i)
					if p.Seed != expectedSeed {
						t.Errorf("trial %d: expected seed %d, got %d", i, expectedSeed, p.Seed)
					}
				}
			},
		},
		{
			name: "shared config propagated to all work units",
			paramSpace: map[string]interface{}{
				"num_trials": float64(5),
				"shared_config": map[string]interface{}{
					"simulation_bounds": []interface{}{float64(0), float64(100)},
					"iterations":        float64(10000),
				},
			},
			batchSize:   100,
			wantBatches: 1,
			wantWUs:     5,
			checkInputData: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				for i, wu := range wus {
					if wu.InputData == nil {
						t.Fatalf("trial %d: InputData should not be nil", i)
					}
					var data map[string]interface{}
					if err := json.Unmarshal(wu.InputData, &data); err != nil {
						t.Fatalf("trial %d: unmarshal InputData: %v", i, err)
					}
					if data["iterations"] != float64(10000) {
						t.Errorf("trial %d: expected iterations 10000, got %v", i, data["iterations"])
					}
				}
			},
		},
		{
			name:        "missing num_trials",
			paramSpace:  map[string]interface{}{},
			batchSize:   100,
			wantErr:     true,
			errContains: "num_trials",
		},
		{
			name:        "num_trials zero",
			paramSpace:  map[string]interface{}{"num_trials": float64(0)},
			batchSize:   100,
			wantErr:     true,
			errContains: "num_trials",
		},
		{
			name:        "num_trials exceeds max",
			paramSpace:  map[string]interface{}{"num_trials": float64(10_000_001)},
			batchSize:   100,
			wantErr:     true,
			errContains: "num_trials",
		},
		{
			name: "invalid seed_strategy",
			paramSpace: map[string]interface{}{
				"num_trials":    float64(10),
				"seed_strategy": "random",
			},
			batchSize:   100,
			wantErr:     true,
			errContains: "seed_strategy",
		},
		{
			name: "request override takes precedence over project config",
			paramSpace: map[string]interface{}{
				"num_trials":  float64(20),
				"seed_offset": float64(500),
			},
			projConfig: map[string]any{
				"num_trials":  float64(100),
				"seed_offset": float64(0),
			},
			batchSize:   100,
			wantBatches: 1,
			wantWUs:     20,
			checkSeeds: func(t *testing.T, wus []*workunit.WorkUnit) {
				t.Helper()
				var p trialParams
				if err := json.Unmarshal(wus[0].Parameters, &p); err != nil {
					t.Fatal(err)
				}
				if p.Seed != 500 {
					t.Errorf("expected first seed 500, got %d", p.Seed)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj := testProject()
			if tt.projConfig != nil {
				proj.DataConfig.SplittingConfig = tt.projConfig
			}

			wuRepo := &mockWURepo{}
			batchRepo := &mockBatchRepo{}

			result, err := Generate(context.Background(), proj, tt.paramSpace, tt.batchSize, wuRepo, batchRepo)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" {
					if msg := err.Error(); !strings.Contains(msg, tt.errContains) {
						t.Errorf("error %q should contain %q", msg, tt.errContains)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(result.BatchIDs) != tt.wantBatches {
				t.Errorf("expected %d batches, got %d", tt.wantBatches, len(result.BatchIDs))
			}
			if result.WorkUnitsCreated != tt.wantWUs {
				t.Errorf("expected %d work units, got %d", tt.wantWUs, result.WorkUnitsCreated)
			}
			if len(wuRepo.created) != tt.wantWUs {
				t.Errorf("expected %d created work units, got %d", tt.wantWUs, len(wuRepo.created))
			}

			if tt.checkSeeds != nil {
				tt.checkSeeds(t, wuRepo.created)
			}
			if tt.checkInputData != nil {
				tt.checkInputData(t, wuRepo.created)
			}
		})
	}
}

func TestGenerate_HashDeterministic(t *testing.T) {
	proj := testProject()
	params := map[string]interface{}{
		"num_trials":    float64(10),
		"seed_strategy": "hash",
	}

	wuRepo1 := &mockWURepo{}
	batchRepo1 := &mockBatchRepo{}
	_, err := Generate(context.Background(), proj, params, 100, wuRepo1, batchRepo1)
	if err != nil {
		t.Fatal(err)
	}

	wuRepo2 := &mockWURepo{}
	batchRepo2 := &mockBatchRepo{}
	_, err = Generate(context.Background(), proj, params, 100, wuRepo2, batchRepo2)
	if err != nil {
		t.Fatal(err)
	}

	for i := range wuRepo1.created {
		var p1, p2 trialParams
		json.Unmarshal(wuRepo1.created[i].Parameters, &p1)
		json.Unmarshal(wuRepo2.created[i].Parameters, &p2)
		if p1.Seed != p2.Seed {
			t.Errorf("trial %d: seeds not deterministic: %d vs %d", i, p1.Seed, p2.Seed)
		}
	}
}

func TestGenerate_NumTrialsOne(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{}

	result, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": float64(1),
	}, 100, wuRepo, batchRepo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsCreated != 1 {
		t.Errorf("expected 1 work unit, got %d", result.WorkUnitsCreated)
	}
	if len(result.BatchIDs) != 1 {
		t.Errorf("expected 1 batch, got %d", len(result.BatchIDs))
	}
}

func TestGenerate_NumTrialsNonNumeric(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": "one hundred",
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for string num_trials")
	}
	if !strings.Contains(err.Error(), "num_trials") {
		t.Errorf("error %q should mention num_trials", err.Error())
	}
}

func TestGenerate_SeedStrategyNonString(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials":    float64(10),
		"seed_strategy": float64(42),
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error for non-string seed_strategy")
	}
	if !strings.Contains(err.Error(), "seed_strategy") {
		t.Errorf("error %q should mention seed_strategy", err.Error())
	}
}

func TestGenerate_BatchCreateError(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{createErr: fmt.Errorf("connection lost")}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": float64(10),
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when batch create fails")
	}
}

func TestGenerate_BulkCreateError(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{bulkCreateErr: fmt.Errorf("constraint violation")}
	batchRepo := &mockBatchRepo{}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": float64(10),
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when bulk create fails")
	}
}

func TestGenerate_BulkTransitionError(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{bulkTransErr: fmt.Errorf("invalid transition")}
	batchRepo := &mockBatchRepo{}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": float64(10),
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when bulk transition fails")
	}
}

func TestGenerate_ListByProjectError(t *testing.T) {
	proj := testProject()
	wuRepo := &mockWURepo{}
	batchRepo := &mockBatchRepo{listByProjErr: errors.New("db timeout")}

	_, err := Generate(context.Background(), proj, map[string]interface{}{
		"num_trials": float64(10),
	}, 100, wuRepo, batchRepo)
	if err == nil {
		t.Fatal("expected error when ListByLeaf fails")
	}
}

