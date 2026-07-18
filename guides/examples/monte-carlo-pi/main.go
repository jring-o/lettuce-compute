// Monte Carlo Pi Estimator — estimates π by random dart-throwing.
//
// Each work unit receives a unique seed from the Monte Carlo generator.
// The program throws 1,000,000 random darts at a unit square and counts
// how many land inside the inscribed quarter circle. The ratio gives
// an estimate of π/4.
//
// Contract:
//   - Reads parameters from $LETTUCE_PARAMS_FILE (JSON with "seed")
//   - Writes results to $LETTUCE_OUTPUT_FILE (JSON with "result" = pi estimate)
//   - Output field "result" is what the Monte Carlo aggregator extracts
//     for statistical analysis (mean, variance, confidence interval)
//
// Expected aggregation result: mean ≈ 3.14159, with CI narrowing as
// more trials complete. 100 trials should give ~2 decimal places.
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"
)

const dartsPerTrial = 1_000_000

type Params struct {
	Seed int64 `json:"seed"`
}

type Result struct {
	Result       float64 `json:"result"` // pi estimate — the aggregator reads this field
	Seed         int64   `json:"seed"`
	DartsThrown  int     `json:"darts_thrown"`
	DartsInside  int     `json:"darts_inside"`
	ComputeTimeMs int64  `json:"compute_time_ms"`
}

func main() {
	paramsFile := os.Getenv("LETTUCE_PARAMS_FILE")
	if paramsFile == "" {
		paramsFile = os.Getenv("LETTUCE_PARAMETERS_FILE")
	}
	outputFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	if outputFile == "" {
		outputDir := os.Getenv("LETTUCE_OUTPUT_DIR")
		if outputDir != "" {
			outputFile = outputDir + "/output.json"
		} else {
			outputFile = "output.json"
		}
	}

	// Read parameters.
	raw, err := os.ReadFile(paramsFile)
	if err != nil {
		panic("read params: " + err.Error())
	}
	var params Params
	if err := json.Unmarshal(raw, &params); err != nil {
		panic("parse params: " + err.Error())
	}

	progressFile := os.Getenv("LETTUCE_PROGRESS_FILE")

	start := time.Now()

	// Throw darts.
	rng := rand.New(rand.NewSource(params.Seed))
	inside := 0
	lastProgress := time.Now()
	for i := 0; i < dartsPerTrial; i++ {
		x := rng.Float64()
		y := rng.Float64()
		if x*x+y*y <= 1.0 {
			inside++
		}
		if progressFile != "" && time.Since(lastProgress) >= 5*time.Second {
			pct := float64(i+1) / float64(dartsPerTrial) * 100
			os.WriteFile(progressFile, []byte(fmt.Sprintf("%.1f", pct)), 0644)
			lastProgress = time.Now()
		}
	}

	// Final progress write, unthrottled: a fast trial can finish inside the 5s
	// throttle window above, and the volunteer's status display expects 100 at
	// completion. Best-effort like every other progress write.
	if progressFile != "" {
		os.WriteFile(progressFile, []byte("100"), 0644)
	}

	piEstimate := 4.0 * float64(inside) / float64(dartsPerTrial)
	elapsed := time.Since(start)

	result := Result{
		Result:        piEstimate,
		Seed:          params.Seed,
		DartsThrown:   dartsPerTrial,
		DartsInside:   inside,
		ComputeTimeMs: elapsed.Milliseconds(),
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(outputFile, out, 0644); err != nil {
		panic("write output: " + err.Error())
	}
}
