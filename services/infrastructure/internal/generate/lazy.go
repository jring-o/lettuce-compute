package generate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// GenerationCursor tracks progress through the input space for lazy generation.
type GenerationCursor struct {
	LastGeneratedOffset int  `json:"last_generated_offset"`
	LastSeedOffset      int  `json:"last_seed_offset"`
	LastChunkOffset     int  `json:"last_chunk_offset"`
	TotalGenerated      int  `json:"total_generated"`
	GenerationExhausted bool `json:"generation_exhausted"`
}

// LazyManager monitors QUEUED work unit counts for lazy-generation projects
// and triggers generation when the count drops below the configured threshold.
type LazyManager struct {
	router      *Router
	wuRepo      workunit.WorkUnitRepository
	batchRepo   workunit.BatchRepository
	leafRepo leaf.Repository
	logger      *slog.Logger
}

// NewLazyManager creates a new LazyManager.
func NewLazyManager(
	router *Router,
	wuRepo workunit.WorkUnitRepository,
	batchRepo workunit.BatchRepository,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *LazyManager {
	return &LazyManager{
		router:      router,
		wuRepo:      wuRepo,
		batchRepo:   batchRepo,
		leafRepo: leafRepo,
		logger:      logger,
	}
}

// Run starts the lazy generation monitor. Checks all lazy projects on a
// configurable interval. Blocks until context is cancelled.
func (m *LazyManager) Run(ctx context.Context, interval time.Duration) {
	m.logger.Info("lazy generation manager starting", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("lazy generation manager stopping")
			return
		case <-ticker.C:
			m.scanProjects(ctx)
		}
	}
}

// scanProjects finds all ACTIVE projects with lazy generation and checks each.
func (m *LazyManager) scanProjects(ctx context.Context) {
	activeState := leaf.StateActive
	projects, _, err := m.leafRepo.List(ctx, leaf.LeafListFilters{
		State: &activeState,
	}, types.PaginationRequest{PageSize: 200})
	if err != nil {
		m.logger.Error("lazy manager: failed to list projects", "error", err)
		return
	}

	for _, proj := range projects {
		if proj.DataConfig.GenerationMode != leaf.GenerationModeLazy {
			continue
		}

		count, err := m.wuRepo.CountByLeafAndState(ctx, proj.ID, workunit.WorkUnitStateQueued)
		if err != nil {
			m.logger.Error("lazy manager: failed to count queued work units",
				"leaf_id", proj.ID, "error", err)
			continue
		}

		if int(count) >= proj.DataConfig.LazyThreshold {
			continue
		}

		generated, err := m.CheckAndGenerate(ctx, proj.ID)
		if err != nil {
			m.logger.Error("lazy manager: generation failed",
				"leaf_id", proj.ID, "error", err)
			continue
		}

		if generated > 0 {
			m.logger.Info("lazy manager: generated work units",
				"leaf_id", proj.ID, "generated", generated)
		}
	}
}

