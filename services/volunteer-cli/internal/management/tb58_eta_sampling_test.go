package management

import "testing"

// TB-58: the rate sample the remaining-time figure is built from was measured
// against the previous status poll, not the previous accepted sample. Progress
// is an integer percent that a leaf may advance in steps minutes apart, and the
// desktop app's Overview polls every 3 s, so a +1 % step arriving after five
// minutes of no change was divided by the 3 s poll gap — a rate a hundred
// times too fast, and a remaining-time figure that collapsed on every step and
// crept back between them.

// A leaf that advances 1 % every 300 s, polled every 3 s: after the 1 % → 2 %
// step the figure must read the remaining 98 % at 300 s each (29,400 s), not
// 98 % at 3 s each (294 s, the pre-fix figure).
func TestTB58_ETA_RateMeasuredFromTheLastAcceptedSample(t *testing.T) {
	e := newETATracker()
	var got int
	for ts := 300; ts <= 600; ts += 3 {
		p := 1
		if ts >= 600 {
			p = 2
		}
		got, _ = e.estimate("wu", p, ts, 0)
	}
	const want = 98 * 300
	if got < want*9/10 || got > want*11/10 {
		t.Errorf("remaining after a 1%% step 300 s apart = %d s, want ≈ %d (pre-fix: 294, the step divided by the 3 s poll gap)", got, want)
	}
}

// A sample is accepted only over a window of at least etaMinSampleInterval: two
// progress writes a second apart (a leaf flushing in a burst) do not become a
// rate, and the anchor stays where the last accepted sample was, so the next
// accepted sample is measured over the whole window from that anchor.
func TestTB58_ETA_NoSampleUnderTheMinimumInterval(t *testing.T) {
	e := newETATracker()
	e.estimate("wu", 1, 100, 0)
	e.estimate("wu", 2, 101, 0)
	if s := e.samples["wu"]; s.elapsed != 100 || s.emaRate != 0 {
		t.Fatalf("anchor moved on a 1 s step: %+v, want elapsed 100 with no rate", s)
	}
	e.estimate("wu", 3, 131, 0)
	s := e.samples["wu"]
	if s.elapsed != 131 || s.emaRate <= 0 {
		t.Fatalf("a step %d s after the anchor should be accepted: %+v", 131-100, s)
	}
	// 2 % over the 31 s from the anchor — not 1 % over the 30 s since the burst.
	if want := 2.0 / 31.0; s.emaRate < want*0.999 || s.emaRate > want*1.001 {
		t.Errorf("rate = %g %%/s, want %g (2 %% over the 31 s window from the anchor)", s.emaRate, want)
	}
}

// A unit that goes backwards (resumed from an older checkpoint) re-anchors at
// its new position instead of waiting to pass the old anchor before it can
// sample again.
func TestTB58_ETA_ReanchorsWhenProgressGoesBackwards(t *testing.T) {
	e := newETATracker()
	e.estimate("wu", 40, 400, 0)
	e.estimate("wu", 50, 500, 0) // accepted: 10 %/100 s
	e.estimate("wu", 30, 520, 0) // resumed from an older checkpoint
	if s := e.samples["wu"]; s.progress != 30 || s.elapsed != 520 {
		t.Fatalf("anchor after a backwards step = %+v, want progress 30 at 520 s", s)
	}
	if s := e.samples["wu"]; s.emaRate <= 0 {
		t.Errorf("the known rate was discarded on re-anchor: %+v", s)
	}
}
