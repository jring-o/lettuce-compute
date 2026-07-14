package validation

import (
	"encoding/json"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// TestValidateSubmittedOutput enforces the §4.3 review-#6 door spec: it demands exactly what the
// leaf's own comparator will later require of an INLINE output — no more, no less.
func TestValidateSubmittedOutput(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		ignore  []string
		compare []string
		output  json.RawMessage
		wantErr bool
	}{
		// Every mode: empty and malformed are refused.
		{name: "numeric_empty", mode: leaf.ComparisonNumericTolerance, output: json.RawMessage(``), wantErr: true},
		{name: "exact_empty", mode: leaf.ComparisonExact, output: json.RawMessage(``), wantErr: true},
		{name: "numeric_malformed", mode: leaf.ComparisonNumericTolerance, output: json.RawMessage(`not json`), wantErr: true},
		{name: "exact_malformed", mode: leaf.ComparisonExact, output: json.RawMessage(`{"x":`), wantErr: true},

		// float64-overflow numeric: refused where the comparator flattens (NUMERIC), accepted where
		// it never flattens (EXACT), with or without ignore_fields.
		{name: "numeric_overflow_refused", mode: leaf.ComparisonNumericTolerance, output: json.RawMessage(`{"x":1e400}`), wantErr: true},
		{name: "exact_overflow_accepted", mode: leaf.ComparisonExact, output: json.RawMessage(`{"x":1e400}`), wantErr: false},
		{name: "exact_overflow_accepted_with_ignore", mode: leaf.ComparisonExact, ignore: []string{"x"}, output: json.RawMessage(`{"x":1e400}`), wantErr: false},

		// NUMERIC field filters run BEFORE the finiteness check, so a non-finite value on a field the
		// validation never reads must not refuse an honest submission.
		{name: "numeric_ignored_nonfinite_accepted", mode: leaf.ComparisonNumericTolerance, ignore: []string{"bad"}, output: json.RawMessage(`{"bad":1e400,"good":1.0}`), wantErr: false},
		{name: "numeric_compare_excludes_bad_accepted", mode: leaf.ComparisonNumericTolerance, compare: []string{"good"}, output: json.RawMessage(`{"bad":1e400,"good":1.0}`), wantErr: false},

		// A clean numeric payload passes.
		{name: "numeric_valid", mode: leaf.ComparisonNumericTolerance, output: json.RawMessage(`{"x":1.5,"y":2.0}`), wantErr: false},
		// A clean exact payload passes.
		{name: "exact_valid", mode: leaf.ComparisonExact, output: json.RawMessage(`{"result":"ok"}`), wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vc := leaf.ValidationConfig{
				ComparisonMode: tc.mode,
				IgnoreFields:   tc.ignore,
				CompareFields:  tc.compare,
			}
			err := ValidateSubmittedOutput(vc, tc.output)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateSubmittedOutput(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateSubmittedOutput(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}
