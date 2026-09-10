package runtime

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TB-83: the arithmetic behind "other programs are using N % of the CPU" —
// cumulative counters to per-interval percentages of all cores, and the
// parsers for what each platform's reader produces.

func TestParseProcStatCPU(t *testing.T) {
	text := "cpu  1000 50 300 8000 200 10 40 0 0 0\ncpu0 500 25 150 4000 100 5 20 0 0 0\n"
	got, err := parseProcStatCPU(text)
	if err != nil {
		t.Fatal(err)
	}
	// total = 9600; idle + iowait = 8200; busy = 1400 (nice counts as busy).
	if got.total != 9600 || got.busy != 1400 {
		t.Errorf("parseProcStatCPU = %+v, want busy 1400 total 9600", got)
	}
	if _, err := parseProcStatCPU("intr 1 2 3\n"); err == nil {
		t.Error("expected an error without an aggregate cpu line")
	}
}

func TestParseTopCPUUsage(t *testing.T) {
	text := `Processes: 512 total, 2 running, 510 sleeping, 2300 threads
CPU usage: 30.12% user, 20.5% sys, 49.38% idle
SharedLibs: 300M resident, 60M data, 20M linkedit.
Processes: 512 total, 3 running, 509 sleeping, 2300 threads
CPU usage: 12.5% user, 6.25% sys, 81.25% idle
PhysMem: 16G used
`
	got, err := parseTopCPUUsage(text)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-18.75) > 0.001 {
		t.Errorf("parseTopCPUUsage = %v, want 18.75 (the LAST sample's user + sys)", got)
	}
	if _, err := parseTopCPUUsage("no cpu line here\n"); err == nil {
		t.Error("expected an error without a CPU usage line")
	}
}

func TestParsePSCPUTime(t *testing.T) {
	cases := map[string]float64{
		"0:00.04":     0.04,
		"12:34.56":    754.56,
		"1:02:03":     3723,
		"2-01:00:00":  176400,
		"  0:01.50  ": 1.5,
	}
	for in, want := range cases {
		got, err := parsePSCPUTime(in)
		if err != nil {
			t.Errorf("parsePSCPUTime(%q): %v", in, err)
			continue
		}
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("parsePSCPUTime(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parsePSCPUTime("abc"); err == nil {
		t.Error("expected an error for a non-time")
	}
}

// deltaSampler: the first call is a baseline, later calls are the interval's
// busy share; a counter that goes backwards says nothing rather than nonsense.
func TestDeltaSampler(t *testing.T) {
	readings := []cpuTimes{{busy: 100, total: 1000}, {busy: 150, total: 1100}, {busy: 250, total: 1200}, {busy: 10, total: 20}, {busy: 15, total: 120}}
	i := 0
	s := &deltaSampler{read: func() (cpuTimes, error) { r := readings[i]; i++; return r, nil }}

	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("first sample err = %v, want ErrNoBaseline", err)
	}
	if got, _ := s.Sample(); got != 50 {
		t.Errorf("second sample = %v, want 50 (50 busy of 100 total)", got)
	}
	if got, _ := s.Sample(); got != 100 {
		t.Errorf("third sample = %v, want 100", got)
	}
	if got, err := s.Sample(); got != 0 || err != nil {
		t.Errorf("counter reset sample = %v, %v; want 0, nil", got, err)
	}
	if got, _ := s.Sample(); got != 5 {
		t.Errorf("post-reset sample = %v, want 5 (5 busy of 100 total)", got)
	}
}

// fixedMachine reports a constant machine percentage.
type fixedMachine struct{ pct float64 }

func (f fixedMachine) Sample() (float64, error) { return f.pct, nil }

// cpuLoadSampler converts own cumulative seconds to a percentage of ALL cores
// over the interval, so on 4 cores 2 CPU-seconds in 1 s of wall time is 50 %.
func TestCPULoadSampler_OwnShareOfAllCores(t *testing.T) {
	now := time.Unix(1000, 0)
	own := 0.0
	s := NewCPULoadSampler(fixedMachine{80}, func() (float64, error) { return own, nil }, 4, func() time.Time { return now })

	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("first sample err = %v, want ErrNoBaseline", err)
	}
	now = now.Add(time.Second)
	own = 2
	got, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if got.MachinePct != 80 || math.Abs(got.OwnPct-50) > 1e-9 {
		t.Errorf("sample = %+v, want machine 80, own 50", got)
	}
	if f := got.ForeignPct(); math.Abs(f-30) > 1e-9 {
		t.Errorf("ForeignPct = %v, want 30", f)
	}

	// Own above machine (clock skew between the two readers) is 0 foreign,
	// never negative.
	now = now.Add(time.Second)
	own = 10 // 8 CPU-seconds in 1 s on 4 cores = 200 %, clamped to 100
	got, _ = s.Sample()
	if got.OwnPct != 100 || got.ForeignPct() != 0 {
		t.Errorf("sample = %+v, want own clamped to 100 and foreign 0", got)
	}
}

// An own-measurement error is the sampler's error, and re-baselines: the
// interval after a failure cannot be trusted either.
func TestCPULoadSampler_OwnErrorPropagatesAndRebaselines(t *testing.T) {
	now := time.Unix(1000, 0)
	var ownErr error
	s := NewCPULoadSampler(fixedMachine{50}, func() (float64, error) { return 1, ownErr }, 2, func() time.Time { return now })
	_, _ = s.Sample() // baseline
	ownErr = errors.New("stats down")
	now = now.Add(time.Second)
	if _, err := s.Sample(); err == nil || errors.Is(err, ErrNoBaseline) {
		t.Fatalf("err = %v, want the own-measurement failure", err)
	}
	ownErr = nil
	now = now.Add(time.Second)
	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("err after recovery = %v, want ErrNoBaseline (re-baselined)", err)
	}
}

// A machine sampler failure is reported, not hidden behind a zero.
func TestCPULoadSampler_MachineErrorPropagates(t *testing.T) {
	failing := &deltaSampler{read: func() (cpuTimes, error) { return cpuTimes{}, errors.New("no counters") }}
	s := NewCPULoadSampler(failing, func() (float64, error) { return 0, nil }, 2, nil)
	if _, err := s.Sample(); err == nil || errors.Is(err, ErrNoBaseline) {
		t.Errorf("err = %v, want the machine reader's failure", err)
	}
}
