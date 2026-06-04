package generate

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// TestSetNoDeadlineCeilingSeconds_LiveKnob asserts that overriding the synthetic
// NoDeadline reclaim ceiling actually changes the deadline_seconds stamped on a
// NoDeadline leaf's work units (the operator knob is not a silent no-op).
func TestSetNoDeadlineCeilingSeconds_LiveKnob(t *testing.T) {
	orig := noDeadlineCeilingSeconds
	t.Cleanup(func() { noDeadlineCeilingSeconds = orig })

	noDeadline := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{NoDeadline: true},
	}
	withDeadline := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{DeadlineMultiplier: 2.0},
	}

	// Default: stamps the package constant.
	if got := ResolveDeadlineSeconds(noDeadline); got != NoDeadlineCeilingSeconds {
		t.Fatalf("default ceiling: expected %d, got %d", NoDeadlineCeilingSeconds, got)
	}

	// Operator lowers the ceiling for tighter reclaim.
	const tighter = 1800
	SetNoDeadlineCeilingSeconds(tighter)
	if got := ResolveDeadlineSeconds(noDeadline); got != tighter {
		t.Fatalf("after lowering ceiling: expected %d, got %d", tighter, got)
	}

	// A leaf with a real deadline is unaffected by the ceiling.
	if got := ResolveDeadlineSeconds(withDeadline); got != int(DefaultDurationSeconds*2.0) {
		t.Fatalf("real-deadline leaf: expected %d, got %d", int(DefaultDurationSeconds*2.0), got)
	}

	// Non-positive override is ignored (keeps current effective value).
	SetNoDeadlineCeilingSeconds(0)
	if got := ResolveDeadlineSeconds(noDeadline); got != tighter {
		t.Fatalf("non-positive override should be ignored: expected %d, got %d", tighter, got)
	}
}
