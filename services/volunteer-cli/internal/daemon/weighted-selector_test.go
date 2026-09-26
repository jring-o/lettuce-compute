package daemon

import (
	"math"
	"testing"
	"time"
)

func TestSelectHead_SingleServer(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 100})

	srv := &ServerConnection{Name: "alpha", Available: true}
	for i := 0; i < 100; i++ {
		got := ws.SelectHead([]*ServerConnection{srv})
		if got != srv {
			t.Fatalf("iteration %d: expected alpha, got %v", i, got)
		}
		ws.RecordAssignment("alpha", "leaf-1", "", 600)
	}
}

func TestSelectHead_EqualWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 100, "beta": 100})

	srvA := &ServerConnection{Name: "alpha", Available: true}
	srvB := &ServerConnection{Name: "beta", Available: true}
	available := []*ServerConnection{srvA, srvB}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		head := ws.SelectHead(available)
		counts[head.Name]++
		ws.RecordAssignment(head.Name, "leaf-1", "", 600)
	}

	// With equal weights, expect ~50/50. Chi-squared test.
	expected := float64(n) / 2.0
	chiSq := math.Pow(float64(counts["alpha"])-expected, 2)/expected +
		math.Pow(float64(counts["beta"])-expected, 2)/expected

	// Chi-squared critical value for 1 df, p < 0.01 is 6.635.
	if chiSq > 6.635 {
		t.Errorf("distribution too skewed: alpha=%d, beta=%d, chi²=%.2f (> 6.635)",
			counts["alpha"], counts["beta"], chiSq)
	}
}

func TestSelectHead_UnequalWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 300, "beta": 100})

	srvA := &ServerConnection{Name: "alpha", Available: true}
	srvB := &ServerConnection{Name: "beta", Available: true}
	available := []*ServerConnection{srvA, srvB}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		head := ws.SelectHead(available)
		counts[head.Name]++
		ws.RecordAssignment(head.Name, "leaf-1", "", 600)
	}

	// Expected: alpha=750, beta=250. Chi-squared test.
	expectedA := float64(n) * 0.75
	expectedB := float64(n) * 0.25
	chiSq := math.Pow(float64(counts["alpha"])-expectedA, 2)/expectedA +
		math.Pow(float64(counts["beta"])-expectedB, 2)/expectedB

	if chiSq > 6.635 {
		t.Errorf("distribution too skewed: alpha=%d (want ~750), beta=%d (want ~250), chi²=%.2f",
			counts["alpha"], counts["beta"], chiSq)
	}
}

func TestSelectLeaf_EqualWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetLeafWeights("srv", map[string]int{"a": 100, "b": 100, "c": 100})

	leafs := []CachedLeafInfo{
		{ID: "id-a", Slug: "a"},
		{ID: "id-b", Slug: "b"},
		{ID: "id-c", Slug: "c"},
	}

	counts := map[string]int{}
	n := 900
	for i := 0; i < n; i++ {
		id := ws.SelectLeaf("srv", leafs)
		counts[id]++
		ws.RecordAssignment("srv", id, "", 600)
	}

	// Each should be ~300. Chi-squared test with 2 df, p<0.01 critical = 9.210.
	expected := float64(n) / 3.0
	chiSq := 0.0
	for _, id := range []string{"id-a", "id-b", "id-c"} {
		chiSq += math.Pow(float64(counts[id])-expected, 2) / expected
	}
	if chiSq > 9.210 {
		t.Errorf("leaf distribution too skewed: %v, chi²=%.2f", counts, chiSq)
	}
}

func TestSelectLeaf_UnequalWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetLeafWeights("srv", map[string]int{"a": 50, "b": 30, "c": 20})

	leafs := []CachedLeafInfo{
		{ID: "id-a", Slug: "a"},
		{ID: "id-b", Slug: "b"},
		{ID: "id-c", Slug: "c"},
	}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		id := ws.SelectLeaf("srv", leafs)
		counts[id]++
		ws.RecordAssignment("srv", id, "", 600)
	}

	// Expected: a=500, b=300, c=200.
	expectedA := float64(n) * 0.5
	expectedB := float64(n) * 0.3
	expectedC := float64(n) * 0.2
	chiSq := math.Pow(float64(counts["id-a"])-expectedA, 2)/expectedA +
		math.Pow(float64(counts["id-b"])-expectedB, 2)/expectedB +
		math.Pow(float64(counts["id-c"])-expectedC, 2)/expectedC

	if chiSq > 9.210 {
		t.Errorf("leaf distribution too skewed: a=%d (want ~500), b=%d (want ~300), c=%d (want ~200), chi²=%.2f",
			counts["id-a"], counts["id-b"], counts["id-c"], chiSq)
	}
}