// CheckAndGenerate checks a single project's QUEUED count and generates more
// work units if below threshold. Returns the number of work units generated (0 if none needed).
func (m *LazyManager) CheckAndGenerate(ctx context.Context, projectID types.ID) (int, error) {
	proj, err := m.leafRepo.GetByID(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if proj == nil {
		return 0, nil
	}

	// Only active projects with lazy generation.
	if proj.State != leaf.StateActive {
		return 0, nil
	}
	if proj.DataConfig.GenerationMode != leaf.GenerationModeLazy {
		return 0, nil
	}

	// Custom pattern doesn't support lazy generation.
	if proj.TaskPattern == leaf.PatternCustom {
		return 0, nil
	}

	// Load cursor from splitting_config.
	cursor := loadCursor(proj.DataConfig.SplittingConfig)

	// If generation is exhausted (finite project fully generated), skip.
	if cursor.GenerationExhausted {
		return 0, nil
	}

	// Build parameterSpace from cursor state.
	parameterSpace := buildParameterSpace(proj, cursor)

	// Generate work units.
	result, err := m.router.Generate(ctx, proj, parameterSpace, proj.DataConfig.LazyBatchSize, m.wuRepo, m.batchRepo)
	if err != nil {
		return 0, err
	}

	// Update cursor.
	generated := result.WorkUnitsCreated
	updateCursor(proj, cursor, generated)

	// Check if exhausted: fewer work units than requested means input space is used up.
	if generated < proj.DataConfig.LazyBatchSize && !proj.IsOngoing {
		cursor.GenerationExhausted = true
	}

	// Persist cursor back to leaf.
	saveCursor(proj, cursor)
	if err := m.leafRepo.Update(ctx, proj); err != nil {
		return generated, err
	}

	return generated, nil
}

// loadCursor extracts the generation cursor from splitting_config["_cursor"].
func loadCursor(splittingConfig map[string]any) *GenerationCursor {
	cursor := &GenerationCursor{}
	if splittingConfig == nil {
		return cursor
	}

	raw, ok := splittingConfig["_cursor"]
	if !ok {
		return cursor
	}

	cursorMap, ok := raw.(map[string]any)
	if !ok {
		return cursor
	}

	if v, ok := cursorMap["last_generated_offset"]; ok {
		if n, err := toFloat64ForCursor(v); err == nil {
			cursor.LastGeneratedOffset = int(n)
		}
	}
	if v, ok := cursorMap["last_seed_offset"]; ok {
		if n, err := toFloat64ForCursor(v); err == nil {
			cursor.LastSeedOffset = int(n)
		}
	}
	if v, ok := cursorMap["last_chunk_offset"]; ok {
		if n, err := toFloat64ForCursor(v); err == nil {
			cursor.LastChunkOffset = int(n)
		}
	}
	if v, ok := cursorMap["total_generated"]; ok {
		if n, err := toFloat64ForCursor(v); err == nil {
			cursor.TotalGenerated = int(n)
		}
	}
	if v, ok := cursorMap["generation_exhausted"]; ok {
		if b, isBool := v.(bool); isBool {
			cursor.GenerationExhausted = b
		}
	}

	return cursor
}

// toFloat64ForCursor converts JSON numbers to float64.
func toFloat64ForCursor(v any) (float64, error) {
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

// buildParameterSpace creates the parameterSpace for the generator based on cursor state.
func buildParameterSpace(proj *leaf.Leaf, cursor *GenerationCursor) map[string]interface{} {
	// Start with project's splitting_config as base (excluding _cursor).
	params := make(map[string]interface{})
	for k, v := range proj.DataConfig.SplittingConfig {
		if k == "_cursor" {
			continue
		}
		params[k] = v
	}

	switch proj.TaskPattern {
	case leaf.PatternParameterSweep:
		params["_offset"] = cursor.LastGeneratedOffset
	case leaf.PatternMonteCarlo:
		params["seed_offset"] = cursor.LastSeedOffset
		// Set num_trials to lazy_batch_size for this generation.
		params["num_trials"] = proj.DataConfig.LazyBatchSize
	case leaf.PatternMapReduce:
		params["_chunk_offset"] = cursor.LastChunkOffset
	}

	return params
}

// updateCursor advances the cursor based on how many work units were generated.
func updateCursor(proj *leaf.Leaf, cursor *GenerationCursor, generated int) {
	cursor.TotalGenerated += generated

	switch proj.TaskPattern {
	case leaf.PatternParameterSweep:
		cursor.LastGeneratedOffset += generated
	case leaf.PatternMonteCarlo:
		cursor.LastSeedOffset += generated
	case leaf.PatternMapReduce:
		cursor.LastChunkOffset += generated
	}
}

// saveCursor persists the cursor back into the project's splitting_config.
func saveCursor(proj *leaf.Leaf, cursor *GenerationCursor) {
	if proj.DataConfig.SplittingConfig == nil {
		proj.DataConfig.SplittingConfig = make(map[string]any)
	}
	proj.DataConfig.SplittingConfig["_cursor"] = map[string]any{
		"last_generated_offset": cursor.LastGeneratedOffset,
		"last_seed_offset":      cursor.LastSeedOffset,
		"last_chunk_offset":     cursor.LastChunkOffset,
		"total_generated":       cursor.TotalGenerated,
		"generation_exhausted":  cursor.GenerationExhausted,
	}
}
