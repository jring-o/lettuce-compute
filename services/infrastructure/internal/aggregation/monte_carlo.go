package aggregation

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// Pre-computed z-scores for common confidence levels.
var zScores = map[float64]float64{
	0.80: 1.282,
	0.85: 1.440,
	0.90: 1.645,
	0.95: 1.960,
	0.99: 2.576,
}

// monteCarloStats holds computed statistics (internal, not serialized directly).
type monteCarloStats struct {
	Mean     float64
	Variance float64
	StdDev   float64
	CI       *confidenceInterval
	Count    int
	Min      float64
	Max      float64
}

// confidenceInterval represents a CI.
type confidenceInterval struct {
	Level float64 `json:"level"`
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// aggregateMonteCarlo computes statistics from trial outputs using Welford's algorithm.
func aggregateMonteCarlo(pairs []aggregatedWorkUnit, config map[string]any) (*AggregateResult, error) {
	aggregatorType, _ := config["aggregator_type"].(string)
	outputField, _ := config["output_field"].(string)
	confidenceLevel := 0.95

	if v, ok := config["confidence_level"].(float64); ok && v > 0 {
		confidenceLevel = v
	}

	if outputField == "" {
		return nil, apierror.ValidationError("monte_carlo aggregation_config missing output_field", nil)
	}

	if aggregatorType == "" {
		aggregatorType = "all"
	}

	// Extract numeric values using Welford's online algorithm for stability.
	var (
		n     int
		mean  float64
		m2    float64
		minV  = math.MaxFloat64
		maxV  = -math.MaxFloat64
	)

	for i, p := range pairs {
		v, err := extractNumericField(p.OutputData, outputField)
		if err != nil {
			return nil, apierror.ValidationError(
				fmt.Sprintf("trial %d: %v", i, err), nil,
			)
		}

		n++
		delta := v - mean
		mean += delta / float64(n)
		delta2 := v - mean
		m2 += delta * delta2

		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}

	if n == 0 {
		return nil, apierror.Conflict("no trial results to aggregate", nil)
	}

	// Population variance (not sample variance, since we have all trials).
	variance := 0.0
	if n > 1 {
		variance = m2 / float64(n)
	}
	stdDev := math.Sqrt(variance)

	stats := monteCarloStats{
		Mean:     mean,
		Variance: variance,
		StdDev:   stdDev,
		Count:    n,
		Min:      minV,
		Max:      maxV,
	}

	// Compute confidence interval if requested and n > 1.
	needCI := aggregatorType == "confidence_interval" || aggregatorType == "all"
	if needCI && n > 1 {
		z := lookupZScore(confidenceLevel)
		margin := z * (stdDev / math.Sqrt(float64(n)))
		stats.CI = &confidenceInterval{
			Level: confidenceLevel,
			Lower: mean - margin,
			Upper: mean + margin,
		}
	}

	// Build result: start with common fields, add extras based on aggregator_type.
	statsMap := map[string]interface{}{
		"mean":  stats.Mean,
		"count": stats.Count,
		"min":   stats.Min,
		"max":   stats.Max,
	}

	includeVariance := aggregatorType == "variance" || aggregatorType == "all"
	includeCI := aggregatorType == "confidence_interval" || aggregatorType == "all"

	if includeVariance {
		statsMap["variance"] = stats.Variance
		statsMap["std_dev"] = stats.StdDev
	}
	if includeCI && stats.CI != nil {
		statsMap["confidence_interval"] = stats.CI
	}

	resultObj := map[string]interface{}{
		"statistics": statsMap,
	}

	data, err := json.Marshal(resultObj)
	if err != nil {
		return nil, fmt.Errorf("marshal monte carlo result: %w", err)
	}

	return &AggregateResult{
		Status:              "complete",
		Format:              "json",
		Result:              data,
		WorkUnitsAggregated: n,
	}, nil
}

// lookupZScore returns the z-score for the given confidence level.
// Falls back to linear interpolation between known values.
func lookupZScore(level float64) float64 {
	if z, ok := zScores[level]; ok {
		return z
	}
	// Approximate using the closest known z-score.
	// For common intermediate values, use simple interpolation.
	if level >= 0.99 {
		return 2.576
	}
	if level >= 0.95 {
		return 1.960 + (level-0.95)/(0.99-0.95)*(2.576-1.960)
	}
	if level >= 0.90 {
		return 1.645 + (level-0.90)/(0.95-0.90)*(1.960-1.645)
	}
	return 1.282
}
