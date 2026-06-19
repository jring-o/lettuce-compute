package result

import (
	"strings"
	"testing"
)

// splitColumns normalizes a SQL column-list const into trimmed column tokens.
func splitColumns(cols string) []string {
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// TestResultColumnsParity guards the invariant that prefixedResultColumns is
// resultColumns with an "r." alias on every column. ListByLeaf JOINs results
// against work_units and must select the same columns scanResult expects;
// drifting these two lists (e.g. forgetting artifact_version_id on one) breaks
// the leaf-scoped results endpoint with "failed to scan result".
func TestResultColumnsParity(t *testing.T) {
	plain := splitColumns(resultColumns)
	prefixed := splitColumns(prefixedResultColumns)

	if len(plain) != len(prefixed) {
		t.Fatalf("column count mismatch: resultColumns=%d prefixedResultColumns=%d",
			len(plain), len(prefixed))
	}
	for i := range plain {
		want := "r." + plain[i]
		if prefixed[i] != want {
			t.Errorf("column %d: prefixedResultColumns=%q, want %q", i, prefixed[i], want)
		}
	}
}
