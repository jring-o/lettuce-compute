package workunit

import "testing"

// TestFeasibleByDeadline covers the pure feasibility rule that the dispatch SQL
// mirrors: a unit is infeasible for a host only when a definite estimate
// (rsc_fpops_est / benchmark) exceeds the deadline; any unknown input means
// "feasible" so work is never refused on a guess.
func TestFeasibleByDeadline(t *testing.T) {
	tests := []struct {
		name           string
		rscFpopsEst    float64
		benchmarkFPOPS float64
		deadline       int
		want           bool
	}{
		// est = 1e12 / 1e9 = 1000s > 10s -> infeasible.
		{"slow host over deadline", 1e12, 1e9, 10, false},
		// est = 1e12 / 1e12 = 1s <= 10s -> feasible.
		{"fast host within deadline", 1e12, 1e12, 10, true},
		// est exactly at the deadline -> feasible (<=).
		{"exactly at deadline", 1e12, 1e9, 1000, true},
		// No benchmark reported -> cannot estimate -> feasible.
		{"no benchmark", 1e12, 0, 10, true},
		// Leaf has no fpops estimate -> cannot estimate -> feasible.
		{"no leaf estimate", 0, 1e9, 10, true},
		// No deadline -> nothing to miss -> feasible.
		{"no deadline", 1e12, 1e9, 0, true},
		// Negative inputs are treated as unknown -> feasible.
		{"negative benchmark", 1e12, -5, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FeasibleByDeadline(tt.rscFpopsEst, tt.benchmarkFPOPS, tt.deadline); got != tt.want {
				t.Errorf("FeasibleByDeadline(%g, %g, %d) = %v, want %v",
					tt.rscFpopsEst, tt.benchmarkFPOPS, tt.deadline, got, tt.want)
			}
		})
	}
}
