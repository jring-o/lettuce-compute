package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Unit tests (no DB) for the fault monitor's automatic-standing-backpressure WARN
// sweep: the optional standing.Repository wiring (WithStandingPopulation), the
// effective-standing counting (an EXPIRED bench counts as PROBATION, not BENCHED), the
// empty-population non-stamp, and the warn throttle. The AllNonOK query itself is
// covered by the standing package's integration tests.
//
// The benchedEntry/probationEntry helpers and the volunteer.Standing* constants come
// from dispatch_cache_standing_test.go (same test package). warnStandingPopulation keys
// its id sample off the AllNonOK MAP key, not Entry.VolunteerID, so those helpers' unset
// VolunteerID is immaterial here.

// fakeStandingPopulationRepo is a full standing.Repository (WithStandingPopulation takes
// the whole interface) whose only meaningful method is AllNonOK — the sole read
// warnStandingPopulation makes. The other four panic because these tests never call
// them; a call would be a bug worth failing loudly on.
type fakeStandingPopulationRepo struct {
	entries map[types.ID]standing.Entry
	calls   int
}

func (r *fakeStandingPopulationRepo) AllNonOK(_ context.Context) (map[types.ID]standing.Entry, error) {
	r.calls++
	return r.entries, nil
}

func (r *fakeStandingPopulationRepo) SetOperator(context.Context, types.ID, string, *time.Time, string) (*standing.Entry, error) {
	panic("unexpected SetOperator")
}

func (r *fakeStandingPopulationRepo) Clear(context.Context, types.ID) (*standing.Entry, error) {
	panic("unexpected Clear")
}

func (r *fakeStandingPopulationRepo) Get(context.Context, types.ID) (*standing.Entry, error) {
	panic("unexpected Get")
}

func (r *fakeStandingPopulationRepo) ListNonOK(context.Context, int, int) ([]*standing.Entry, error) {
	panic("unexpected ListNonOK")
}

// newStandingTestMonitor builds a monitor around repo with a buffer-backed logger, so a
// test can assert exactly when the WARN line fires. A nil repo leaves the sweep unwired
// (the machine-off case).
func newStandingTestMonitor(repo standing.Repository) (*FaultMonitor, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	m := &FaultMonitor{logger: logger}
	if repo != nil {
		m.WithStandingPopulation(repo)
	}
	return m, buf
}

func TestWarnStandingPopulation_NilRepoSkipped(t *testing.T) {
	// No standing repo wired (the backpressure machine is off) -> the sweep is a no-op:
	// no read, no log, no panic.
	m, buf := newStandingTestMonitor(nil)
	m.warnStandingPopulation(context.Background())
	if buf.Len() != 0 {
		t.Fatalf("nil standing repo must emit no log\nlog: %s", buf.String())
	}
}

func TestWarnStandingPopulation_CountsBenchedVsProbation(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	vBenched, vExpired, vProbation := types.NewID(), types.NewID(), types.NewID()
	repo := &fakeStandingPopulationRepo{entries: map[types.ID]standing.Entry{
		// A live bench -> effective BENCHED.
		vBenched: benchedEntry(&future),
		// An EXPIRED bench -> effective PROBATION (the EffectiveStanding rule: re-entry
		// to OK goes through the backpressure exit, not straight from an expired bench).
		vExpired: benchedEntry(&past),
		// Stored PROBATION -> effective PROBATION.
		vProbation: probationEntry(),
	}}
	m, buf := newStandingTestMonitor(repo)

	m.warnStandingPopulation(context.Background())

	if repo.calls != 1 {
		t.Fatalf("AllNonOK calls = %d, want 1", repo.calls)
	}
	log := buf.String()
	if got := strings.Count(log, "automatic standing backpressure"); got != 1 {
		t.Fatalf("WARN lines = %d, want 1\nlog: %s", got, log)
	}
	// Only the live bench counts as BENCHED; the expired bench folds into PROBATION.
	if !strings.Contains(log, "benched=1") {
		t.Fatalf("want benched=1 (only the live bench)\nlog: %s", log)
	}
	if !strings.Contains(log, "probation=2") {
		t.Fatalf("want probation=2 (stored PROBATION + the EXPIRED bench)\nlog: %s", log)
	}
	if m.lastStandingPopulationWarn.IsZero() {
		t.Fatalf("a non-empty population must stamp the throttle")
	}
}

func TestWarnStandingPopulation_EmptyPopulationNeverWarnsNorThrottles(t *testing.T) {
	repo := &fakeStandingPopulationRepo{entries: map[types.ID]standing.Entry{}}
	m, buf := newStandingTestMonitor(repo)

	// An empty (all-OK) population warns nothing and does NOT arm the throttle: the next
	// sweep reads again, so the WARN fires on the very scan a population first appears.
	m.warnStandingPopulation(context.Background())
	m.warnStandingPopulation(context.Background())
	if repo.calls != 2 {
		t.Fatalf("AllNonOK calls with empty population = %d, want 2 (no throttle armed)", repo.calls)
	}
	if strings.Contains(buf.String(), "automatic standing backpressure") {
		t.Fatalf("empty population must not WARN\nlog: %s", buf.String())
	}
	if !m.lastStandingPopulationWarn.IsZero() {
		t.Fatalf("empty population must not stamp the throttle")
	}
}

func TestWarnStandingPopulation_ThrottleSuppressesSecondWarn(t *testing.T) {
	v := types.NewID()
	repo := &fakeStandingPopulationRepo{entries: map[types.ID]standing.Entry{
		v: probationEntry(),
	}}
	m, buf := newStandingTestMonitor(repo)

	m.warnStandingPopulation(context.Background())
	if got := strings.Count(buf.String(), "automatic standing backpressure"); got != 1 {
		t.Fatalf("WARN lines after first sweep = %d, want 1\nlog: %s", got, buf.String())
	}

	// Within the throttle window the sweep does not even read the repo.
	m.warnStandingPopulation(context.Background())
	if repo.calls != 1 {
		t.Fatalf("AllNonOK calls within throttle window = %d, want 1 (throttled before reading)", repo.calls)
	}
	if got := strings.Count(buf.String(), "automatic standing backpressure"); got != 1 {
		t.Fatalf("WARN lines within throttle window = %d, want still 1", got)
	}

	// Once the throttle window has elapsed the sweep reads and warns again.
	m.lastStandingPopulationWarn = time.Now().Add(-standingPopulationWarnEvery - time.Second)
	m.warnStandingPopulation(context.Background())
	if repo.calls != 2 {
		t.Fatalf("AllNonOK calls after throttle elapsed = %d, want 2", repo.calls)
	}
	if got := strings.Count(buf.String(), "automatic standing backpressure"); got != 2 {
		t.Fatalf("WARN lines after throttle elapsed = %d, want 2", got)
	}
}
