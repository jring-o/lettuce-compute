package leaf

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A leaf declares a core range per unit: min_cpu_cores, the dispatch gate, and
// max_cpu_cores, the most one unit can use. Volunteers grant each task a figure
// between the two. A leaf that declares no max gets exactly its min.

func TestValidateResourceRequirements_CoreRange(t *testing.T) {
	tests := []struct {
		name     string
		min, max int
		wantErr  bool
		errMsg   string
	}{
		{"no max declared", 2, 0, false, ""},
		{"max equals min", 2, 2, false, ""},
		{"range", 2, 4, false, ""},
		{"max below min", 3, 2, true, "max_cpu_cores (2) must be at least min_cpu_cores (3)"},
		{"negative max", 1, -1, true, "max_cpu_cores must be non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Decoded from the JSON a leaf author sends, as the update handler does.
			r := &ResourceRequirements{}
			body := fmt.Sprintf(`{"min_cpu_cores": %d, "max_cpu_cores": %d, "min_disk_mb": 1024}`, tt.min, tt.max)
			if err := json.Unmarshal([]byte(body), r); err != nil {
				t.Fatal(err)
			}
			assertValidationResult(t, ValidateResourceRequirements(r), tt.wantErr, tt.errMsg)
		})
	}
}

func TestResolveMaxCPUCores(t *testing.T) {
	for _, tt := range []struct{ min, max, want int }{
		{1, 0, 1},
		{2, 0, 2},
		{2, 4, 4},
		{3, 3, 3},
		{3, 2, 3}, // never below the min, even if stored so before validation existed
	} {
		r := ResourceRequirements{MinCPUCores: tt.min, MaxCPUCores: tt.max}
		if got := r.ResolveMaxCPUCores(); got != tt.want {
			t.Errorf("min %d max %d: ResolveMaxCPUCores = %d, want %d", tt.min, tt.max, got, tt.want)
		}
	}
}

func TestCoreRangeWarnings(t *testing.T) {
	undeclared := &Leaf{ResourceRequirements: ResourceRequirements{MinCPUCores: 1}}
	w := CoreRangeWarnings(undeclared)
	if len(w) != 1 || !strings.Contains(w[0], "max_cpu_cores is not set") || !strings.Contains(w[0], "exactly min_cpu_cores (1)") {
		t.Fatalf("a leaf with no max_cpu_cores: warnings %q, want one naming the missing max and the min it falls back to", w)
	}
	declared := &Leaf{ResourceRequirements: ResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}}
	if w := CoreRangeWarnings(declared); len(w) != 0 {
		t.Fatalf("a leaf that declares its max (even equal to the min): warnings %q, want none", w)
	}
}

func TestToLeafSummary_PublishesCoreRange(t *testing.T) {
	lf := &Leaf{ResourceRequirements: ResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}}
	rr := ToLeafSummary(lf).ResourceRequirements
	if rr.MinCPUCores != 2 || rr.MaxCPUCores != 4 {
		t.Fatalf("catalog publishes %d–%d cores, want 2–4", rr.MinCPUCores, rr.MaxCPUCores)
	}
	lf.ResourceRequirements.MaxCPUCores = 0
	if got := ToLeafSummary(lf).ResourceRequirements.MaxCPUCores; got != 2 {
		t.Fatalf("a leaf with no max publishes max %d, want its min (2)", got)
	}
}
