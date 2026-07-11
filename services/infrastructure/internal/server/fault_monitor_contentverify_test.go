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

// Unit tests (no DB) for the fault monitor's content-verification (fetch-and-verify) WARN
// sweep: the three independent lanes (stalled fetch, terminal FAILED delta, knob-off with
// held rows), their per-lane throttles, the FAILED boot-baseline, tolerance of an unwired
// probe, and best-effort error handling. The probe's SQL itself is wired in main.go and
// covered by the content-verification integration suite. Mirrors the emission-anomaly
// sweep tests.

// Per-lane WARN message fingerprints (msg= substrings) used to count fired lanes.
const (
	cvStalledFingerprint = "content-verification fetch lane stalled"
	cvFailedFingerprint  = "reached CONTENT_VERIFICATION_FAILED"
	cvKnobOffFingerprint = "content-fetch knob is OFF"
)

// contentVerifyProbe scripts the content-verification probe closure with a call counter.
type contentVerifyProbe struct {
	stats ContentVerificationStats
	err   error
	calls int
}

func (p *contentVerifyProbe) probe(_ context.Context) (ContentVerificationStats, error) {
	p.calls++
	return p.stats, p.err
}

func newContentVerifyTestMonitor() (*FaultMonitor, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	return &FaultMonitor{logger: logger}, buf
}

func TestWarnContentVerification_StalledLaneWarnsAndThrottles(t *testing.T) {
	// Held rows whose oldest has waited past the stalled threshold.
	probe := &contentVerifyProbe{stats: ContentVerificationStats{
		Held:          3,
		OldestHeldAge: contentVerifyStalledAge + time.Minute,
		FetchEnabled:  true,
	}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvStalledFingerprint); got != 1 {
		t.Fatalf("stalled WARN after first sweep = %d, want 1\nlog: %s", got, buf.String())
	}

	// Within the throttle window the stalled lane does not re-warn (the sweep may still
	// probe for the other lanes, but the stalled clock is armed).
	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvStalledFingerprint); got != 1 {
		t.Fatalf("stalled WARN within throttle window = %d, want still 1", got)
	}

	// Once the throttle window has elapsed the stalled lane warns again.
	m.lastContentVerifyStalledWarn = time.Now().Add(-contentVerifyWarnEvery - time.Second)
	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvStalledFingerprint); got != 2 {
		t.Fatalf("stalled WARN after throttle elapsed = %d, want 2", got)
	}
}

func TestWarnContentVerification_StalledLaneSilentUnderThreshold(t *testing.T) {
	// Held rows, but the oldest is still under the stalled threshold: no stalled WARN.
	probe := &contentVerifyProbe{stats: ContentVerificationStats{
		Held:          2,
		OldestHeldAge: contentVerifyStalledAge - time.Minute,
		FetchEnabled:  true,
	}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	if strings.Contains(buf.String(), cvStalledFingerprint) {
		t.Fatalf("held rows under the stalled threshold must not WARN\nlog: %s", buf.String())
	}
}

func TestWarnContentVerification_FailedDeltaFiresOnceAndRearms(t *testing.T) {
	// Pre-existing terminal rows at boot; the baseline must swallow them.
	probe := &contentVerifyProbe{stats: ContentVerificationStats{
		FailedTotal:  5,
		FetchEnabled: true,
	}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	// First probe boot-baselines FailedTotal=5: no WARN for pre-existing failed rows.
	m.warnContentVerification(context.Background())
	if strings.Contains(buf.String(), cvFailedFingerprint) {
		t.Fatalf("pre-existing failed rows must not page at startup\nlog: %s", buf.String())
	}

	// Two new terminal failures accumulate -> one WARN.
	probe.stats.FailedTotal = 7
	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvFailedFingerprint); got != 1 {
		t.Fatalf("failed WARN after 2 new failures = %d, want 1\nlog: %s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "new_failed=2") {
		t.Fatalf("failed WARN should report new_failed=2\nlog: %s", buf.String())
	}

	// Within the throttle window, further sweeps do not re-warn.
	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvFailedFingerprint); got != 1 {
		t.Fatalf("failed WARN within throttle window = %d, want still 1", got)
	}

	// Throttle elapsed + more failures -> the lane re-arms and warns again.
	m.lastContentVerifyFailedWarn = time.Now().Add(-contentVerifyWarnEvery - time.Second)
	probe.stats.FailedTotal = 10
	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvFailedFingerprint); got != 2 {
		t.Fatalf("failed WARN after re-arm = %d, want 2", got)
	}
	if !strings.Contains(buf.String(), "new_failed=3") {
		t.Fatalf("re-armed failed WARN should report new_failed=3 (delta from the last WARN)\nlog: %s", buf.String())
	}
}

