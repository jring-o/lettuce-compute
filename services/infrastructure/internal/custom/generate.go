package custom

import (
	"context"
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Generate returns an error for custom pattern projects, directing them to the
// /work-units/bulk endpoint. The /generate endpoint is for built-in patterns.
func Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	sink workunit.BatchSink,
) (*workunit.GenerateResult, error) {
	return nil, &apierror.APIError{
		Code:    "INVALID_PATTERN_FOR_GENERATE",
		Message: "Custom pattern leafs use /work-units/bulk to upload work units directly. The /generate endpoint is for built-in patterns (parameter_sweep, map_reduce, monte_carlo).",
		Details: map[string]string{
			"redirect_endpoint": fmt.Sprintf("/api/v1/leafs/%s/work-units/bulk", proj.ID.String()),
		},
		HTTPStatus: 400,
	}
}
