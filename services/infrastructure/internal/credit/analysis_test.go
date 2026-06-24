package credit

import (
	"testing"
)

func TestPercentileStats_Structure(t *testing.T) {
	ps := percentileStats{P50: 120, P90: 300, P99: 850}
	if ps.P50 != 120 || ps.P90 != 300 || ps.P99 != 850 {
		t.Errorf("unexpected percentile values: %+v", ps)
	}
}

func TestTaskPatternStats_Structure(t *testing.T) {
	tps := taskPatternStats{Count: 3000, AvgCPUSeconds: 150}
	if tps.Count != 3000 || tps.AvgCPUSeconds != 150 {
		t.Errorf("unexpected task pattern stats: %+v", tps)
	}
}

func TestNormalizationFactors_Ratio(t *testing.T) {
	tests := []struct {
		name  string
		max   float64
		min   float64
		ratio float64
	}{
		{"equal", 100, 100, 1.0},
		{"10x spread", 500, 50, 10.0},
		{"zero min", 100, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ratio float64
			if tt.min > 0 {
				ratio = tt.max / tt.min
			}
			if ratio != tt.ratio {
				t.Errorf("expected ratio %v, got %v", tt.ratio, ratio)
			}
		})
	}
}

func TestResourceTypeBreakdown_Structure(t *testing.T) {
	cpuOnly := ResourceTypeCredit{Credit: 1000, WorkUnits: 1000}
	gpu := ResourceTypeCredit{Credit: 234, WorkUnits: 234}

	total := cpuOnly.Credit + gpu.Credit
	if total != 1234 {
		t.Errorf("expected total credit 1234, got %v", total)
	}
}

func TestNewAnalysisHandler(t *testing.T) {
	h := NewAnalysisHandler(nil, nil, nil)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestLeafAnalysisResponse_Structure(t *testing.T) {
	resp := leafAnalysisResponse{
		WorkUnitsAnalyzed: 5000,
		CPUSecondsPerWU:   percentileStats{P50: 100, P90: 200, P99: 500},
		GPUSecondsPerWU:   percentileStats{P50: 0, P90: 0, P99: 0},
		WallClockPerWU:    percentileStats{P50: 120, P90: 240, P99: 600},
		MemoryMBPerWU:     percentileStats{P50: 512, P90: 1024, P99: 2048},
		ByTaskPattern:     map[string]taskPatternStats{"PARAMETER_SWEEP": {Count: 5000, AvgCPUSeconds: 100}},
	}

	if resp.WorkUnitsAnalyzed != 5000 {
		t.Errorf("expected 5000, got %d", resp.WorkUnitsAnalyzed)
	}
	if resp.CPUSecondsPerWU.P50 != 100 {
		t.Errorf("expected CPU P50=100, got %v", resp.CPUSecondsPerWU.P50)
	}
	if resp.ByTaskPattern["PARAMETER_SWEEP"].Count != 5000 {
		t.Errorf("expected pattern count 5000, got %d", resp.ByTaskPattern["PARAMETER_SWEEP"].Count)
	}
}

func TestCrossLeafResponse_Structure(t *testing.T) {
	resp := crossLeafResponse{
		Leafs: []crossLeafEntry{
			{
				LeafName:            "Alpha",
				AvgCPUSecondsPerCredit: 120,
				TotalCreditGranted:     50000,
				ActiveVolunteers:       25,
			},
			{
				LeafName:            "Beta",
				AvgCPUSecondsPerCredit: 60,
				TotalCreditGranted:     30000,
				ActiveVolunteers:       15,
			},
		},
		NormalizationFactors: normalizationFactors{
			MaxCPUSecondsPerCredit: 120,
			MinCPUSecondsPerCredit: 60,
			Ratio:                  2.0,
		},
	}

	if len(resp.Leafs) != 2 {
		t.Fatalf("expected 2 leafs, got %d", len(resp.Leafs))
	}
	if resp.NormalizationFactors.Ratio != 2.0 {
		t.Errorf("expected ratio 2.0, got %v", resp.NormalizationFactors.Ratio)
	}
}

func TestVolunteerBreakdownResponse_Structure(t *testing.T) {
	resp := VolunteerBreakdown{
		TotalCredit: 1500,
		ByLeaf: []LeafCredit{
			{LeafName: "proj-a", Credit: 1000, WorkUnits: 100, CPUSeconds: 5000},
			{LeafName: "proj-b", Credit: 500, WorkUnits: 50, GPUSeconds: 2000},
		},
		ByResourceType: map[string]ResourceTypeCredit{
			"cpu_only": {Credit: 1000, WorkUnits: 100},
			"gpu":      {Credit: 500, WorkUnits: 50},
		},
	}

	if resp.TotalCredit != 1500 {
		t.Errorf("expected total 1500, got %v", resp.TotalCredit)
	}
	if len(resp.ByLeaf) != 2 {
		t.Fatalf("expected 2 leafs, got %d", len(resp.ByLeaf))
	}
	if resp.ByResourceType["cpu_only"].Credit != 1000 {
		t.Errorf("expected cpu_only credit 1000, got %v", resp.ByResourceType["cpu_only"].Credit)
	}
	if resp.ByResourceType["gpu"].WorkUnits != 50 {
		t.Errorf("expected gpu work_units 50, got %d", resp.ByResourceType["gpu"].WorkUnits)
	}
}

func TestDailyWeeklyTimeline_Structure(t *testing.T) {
	daily := DailyCredit{Date: "2026-03-22", Credit: 42}
	weekly := WeeklyCredit{WeekStart: "2026-03-16", Credit: 200}

	if daily.Date != "2026-03-22" {
		t.Errorf("expected date '2026-03-22', got %q", daily.Date)
	}
	if daily.Credit != 42 {
		t.Errorf("expected credit 42, got %v", daily.Credit)
	}
	if weekly.WeekStart != "2026-03-16" {
		t.Errorf("expected week_start '2026-03-16', got %q", weekly.WeekStart)
	}
	if weekly.Credit != 200 {
		t.Errorf("expected credit 200, got %v", weekly.Credit)
	}
}