func TestSelectHead_AllInBackoff(t *testing.T) {
	ws := NewWeightedSelector()
	got := ws.SelectHead(nil)
	if got != nil {
		t.Errorf("expected nil for empty available list, got %v", got)
	}

	got = ws.SelectHead([]*ServerConnection{})
	if got != nil {
		t.Errorf("expected nil for empty available list, got %v", got)
	}
}

// fixedClock returns a clock the test moves by hand.
func fixedClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	t := start
	return func() time.Time { return t }, func(d time.Duration) { t = t.Add(d) }
}

func TestRecordAssignment_BooksSeconds(t *testing.T) {
	ws := NewWeightedSelector()
	ws.now, _ = fixedClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	ws.RecordAssignment("srv-a", "leaf-1", "u1", 600)
	ws.RecordAssignment("srv-a", "leaf-1", "u2", 600)
	ws.RecordAssignment("srv-a", "leaf-2", "u3", 3000)

	if got := ws.HeadBookedSeconds("srv-a"); got != 4200 {
		t.Errorf("head booked = %g, want 4200", got)
	}
	if got := ws.BookedSeconds("srv-a", "leaf-1"); got != 1200 {
		t.Errorf("leaf-1 booked = %g, want 1200", got)
	}
	if got := ws.BookedSeconds("srv-a", "leaf-2"); got != 3000 {
		t.Errorf("leaf-2 booked = %g, want 3000", got)
	}
	// A unit nothing estimates yet is booked at unknownUnitSeconds.
	ws.RecordAssignment("srv-a", "leaf-3", "u4", 0)
	if got := ws.BookedSeconds("srv-a", "leaf-3"); got != unknownUnitSeconds {
		t.Errorf("unknown unit booked = %g, want %g", got, unknownUnitSeconds)
	}
}

// A booking fades with weightBalanceHalfLife: half of it counts one half-life
// later, and a quarter two half-lives later.
func TestBookedSeconds_FadeWithTheHalfLife(t *testing.T) {
	ws := NewWeightedSelector()
	var advance func(time.Duration)
	ws.now, advance = fixedClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	ws.RecordAssignment("srv", "leaf", "u1", 3600)
	advance(weightBalanceHalfLife)
	if got := ws.BookedSeconds("srv", "leaf"); math.Abs(got-1800) > 1e-6 {
		t.Errorf("after one half-life = %g, want 1800", got)
	}
	ws.RecordAssignment("srv", "leaf", "u2", 3600)
	advance(weightBalanceHalfLife)
	if got := ws.BookedSeconds("srv", "leaf"); math.Abs(got-(900+1800)) > 1e-6 {
		t.Errorf("after two half-lives with a second booking = %g, want 2700", got)
	}
}

// RecordCompletion replaces a unit's estimate with the time it took, dated as
// the booking was; a unit this selector never booked (a result resent after a
// restart) is booked at its time now.
func TestRecordCompletion_ReplacesTheEstimate(t *testing.T) {
	ws := NewWeightedSelector()
	var advance func(time.Duration)
	ws.now, advance = fixedClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	ws.RecordAssignment("srv", "leaf-a", "u1", unknownUnitSeconds)
	ws.RecordCompletion("srv", "leaf-a", "u1", 600)
	if got := ws.BookedSeconds("srv", "leaf-a"); math.Abs(got-600) > 1e-6 {
		t.Errorf("after completion = %g, want 600 (the hour's estimate replaced by the ten minutes it took)", got)
	}
	// Corrected a half-life later, the correction fades with the booking.
	ws.RecordAssignment("srv", "leaf-b", "u2", 1000)
	advance(weightBalanceHalfLife)
	ws.RecordCompletion("srv", "leaf-b", "u2", 3000)
	if got := ws.BookedSeconds("srv", "leaf-b"); math.Abs(got-1500) > 1e-6 {
		t.Errorf("late completion = %g, want 1500 (3000 s booked a half-life ago)", got)
	}
	// A completion never booked here is booked now; a second completion of
	// the same unit is then a new booking too, never a double correction.
	ws.RecordCompletion("srv", "leaf-c", "u3", 700)
	if got := ws.BookedSeconds("srv", "leaf-c"); got != 700 {
		t.Errorf("unbooked completion = %g, want 700", got)
	}
	if got := ws.HeadBookedSeconds("srv"); math.Abs(got-(600*0.5+1500+700)) > 1e-6 {
		t.Errorf("head booked = %g, want the leaves' sum %g", got, 600*0.5+1500+700)
	}
}

