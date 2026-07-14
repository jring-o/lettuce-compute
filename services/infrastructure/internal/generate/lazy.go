package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// GenerationCursor tracks progress through the declared parameter space for lazy generation.
// It is persisted in the dedicated leafs.generation_cursor jsonb column (migration 00026), NOT
// inside owner-editable splitting_config, and advances only through the guarded, atomic writes
// below. total_generated is the guard key every advance maintains monotonically.
type GenerationCursor struct {
	LastGeneratedOffset int  `json:"last_generated_offset"`
	LastSeedOffset      int  `json:"last_seed_offset"`
	TotalGenerated      int  `json:"total_generated"`
	GenerationExhausted bool `json:"generation_exhausted"`
}

// LazyManager monitors QUEUED work unit counts for lazy-generation leaves and triggers
// generation when the count drops below the configured threshold.
type LazyManager struct {
	router   *Router
	wuRepo   workunit.WorkUnitRepository
	store    GenerationStore
	leafRepo leaf.Repository
	logger   *slog.Logger
}

// NewLazyManager creates a new LazyManager. store is the durable generation store (the atomic
// per-batch sink plus the guarded standalone cursor write used to stamp exhaustion).
func NewLazyManager(
	router *Router,
	wuRepo workunit.WorkUnitRepository,
	store GenerationStore,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *LazyManager {
	return &LazyManager{
		router:   router,
		wuRepo:   wuRepo,
		store:    store,
		leafRepo: leafRepo,
		logger:   logger,
	}
}

// Run starts the lazy generation monitor. Checks all lazy leaves on a
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

// scanProjects finds all ACTIVE leaves with lazy generation and checks each.
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

// CheckAndGenerate generates one tick's worth of work units for a single lazy leaf and advances
// (or exhausts) its durable cursor atomically. Returns the number of work units generated
// (0 if none needed or the space is already covered).
func (m *LazyManager) CheckAndGenerate(ctx context.Context, projectID types.ID) (int, error) {
	proj, err := m.leafRepo.GetByID(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if proj == nil {
		return 0, nil
	}

	// Only active leaves with lazy generation.
	if proj.State != leaf.StateActive {
		return 0, nil
	}
	if proj.DataConfig.GenerationMode != leaf.GenerationModeLazy {
		return 0, nil
	}

	// Custom pattern uploads work units directly; it does not support lazy generation.
	if proj.TaskPattern == leaf.PatternCustom {
		return 0, nil
	}

	// Lazy map-reduce is forbidden (design §4.10, BG-22b): the full input is present at creation,
	// so there is no not-yet-known tail for laziness to defer. Create/update rejects the config;
	// a pre-migration row that still carries it is skipped-and-WARNed here.
	if proj.TaskPattern == leaf.PatternMapReduce {
		m.logger.WarnContext(ctx, "lazy manager: skipping lazy MAP_REDUCE leaf; lazy generation is not supported for map_reduce",
			"leaf_id", proj.ID)
		return 0, nil
	}

	// Load the cursor from the dedicated column.
	cursor := loadCursor(proj.GenerationCursor)

	// If generation is exhausted (finite leaf fully generated), skip.
	if cursor.GenerationExhausted {
		return 0, nil
	}

	// Monte Carlo finite exhaustion pre-check (design §4.6): if the declared total N is already
	// covered, stamp exhausted WITHOUT invoking the generator. This also covers the N%batch==0
	// edge (the tick after a final full batch generates nothing).
	if proj.TaskPattern == leaf.PatternMonteCarlo && !proj.IsOngoing {
		if n, ok := storedNumTrials(proj); ok && n-cursor.LastSeedOffset <= 0 {
			return 0, m.markExhausted(ctx, proj.ID, cursor)
		}
	}

	// Build the per-tick parameter space (offset keys + the windowed request count).
	parameterSpace := buildParameterSpace(proj, cursor)

	// Wrap the base sink so the tick's single batch carries the cursor advance atomically.
	ws := &cursorAdvancingSink{
		inner:    m.store,
		leafID:   proj.ID,
		pattern:  proj.TaskPattern,
		prior:    cursor,
		advanced: cursor,
	}

	result, err := m.router.Generate(ctx, proj, parameterSpace, proj.DataConfig.LazyBatchSize, ws)
	if err != nil {
		return 0, err
	}

	generated := result.WorkUnitsCreated

	// Exhaustion: a finite leaf whose tick produced fewer than a full batch has spent its space.
	// The exhaustion flag is a separate guarded write AFTER the batch (design §4.8 residual (2)):
	// a crash between them re-runs one empty/short tick and then exhausts — converging, no dup.
	if !proj.IsOngoing && generated < proj.DataConfig.LazyBatchSize {
		if err := m.markExhausted(ctx, proj.ID, ws.advanced); err != nil {
			return generated, err
		}
	}

	return generated, nil
}

// markExhausted stamps GenerationExhausted on the cursor via a standalone guarded write, keyed on
// the cursor's current total_generated. A guard miss (a concurrent writer advanced first) is not
// fatal — the next tick re-evaluates from the winner's cursor.
func (m *LazyManager) markExhausted(ctx context.Context, leafID types.ID, cursor *GenerationCursor) error {
	next := *cursor
	next.GenerationExhausted = true
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	ok, err := m.store.UpdateGenerationCursor(ctx, leafID, raw, int64(cursor.TotalGenerated))
	if err != nil {
		return err
	}
	if !ok {
		m.logger.WarnContext(ctx, "lazy manager: exhaustion stamp skipped; cursor advanced concurrently",
			"leaf_id", leafID)
	}
	return nil
}

// cursorAdvancingSink wraps the base BatchSink for one lazy tick, injecting the tick's cursor
// advance into its single PersistBatch call so the advance commits atomically with the batch it
// accounts for. A lazy tick requests <= LazyBatchSize work units, so the generator emits exactly
// one batch; a second cursor-carrying batch would need a second atomic advance this seam cannot
// express, so it is rejected as a contract violation.
type cursorAdvancingSink struct {
	inner    workunit.BatchSink
	leafID   types.ID
	pattern  leaf.TaskPattern
	prior    *GenerationCursor
	advanced *GenerationCursor // the committed cursor; == prior until PersistBatch runs
	called   bool
}

func (s *cursorAdvancingSink) NextSequenceNumber(ctx context.Context, leafID types.ID) (int, error) {
	return s.inner.NextSequenceNumber(ctx, leafID)
}

func (s *cursorAdvancingSink) PersistBatch(ctx context.Context, batch *workunit.Batch, wus []*workunit.WorkUnit, cursor *workunit.GenerationCursorAdvance) error {
	if cursor != nil {
		return fmt.Errorf("lazy generation: generator set its own cursor advance (must be nil)")
	}
	if s.called {
		return fmt.Errorf("lazy generation: tick emitted more than one batch (expected exactly one)")
	}
	s.called = true

	n := len(wus)
	next := *s.prior
	next.TotalGenerated += n
	switch s.pattern {
	case leaf.PatternMonteCarlo:
		next.LastSeedOffset += n
	case leaf.PatternParameterSweep:
		next.LastGeneratedOffset += n
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	adv := &workunit.GenerationCursorAdvance{
		LeafID:                     s.leafID,
		Cursor:                     raw,
		ExpectedPrevTotalGenerated: int64(s.prior.TotalGenerated),
	}
	if err := s.inner.PersistBatch(ctx, batch, wus, adv); err != nil {
		return err
	}
	advanced := next
	s.advanced = &advanced
	return nil
}

// loadCursor decodes the generation cursor from the leaf's generation_cursor column. An empty or
// {} value yields the zero cursor. Unknown fields (e.g. a migrated pre-fix cursor's obsolete
// keys) are ignored.
func loadCursor(raw json.RawMessage) *GenerationCursor {
	cursor := &GenerationCursor{}
	if len(raw) == 0 {
		return cursor
	}
	_ = json.Unmarshal(raw, cursor)
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

// storedNumTrials reads the researcher's declared total N from splitting_config.num_trials. It is
// left untouched by generation (the cursor is a separate column), so it is the stable target for
// finite Monte Carlo exhaustion.
func storedNumTrials(proj *leaf.Leaf) (int, bool) {
	if proj.DataConfig.SplittingConfig == nil {
		return 0, false
	}
	v, ok := proj.DataConfig.SplittingConfig["num_trials"]
	if !ok {
		return 0, false
	}
	n, err := toFloat64ForCursor(v)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// buildParameterSpace creates the per-tick parameter space for the generator from the cursor.
func buildParameterSpace(proj *leaf.Leaf, cursor *GenerationCursor) map[string]interface{} {
	// Start from the leaf's splitting_config (its declared total num_trials is preserved).
	params := make(map[string]interface{})
	for k, v := range proj.DataConfig.SplittingConfig {
		params[k] = v
	}

	switch proj.TaskPattern {
	case leaf.PatternParameterSweep:
		// The windowed generator paces itself to one batch when _offset is present.
		params["_offset"] = cursor.LastGeneratedOffset
	case leaf.PatternMonteCarlo:
		params["seed_offset"] = cursor.LastSeedOffset
		// Request only this tick's window: min(LazyBatchSize, remaining). The stored total N is
		// NOT overwritten (it lives in splitting_config; the cursor is its own column).
		params["num_trials"] = perTickNumTrials(proj, cursor)
	}

	return params
}

// perTickNumTrials is the Monte Carlo per-tick request: min(LazyBatchSize, remaining) for a
// finite leaf, or LazyBatchSize for an ongoing leaf (num_trials bounds nothing — the DEFINED
// semantics: ongoing MC never exhausts and requests a full batch each tick).
func perTickNumTrials(proj *leaf.Leaf, cursor *GenerationCursor) int {
	batch := proj.DataConfig.LazyBatchSize
	if proj.IsOngoing {
		return batch
	}
	n, ok := storedNumTrials(proj)
	if !ok {
		return batch
	}
	remaining := n - cursor.LastSeedOffset
	if remaining < 0 {
		remaining = 0
	}
	if remaining < batch {
		return remaining
	}
	return batch
}
