package aggregation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Engine performs result aggregation for projects.
type Engine struct {
	resultRepo  result.Repository
	wuRepo      workunit.WorkUnitRepository
	leafRepo leaf.Repository
	logger      *slog.Logger

	mu    sync.RWMutex
	cache map[types.ID]*AggregateResult
}

// NewEngine creates a new aggregation engine.
func NewEngine(
	resultRepo result.Repository,
	wuRepo workunit.WorkUnitRepository,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *Engine {
	return &Engine{
		resultRepo:  resultRepo,
		wuRepo:      wuRepo,
		leafRepo: leafRepo,
		logger:      logger,
		cache:       make(map[types.ID]*AggregateResult),
	}
}

// GetCached returns the last cached aggregation result, or nil if none.
func (e *Engine) GetCached(leafID types.ID) *AggregateResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cache[leafID]
}

// Aggregate collects validated results and combines them according to the
// project's task pattern and aggregation config.
func (e *Engine) Aggregate(ctx context.Context, leafID types.ID, opts AggregateOptions) (*AggregateResult, error) {
	if !opts.Force {
		if cached := e.GetCached(leafID); cached != nil {
			return cached, nil
		}
	}

	proj, err := e.leafRepo.GetByID(ctx, leafID)
	if err != nil {
		return nil, apierror.NotFound("project", leafID.String())
	}

	// Count total work units for this project.
	totalWUs, err := e.countWorkUnits(ctx, leafID, opts.BatchID)
	if err != nil {
		return nil, apierror.Internal("failed to count work units", err)
	}

	// Collect validated work units and their agreed results.
	pairs, err := e.collectAgreedResults(ctx, leafID, opts.BatchID)
	if err != nil {
		return nil, apierror.Internal("failed to collect results", err)
	}

	if len(pairs) == 0 {
		return nil, apierror.Conflict("no validated results yet", nil)
	}

	format := opts.Format
	if format == "" {
		format = resolveFormat(proj.DataConfig.AggregationFormat)
	}

	var aggResult *AggregateResult

	switch proj.TaskPattern {
	case leaf.PatternParameterSweep:
		aggResult, err = aggregateParamSweep(pairs, format)
	case leaf.PatternMapReduce:
		aggResult, err = aggregateMapReduce(pairs, proj.DataConfig.AggregationConfig)
	case leaf.PatternMonteCarlo:
		aggResult, err = aggregateMonteCarlo(pairs, proj.DataConfig.AggregationConfig)
	case leaf.PatternCustom:
		aggResult, err = aggregateCustom(pairs, proj.DataConfig.AggregationConfig)
	default:
		return nil, apierror.ValidationError("unsupported task pattern for aggregation", nil)
	}

	if err != nil {
		return nil, err
	}

	aggResult.WorkUnitsTotal = totalWUs
	aggResult.AggregatedAt = time.Now().UTC()

	if aggResult.WorkUnitsAggregated < totalWUs {
		aggResult.Status = "partial"
	}

	// Cache the result.
	e.mu.Lock()
	e.cache[leafID] = aggResult
	e.mu.Unlock()

	e.logger.InfoContext(ctx, "aggregation complete",
		"leaf_id", leafID,
		"pattern", proj.TaskPattern,
		"work_units_aggregated", aggResult.WorkUnitsAggregated,
		"work_units_total", totalWUs,
		"status", aggResult.Status,
	)

	return aggResult, nil
}

// countWorkUnits counts total work units for a project, optionally filtered by batch.
func (e *Engine) countWorkUnits(ctx context.Context, leafID types.ID, batchIDStr *string) (int, error) {
	filters := workunit.WorkUnitListFilters{
		LeafID: &leafID,
	}
	if batchIDStr != nil {
		bid, err := types.ParseID(*batchIDStr)
		if err != nil {
			return 0, apierror.ValidationError("invalid batch_id", nil)
		}
		filters.BatchID = &bid
	}

	count := 0
	page := types.PaginationRequest{PageSize: 200}
	for {
		wus, pag, err := e.wuRepo.List(ctx, filters, page)
		if err != nil {
			return 0, err
		}
		count += len(wus)
		if !pag.HasMore {
			break
		}
		page.Cursor = pag.NextCursor
	}
	return count, nil
}

// collectAgreedResults fetches all VALIDATED work units and their AGREED results.
func (e *Engine) collectAgreedResults(ctx context.Context, leafID types.ID, batchIDStr *string) ([]aggregatedWorkUnit, error) {
	validatedState := workunit.WorkUnitStateValidated
	filters := workunit.WorkUnitListFilters{
		LeafID: &leafID,
		State:     &validatedState,
	}
	if batchIDStr != nil {
		bid, err := types.ParseID(*batchIDStr)
		if err != nil {
			return nil, apierror.ValidationError("invalid batch_id", nil)
		}
		filters.BatchID = &bid
	}

	agreedStatus := result.ValidationAgreed
	var pairs []aggregatedWorkUnit
	page := types.PaginationRequest{PageSize: 200}

	for {
		wus, pag, err := e.wuRepo.List(ctx, filters, page)
		if err != nil {
			return nil, err
		}

		for _, wu := range wus {
			results, _, err := e.resultRepo.ListByLeaf(ctx, leafID, result.ResultFilters{
				ValidationStatus: &agreedStatus,
				WorkUnitID:       &wu.ID,
			}, types.PaginationRequest{PageSize: 1})
			if err != nil {
				return nil, err
			}
			if len(results) > 0 {
				pairs = append(pairs, aggregatedWorkUnit{
					Parameters: wu.Parameters,
					OutputData: results[0].OutputData,
				})
			}
		}

		if !pag.HasMore {
			break
		}
		page.Cursor = pag.NextCursor
	}

	return pairs, nil
}

// resolveFormat normalizes the aggregation format to lowercase json/csv.
func resolveFormat(format string) string {
	switch format {
	case "CSV", "csv":
		return "csv"
	default:
		return "json"
	}
}
