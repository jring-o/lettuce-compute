package generate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Router dispatches work unit generation to pattern-specific generators.
type Router struct {
	paramSweep workunit.GenerateFunc
	mapReduce  workunit.GenerateFunc
	monteCarlo workunit.GenerateFunc
	custom     workunit.GenerateFunc
	logger     *slog.Logger
}

// NewRouter creates a Router with the given pattern-specific generators.
func NewRouter(paramSweep, mapReduce, monteCarlo, custom workunit.GenerateFunc, logger *slog.Logger) *Router {
	return &Router{
		paramSweep: paramSweep,
		mapReduce:  mapReduce,
		monteCarlo: monteCarlo,
		custom:     custom,
		logger:     logger,
	}
}

// Generate dispatches to the correct generator based on leaf.TaskPattern.
// Returns apierror.ValidationError if the pattern has no registered generator.
func (r *Router) Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	sink workunit.BatchSink,
) (*workunit.GenerateResult, error) {
	r.logger.InfoContext(ctx, "dispatching work unit generation",
		"leaf_id", proj.ID,
		"task_pattern", proj.TaskPattern,
	)

	switch proj.TaskPattern {
	case leaf.PatternParameterSweep:
		if r.paramSweep == nil {
			return nil, apierror.ValidationError("parameter_sweep generator not configured", nil)
		}
		return r.paramSweep(ctx, proj, parameterSpace, batchSize, sink)

	case leaf.PatternMapReduce:
		if r.mapReduce == nil {
			return nil, apierror.ValidationError("map_reduce generator not configured", nil)
		}
		return r.mapReduce(ctx, proj, parameterSpace, batchSize, sink)

	case leaf.PatternMonteCarlo:
		if r.monteCarlo == nil {
			return nil, apierror.ValidationError("monte_carlo generator not configured", nil)
		}
		return r.monteCarlo(ctx, proj, parameterSpace, batchSize, sink)

	case leaf.PatternCustom:
		if r.custom == nil {
			return nil, apierror.ValidationError("custom generator not configured", nil)
		}
		return r.custom(ctx, proj, parameterSpace, batchSize, sink)

	default:
		return nil, apierror.ValidationError(
			fmt.Sprintf("unknown task pattern: %q", proj.TaskPattern),
			nil,
		)
	}
}
