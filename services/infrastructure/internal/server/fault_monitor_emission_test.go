package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Unit tests (no DB) for the fault monitor's emission-anomaly WARN sweep: the optional
// probe closure, the warn throttle, tolerance of an unwired probe, and best-effort error
// handling. The probe's SQL itself (AnomalyChecker) is covered by the credit integration
// suite. Mirrors the trust-starved sweep tests.

// emissionProbe scripts the emission-anomaly probe closure with a call counter.
type emissionProbe struct {
	halted   bool
	today    float64
	baseline float64
	err      error
	calls    int
}

func (p *emissionProbe) check(_ context.Context) (bool, float64, float64, error) {
	p.calls++
	return p.halted, p.today, p.baseline, p.err
}

func newEmissionTestMonitor() (*FaultMonitor, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	return &FaultMonitor{logger: logger}, buf
}

func TestWarnEmissionAnomaly_WarnsAndThrottles(t *testing.T) {
	probe := &emissionProbe{halted: true, today: 900, baseline: 100}
	m, buf := newEmissionTestMonitor()
	m.WithEmissionAnomalyCheck(probe.check)

	m.warnEmissionAnomaly(context.Background())
	if probe.calls != 1 {
		t.Fatalf("probe calls after first sweep = %d, want 1", probe.calls)
	}
	if got := strings.Count(buf.String(), "emission anomaly:"); got != 1 {
		t.Fatalf("WARN lines after first sweep = %d, want 1\nlog: %s", got, buf.String())
	}

	// Within the throttle window the sweep does not even probe.
	m.warnEmissionAnomaly(context.Background())
	if probe.calls != 1 {
		t.Fatalf("probe calls within throttle window = %d, want 1 (throttled before probing)", probe.calls)
	}
	if got := strings.Count(buf.String(), "emission anomaly:"); got != 1 {
		t.Fatalf("WARN lines within throttle window = %d, want still 1", got)
	}

	// Once the throttle window has elapsed the sweep probes and warns again.
	m.lastEmissionAnomalyWarn = time.Now().Add(-emissionAnomalyWarnEvery - time.Second)
	m.warnEmissionAnomaly(context.Background())
	if probe.calls != 2 {
		t.Fatalf("probe calls after throttle elapsed = %d, want 2", probe.calls)
	}
	if got := strings.Count(buf.String(), "emission anomaly:"); got != 2 {
		t.Fatalf("WARN lines after throttle elapsed = %d, want 2", got)
	}
}

func TestWarnEmissionAnomaly_NotHaltedNeverWarnsNorThrottles(t *testing.T) {
	probe := &emissionProbe{halted: false}
	m, buf := newEmissionTestMonitor()
	m.WithEmissionAnomalyCheck(probe.check)

	// A not-halted sweep warns nothing and does NOT arm the throttle: the next sweep probes
	// again, so the WARN fires on the very scan the anomaly first trips.
	m.warnEmissionAnomaly(context.Background())
	m.warnEmissionAnomaly(context.Background())
	if probe.calls != 2 {
		t.Fatalf("probe calls while healthy = %d, want 2 (no throttle armed)", probe.calls)
	}
	if strings.Contains(buf.String(), "emission anomaly:") {
		t.Fatalf("healthy sweep must not WARN\nlog: %s", buf.String())
	}
}

func TestWarnEmissionAnomaly_NoProbeSkipped(t *testing.T) {
	// No probe wired (the anomaly halt is off) -> the sweep is a no-op: no panic, no log.
	m, buf := newEmissionTestMonitor()
	m.warnEmissionAnomaly(context.Background())
	if strings.Contains(buf.String(), "emission anomaly") {
		t.Fatalf("unwired probe must skip the sweep\nlog: %s", buf.String())
	}
}

func TestWarnEmissionAnomaly_ProbeErrorIsBestEffort(t *testing.T) {
	probe := &emissionProbe{err: errors.New("db down")}
	m, buf := newEmissionTestMonitor()
	m.WithEmissionAnomalyCheck(probe.check)

	m.warnEmissionAnomaly(context.Background())
	if strings.Contains(buf.String(), "emission anomaly:") {
		t.Fatalf("a probe error must not produce the halt WARN\nlog: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "emission-anomaly sweep failed") {
		t.Fatalf("a probe error should be logged\nlog: %s", buf.String())
	}
	// An error does not arm the throttle: the next sweep probes again.
	m.warnEmissionAnomaly(context.Background())
	if probe.calls != 2 {
		t.Fatalf("probe calls after error = %d, want 2 (no throttle armed on error)", probe.calls)
	}
}
