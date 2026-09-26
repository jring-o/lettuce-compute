package generate

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// TestResolveDeadlineSeconds_HeadDefaultIsLive asserts that the head's
// default_deadline_seconds setting actually changes the deadline_seconds stamped on
// the units of a leaf that sets no deadline of its own (the setting is not a silent
// no-op), and leaves a leaf's own deadline alone.
func TestResolveDeadlineSeconds_HeadDefaultIsLive(t *testing.T) {
	orig := leaf.HeadDefaultDeadlineSeconds()
	t.Cleanup(func() { leaf.SetHeadDefaultDeadlineSeconds(orig) })

	noLeafDeadline := &leaf.Leaf{}
	own := 5400
	withDeadline := &leaf.Leaf{
		FaultToleranceConfig: leaf.FaultToleranceConfig{DeadlineSeconds: &own},
	}

	// Default: the built-in 6h.
	if got := ResolveDeadlineSeconds(noLeafDeadline); got != leaf.BuiltinDefaultDeadlineSeconds {
		t.Fatalf("built-in default: expected %d, got %d", leaf.BuiltinDefaultDeadlineSeconds, got)
	}

	// The operator setting moves the stamped value.
	const tighter = 3600
	leaf.SetHeadDefaultDeadlineSeconds(tighter)
	if got := ResolveDeadlineSeconds(noLeafDeadline); got != tighter {
		t.Fatalf("configured default: expected %d, got %d", tighter, got)
	}

	// A leaf with its own deadline is unaffected by the head default.
	if got := ResolveDeadlineSeconds(withDeadline); got != own {
		t.Fatalf("leaf deadline: expected %d, got %d", own, got)
	}
}
