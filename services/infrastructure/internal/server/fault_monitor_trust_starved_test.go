package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Unit tests (no DB) for the fault monitor's trust-starved WARN sweep: the optional
// trustStarvationProber capability, the warn throttle, and tolerance of repositories that
// do not implement the probe. The probe's SQL itself is covered by the workunit
// integration test TestCountTrustStarvedUnits.

// starvedProbeRepo implements just enough of workunit.WorkUnitRepository for
// warnTrustStarved — the embedded interface panics on anything else, which these tests
// never call — plus the optional prober with a scripted result and a call counter.
type starvedProbeRepo struct {
	workunit.WorkUnitRepository // nil embed: only the probe is ever called here
	count                       int
	calls                       int
}

func (r *starvedProbeRepo) CountTrustStarvedUnits(_ context.Context, _ int) (int, []types.ID, error) {
	r.calls++
	return r.count, []types.ID{types.NewID()}, nil
}

// newStarvedTestMonitor builds a monitor around repo with a buffer-backed logger, so a
// test can assert exactly when the WARN line fires.
func newStarvedTestMonitor(repo workunit.WorkUnitRepository) (*FaultMonitor, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	return &FaultMonitor{workUnitRepo: repo, logger: logger}, buf
}

func TestWarnTrustStarved_WarnsAndThrottles(t *testing.T) {
	repo := &starvedProbeRepo{count: 3}
	m, buf := newStarvedTestMonitor(repo)

	m.warnTrustStarved(context.Background())
	if repo.calls != 1 {
		t.Fatalf("probe calls after first sweep = %d, want 1", repo.calls)
	}
	if got := strings.Count(buf.String(), "trust-starved"); got != 1 {
		t.Fatalf("WARN lines after first sweep = %d, want 1\nlog: %s", got, buf.String())
	}

	// Within the throttle window the sweep does not even probe (a stable starved
	// population re-logs on operator timescales, not every 30s scan).
	m.warnTrustStarved(context.Background())
	if repo.calls != 1 {
		t.Fatalf("probe calls within throttle window = %d, want 1 (throttled before probing)", repo.calls)
	}
	if got := strings.Count(buf.String(), "trust-starved"); got != 1 {
		t.Fatalf("WARN lines within throttle window = %d, want still 1", got)
	}

	// Once the throttle window has elapsed the sweep probes and warns again.
	m.lastTrustStarvedWarn = time.Now().Add(-trustStarvedWarnEvery - time.Second)
	m.warnTrustStarved(context.Background())
	if repo.calls != 2 {
		t.Fatalf("probe calls after throttle elapsed = %d, want 2", repo.calls)
	}
	if got := strings.Count(buf.String(), "trust-starved"); got != 2 {
		t.Fatalf("WARN lines after throttle elapsed = %d, want 2", got)
	}
}

func TestWarnTrustStarved_ZeroCountNeverWarnsNorThrottles(t *testing.T) {
	repo := &starvedProbeRepo{count: 0}
	m, buf := newStarvedTestMonitor(repo)

	// A zero-count sweep warns nothing and does NOT arm the throttle: the next sweep
	// probes again, so the WARN fires on the very scan a population first appears.
	m.warnTrustStarved(context.Background())
	m.warnTrustStarved(context.Background())
	if repo.calls != 2 {
		t.Fatalf("probe calls with zero count = %d, want 2 (no throttle armed)", repo.calls)
	}
	if strings.Contains(buf.String(), "trust-starved") {
		t.Fatalf("zero-count sweep must not WARN\nlog: %s", buf.String())
	}
}

// nonProberRepo is a repository WITHOUT the optional probe: the embedded interface does
// not carry CountTrustStarvedUnits, so the trustStarvationProber assertion fails.
type nonProberRepo struct {
	workunit.WorkUnitRepository
}

func TestWarnTrustStarved_NonProberRepoSkipped(t *testing.T) {
	// A repository without the optional probe (every fake in the tree) skips the sweep
	// entirely — no panic, no log.
	m, buf := newStarvedTestMonitor(&nonProberRepo{})
	m.warnTrustStarved(context.Background())
	if strings.Contains(buf.String(), "trust-starved") {
		t.Fatalf("non-prober repo must skip the sweep\nlog: %s", buf.String())
	}
}
