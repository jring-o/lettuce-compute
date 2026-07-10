package credit

import (
	"encoding/json"
	"strings"
	"testing"
)

// wantMarker is the exact JSON fragment every execution_metadata-derived response
// aggregate must carry so a consumer cannot mistake unverified volunteer-reported
// resource numbers for head-certified fact (design §8.6, BETA_GATE BG-06a item 3).
const wantMarker = `"metrics_provenance":"unverified_volunteer_reported"`

// TestMetricsProvenanceConstant pins the marker value the published contract and the
// per-surface tests both depend on.
func TestMetricsProvenanceConstant(t *testing.T) {
	if MetricsProvenanceUnverified != "unverified_volunteer_reported" {
		t.Errorf("MetricsProvenanceUnverified = %q, want %q", MetricsProvenanceUnverified, "unverified_volunteer_reported")
	}
}

// TestLeafAnalysis_LabelsUnverifiedMetrics asserts the per-leaf analysis response
// carries the unverified-metrics marker. Fails on pre-fix code (no such field).
func TestLeafAnalysis_LabelsUnverifiedMetrics(t *testing.T) {
	resp := leafAnalysisResponse{
		WorkUnitsAnalyzed: 10,
		CPUSecondsPerWU:   percentileStats{P50: 100, P90: 200, P99: 500},
		MetricsProvenance: MetricsProvenanceUnverified,
	}
	assertHasMarker(t, resp)
}

// TestCrossLeaf_LabelsUnverifiedMetrics asserts the cross-leaf normalization response
// carries the unverified-metrics marker at the envelope top level.
func TestCrossLeaf_LabelsUnverifiedMetrics(t *testing.T) {
	resp := crossLeafResponse{
		Leafs: []crossLeafEntry{
			{LeafName: "alpha", AvgCPUSecondsPerCredit: 120, TotalCreditGranted: 5000},
		},
		NormalizationFactors: normalizationFactors{MaxCPUSecondsPerCredit: 120, MinCPUSecondsPerCredit: 60, Ratio: 2},
		MetricsProvenance:    MetricsProvenanceUnverified,
	}
	assertHasMarker(t, resp)
}

// TestVolunteerBreakdown_LabelsUnverifiedMetrics asserts the volunteer credit
// breakdown response carries the unverified-metrics marker (covers the per-leaf
// cpu/gpu-seconds aggregates).
func TestVolunteerBreakdown_LabelsUnverifiedMetrics(t *testing.T) {
	resp := VolunteerBreakdown{
		TotalCredit: 1500,
		ByLeaf: []LeafCredit{
			{LeafName: "proj-a", Credit: 1000, WorkUnits: 100, CPUSeconds: 5000},
		},
		MetricsProvenance: MetricsProvenanceUnverified,
	}
	assertHasMarker(t, resp)
}

// TestByHostBreakdown_LabelsUnverifiedMetrics asserts the by-host breakdown, which is
// served only nested inside the VolunteerBreakdown envelope, is covered by the same
// top-level marker (its per-host cpu/gpu-seconds are equally unverified).
func TestByHostBreakdown_LabelsUnverifiedMetrics(t *testing.T) {
	resp := VolunteerBreakdown{
		TotalCredit: 7,
		ByHost: []HostCredit{
			{Hostname: "laptop", Credit: 3, WorkUnits: 2, CPUSeconds: 900},
			{Hostname: "desktop", Credit: 4, WorkUnits: 1, GPUSeconds: 120},
		},
		MetricsProvenance: MetricsProvenanceUnverified,
	}
	assertHasMarker(t, resp)
}

func assertHasMarker(t *testing.T, resp any) {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), wantMarker) {
		t.Errorf("response JSON missing provenance marker %s\ngot: %s", wantMarker, b)
	}
}
