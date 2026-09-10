package runtime

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TB-77 regression tests: thermal protection was silently inert wherever the
// CPU temperature could not be read. The monitor's threshold check treats a
// reading of 0 as "unknown" (never hot), which is right, but nothing said so —
// no log line, no notice, no doctor row — so a tester's Intel MacBook sat at
// 100 °C under "pause above 85 °C". The monitor must now detect and announce
// its CPU source at start, and raise a notice when it has none.

func newCapabilityTestMonitor(t *testing.T, enabled bool) (*ThermalMonitor, *recordingSink) {
	t.Helper()
	withMockExecutor(t, notFoundForAll) // no GPU tools
	sink := &recordingSink{}
	cfg := defaultThermalConfig()
	cfg.Enabled = enabled
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewThermalMonitor(cfg, make(chan bool, 1), logger)
	m.SetNoticeSink(sink)
	m.SetPollIntervalForTest(time.Hour) // never polls; Start's announcement is what is under test
	return m, sink
}

func startAndStop(t *testing.T, m *ThermalMonitor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	m.Stop()
}

// The core regression: an enabled monitor on a machine whose CPU temperature
// cannot be read raises the notice exactly once, naming the threshold that
// has no effect, and records the capability for the daemon to report.
func TestTB77_UnreadableCPUSourceRaisesNoticeOnce(t *testing.T) {
	withMockCPUTemp(t, 0)
	withMockThermalCapability(t, ThermalCapability{
		CPUSource: "none",
		Detail:    "no helper installed",
		Remedy:    "install the helper",
	})
	m, sink := newCapabilityTestMonitor(t, true)
	startAndStop(t, m)

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d notices, want exactly one thermal_cpu_unreadable: %+v", len(got), got)
	}
	n := got[0]
	if n.code != thermalCPUUnreadableCode {
		t.Errorf("notice code = %q, want %q", n.code, thermalCPUUnreadableCode)
	}
	if n.level != NoticeLevelWarn {
		t.Errorf("notice level = %q, want warn: the volunteer can fix this one (a remedy exists)", n.level)
	}
	for _, want := range []string{"cannot read this machine's CPU temperature", "no helper installed", "85°C", "has no effect", "install the helper"} {
		if !strings.Contains(n.message, want) {
			t.Errorf("notice message lacks %q: %q", want, n.message)
		}
	}
	if cap := m.Capability(); cap.CPUReadable || cap.CPUSource != "none" {
		t.Errorf("Capability() = %+v, want the detected unreadable source", cap)
	}
}

// An unfixable gap (Windows: this client chooses not to ask WMI) is
// information, not a warning: TI-19's rule that "needs attention" never
// holds what nobody can act on.
func TestTB77_UnfixableGapIsInformation(t *testing.T) {
	withMockCPUTemp(t, 0)
	withMockThermalCapability(t, ThermalCapability{CPUSource: "none", Detail: "the platform does not expose it"})
	m, sink := newCapabilityTestMonitor(t, true)
	startAndStop(t, m)

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d notices, want one: %+v", len(got), got)
	}
	if got[0].level != NoticeLevelInfo {
		t.Errorf("level = %q, want info for a gap with no remedy", got[0].level)
	}
	if strings.Contains(got[0].message, "To enable it") {
		t.Errorf("message must not promise a remedy that does not exist: %q", got[0].message)
	}
}

// A readable source raises nothing: the healthy half.
func TestTB77_ReadableCPUSourceRaisesNoNotice(t *testing.T) {
	withMockCPUTemp(t, 40) // also stubs the capability readable
	m, sink := newCapabilityTestMonitor(t, true)
	startAndStop(t, m)

	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("got notices %+v with a readable CPU source, want none", got)
	}
	if cap := m.Capability(); !cap.CPUReadable {
		t.Errorf("Capability() = %+v, want readable", cap)
	}
}

// With protection off the capability is still detected — `doctor`, the API
// and the app say what the machine can read either way — but no notice is
// raised about a threshold that is not in force.
func TestTB77_DisabledMonitorDetectsButStaysQuiet(t *testing.T) {
	withMockCPUTemp(t, 0)
	withMockThermalCapability(t, ThermalCapability{CPUSource: "none", Detail: "nothing", Remedy: "something"})
	m, sink := newCapabilityTestMonitor(t, false)
	startAndStop(t, m)

	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("disabled monitor raised %+v, want nothing", got)
	}
	if cap := m.Capability(); cap.CPUSource != "none" {
		t.Errorf("Capability() = %+v, want the detected source even while off", cap)
	}
}

// The fixable/unfixable split is the remedy's presence, nothing subtler.
func TestTB77_FixableIsRemedyPresence(t *testing.T) {
	if (ThermalCapability{Remedy: "do x"}).Fixable() != true {
		t.Error("a capability with a remedy must be fixable")
	}
	if (ThermalCapability{}).Fixable() {
		t.Error("a capability without a remedy must not be fixable")
	}
}