func TestWarnContentVerification_KnobOffWithHeldWarns(t *testing.T) {
	// Knob OFF and rows still held (oldest under the stalled threshold, so only the
	// knob-off lane should fire).
	probe := &contentVerifyProbe{stats: ContentVerificationStats{
		Held:          4,
		OldestHeldAge: time.Minute,
		FetchEnabled:  false,
	}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	if got := strings.Count(buf.String(), cvKnobOffFingerprint); got != 1 {
		t.Fatalf("knob-off WARN = %d, want 1\nlog: %s", got, buf.String())
	}
	if strings.Contains(buf.String(), cvStalledFingerprint) {
		t.Fatalf("held rows under the stalled threshold must not fire the stalled lane\nlog: %s", buf.String())
	}
}

func TestWarnContentVerification_KnobOnWithHeldNoKnobOffWarn(t *testing.T) {
	// Knob ON with held rows: the knob-off lane must stay silent.
	probe := &contentVerifyProbe{stats: ContentVerificationStats{
		Held:          4,
		OldestHeldAge: time.Minute,
		FetchEnabled:  true,
	}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	if strings.Contains(buf.String(), cvKnobOffFingerprint) {
		t.Fatalf("knob-on head must not fire the knob-off lane\nlog: %s", buf.String())
	}
}

func TestWarnContentVerification_AllQuietNoWarns(t *testing.T) {
	// No held rows, no failures, knob on: a healthy head is silent and arms no throttle,
	// so the next sweep probes again.
	probe := &contentVerifyProbe{stats: ContentVerificationStats{FetchEnabled: true}}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	m.warnContentVerification(context.Background())
	if probe.calls != 2 {
		t.Fatalf("probe calls while healthy = %d, want 2 (no throttle armed)", probe.calls)
	}
	for _, fp := range []string{cvStalledFingerprint, cvFailedFingerprint, cvKnobOffFingerprint} {
		if strings.Contains(buf.String(), fp) {
			t.Fatalf("healthy sweep must not WARN (%q)\nlog: %s", fp, buf.String())
		}
	}
}

func TestWarnContentVerification_NoProbeSkipped(t *testing.T) {
	// No probe wired -> the sweep is a no-op: no panic, no log.
	m, buf := newContentVerifyTestMonitor()
	m.warnContentVerification(context.Background())
	if buf.Len() != 0 {
		t.Fatalf("unwired probe must skip the sweep\nlog: %s", buf.String())
	}
}

func TestWarnContentVerification_ProbeErrorIsBestEffort(t *testing.T) {
	probe := &contentVerifyProbe{err: errors.New("db down")}
	m, buf := newContentVerifyTestMonitor()
	m.WithContentVerificationStats(probe.probe)

	m.warnContentVerification(context.Background())
	for _, fp := range []string{cvStalledFingerprint, cvFailedFingerprint, cvKnobOffFingerprint} {
		if strings.Contains(buf.String(), fp) {
			t.Fatalf("a probe error must not produce a lane WARN (%q)\nlog: %s", fp, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "content-verification sweep failed") {
		t.Fatalf("a probe error should be logged\nlog: %s", buf.String())
	}
	// An error does not arm any throttle: the next sweep probes again.
	m.warnContentVerification(context.Background())
	if probe.calls != 2 {
		t.Fatalf("probe calls after error = %d, want 2 (no throttle armed on error)", probe.calls)
	}
}
