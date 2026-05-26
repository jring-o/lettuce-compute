package daemon

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"
)

// BenchmarkResult stores the volunteer's CPU performance measurement.
type BenchmarkResult struct {
	FPOPS     float64   `json:"fpops"`      // floating-point operations per second
	MeasuredAt time.Time `json:"measured_at"`
	CPUModel  string    `json:"cpu_model"`
}

const benchmarkFile = "benchmark.json"

// benchmarkDuration is how long the benchmark runs. Short enough to not annoy
// volunteers, long enough for a stable measurement.
const benchmarkDuration = 3 * time.Second

// RunCPUBenchmark measures floating-point operations per second using a
// Whetstone-style workload (mixed arithmetic + transcendental functions).
// Returns operations per second.
func RunCPUBenchmark() float64 {
	deadline := time.Now().Add(benchmarkDuration)
	var ops int64

	// Mixed floating-point workload: arithmetic + transcendental functions.
	// Each iteration counts as 8 floating-point operations.
	x := 1.0
	y := 2.0
	for time.Now().Before(deadline) {
		// Inner loop to reduce time.Now() overhead.
		for i := 0; i < 10000; i++ {
			x = x*y + 1.5
			y = math.Sqrt(x) + math.Sin(y)
			x = x/y + 0.7
			y = math.Log(math.Abs(x)+1.0) + math.Cos(y)
		}
		ops += 10000 * 8 // 8 FP ops per inner iteration
	}

	elapsed := benchmarkDuration.Seconds()
	return float64(ops) / elapsed
}

// LoadBenchmark reads a cached benchmark result from the data directory.
// Returns nil if no benchmark has been run yet.
func LoadBenchmark(dataDir string) *BenchmarkResult {
	data, err := os.ReadFile(filepath.Join(dataDir, benchmarkFile))
	if err != nil {
		return nil
	}
	var result BenchmarkResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return &result
}

// SaveBenchmark writes a benchmark result to the data directory.
func SaveBenchmark(dataDir string, result *BenchmarkResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, benchmarkFile), data, 0600)
}

// EnsureBenchmark loads an existing benchmark or runs a new one.
// Returns the FPOPS score.
func EnsureBenchmark(dataDir, cpuModel string) (float64, error) {
	if result := LoadBenchmark(dataDir); result != nil {
		return result.FPOPS, nil
	}

	fpops := RunCPUBenchmark()
	result := &BenchmarkResult{
		FPOPS:      fpops,
		MeasuredAt: time.Now().UTC(),
		CPUModel:   cpuModel,
	}
	if err := SaveBenchmark(dataDir, result); err != nil {
		return fpops, err
	}
	return fpops, nil
}
