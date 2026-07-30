package workunit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// boundedOutcomeReason guards the outcome_reason column at its single write point:
// empty maps to NULL, honest client reasons (short prefix + 500-byte log tail) pass
// through untouched, and an arbitrary self-compiled client's oversized text is
// truncated on a rune boundary — never stored invalid, never unbounded.
func TestBoundedOutcomeReason(t *testing.T) {
	if got := boundedOutcomeReason(""); got != nil {
		t.Fatalf("empty reason = %q, want nil (SQL NULL)", *got)
	}

	honest := "non-zero exit code 137; output: " + strings.Repeat("x", 500)
	if got := boundedOutcomeReason(honest); got == nil || *got != honest {
		t.Fatalf("an honest client reason must pass through unmodified")
	}

	// Oversized, with a multi-byte rune straddling the byte cap: the cut must land on
	// the rune boundary below the cap, not mid-rune.
	oversized := strings.Repeat("x", maxOutcomeReasonBytes-1) + "☃" + "tail past the cap"
	got := boundedOutcomeReason(oversized)
	if got == nil {
		t.Fatal("oversized reason must still be stored (truncated), not dropped")
	}
	if len(*got) > maxOutcomeReasonBytes {
		t.Fatalf("stored reason is %d bytes, want <= %d", len(*got), maxOutcomeReasonBytes)
	}
	if !utf8.ValidString(*got) {
		t.Fatalf("truncation split a rune: %q", (*got)[len(*got)-6:])
	}
	if len(*got) != maxOutcomeReasonBytes-1 {
		t.Fatalf("cut landed at %d bytes, want %d (the rune boundary below the cap)", len(*got), maxOutcomeReasonBytes-1)
	}
}