// Ties go to the head asked least recently, then to the name; before, the
// alphabetically first name won every tie.
func TestSelectHead_TieGoesToTheLeastRecentlyAsked(t *testing.T) {
	ws := NewWeightedSelector()
	var advance func(time.Duration)
	ws.now, advance = fixedClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	a := &ServerConnection{Name: "alpha"}
	b := &ServerConnection{Name: "beta"}
	if got := ws.SelectHead([]*ServerConnection{a, b}); got != a {
		t.Fatalf("never-asked tie = %s, want alpha (by name)", got.Name)
	}
	ws.NoteAsked("alpha", "")
	advance(time.Second)
	if got := ws.SelectHead([]*ServerConnection{a, b}); got != b {
		t.Errorf("tie after alpha was asked = %s, want beta", got.Name)
	}
	ws.NoteAsked("beta", "")
	advance(time.Second)
	if got := ws.SelectHead([]*ServerConnection{a, b}); got != a {
		t.Errorf("tie after both were asked = %s, want alpha (asked longer ago)", got.Name)
	}
}

func TestShares_AmongWhatIsInPlay(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"lbry": 25, "scios": 100})
	lbry, scios := &ServerConnection{Name: "lbry"}, &ServerConnection{Name: "scios"}
	if got := ws.HeadShare("lbry", []*ServerConnection{lbry, scios}); math.Abs(got-0.2) > 1e-9 {
		t.Errorf("lbry share with both in play = %g, want 0.2", got)
	}
	if got := ws.HeadShare("lbry", []*ServerConnection{lbry}); got != 1 {
		t.Errorf("lbry share once scios is spent = %g, want 1", got)
	}
	ws.SetLeafWeights("lbry", map[string]int{"a": 300})
	leafs := []CachedLeafInfo{{ID: "id-a", Slug: "a"}, {ID: "id-b", Slug: "b"}}
	if got := ws.LeafShare("lbry", leafs[1], leafs); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("b share beside a at 300 = %g, want 0.25 (b's weight defaults to 100)", got)
	}
	if got := ws.LeafShare("lbry", leafs[1], leafs[1:]); got != 1 {
		t.Errorf("b share once a is spent = %g, want 1", got)
	}
}

// The seed books the runs within the window at their active seconds (wall
// clock when none was recorded), faded by age; older runs and entries naming
// no head are skipped.
func TestSeedFromHistory(t *testing.T) {
	ws := NewWeightedSelector()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ws.now, _ = fixedClock(now)
	n := ws.SeedFromHistory([]HistoryEntry{
		{ServerName: "srv", LeafID: "leaf-a", CompletedAt: now, CPUSeconds: 600, WallClockSeconds: 700},
		{ServerName: "srv", LeafID: "leaf-a", CompletedAt: now.Add(-weightBalanceHalfLife), CPUSeconds: 1000},
		{ServerName: "srv", LeafID: "leaf-b", CompletedAt: now, WallClockSeconds: 500, Outcome: HistoryOutcomeNotNeeded},
		{ServerName: "srv", LeafID: "leaf-b", CompletedAt: now.Add(-weightBalanceWindow - time.Minute), CPUSeconds: 1e6},
		{LeafID: "leaf-b", CompletedAt: now, CPUSeconds: 1e6},
	})
	if n != 3 {
		t.Errorf("seeded %d runs, want 3", n)
	}
	if got := ws.BookedSeconds("srv", "leaf-a"); math.Abs(got-1100) > 1e-6 {
		t.Errorf("leaf-a seeded = %g, want 1100 (600 now + 1000 a half-life ago)", got)
	}
	if got := ws.BookedSeconds("srv", "leaf-b"); got != 500 {
		t.Errorf("leaf-b seeded = %g, want 500 (a not-needed run's wall clock; the old and the head-less runs skipped)", got)
	}
}
