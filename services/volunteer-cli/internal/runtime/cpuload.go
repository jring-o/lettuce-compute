package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CPU load measurement for yielding to other programs (TB-83).
//
// The yield monitor needs two numbers every poll: how busy the whole machine
// is, and how much of that is Lettuce's own doing — the daemon process, every
// native task's process tree and every running container. What is left is
// "foreign" load: other programs, or another volunteer client, needing the
// CPU. This file is the platform-independent half: the interfaces, the
// arithmetic that turns cumulative CPU time into a percentage of all cores,
// and the parsers for the text the platform readers produce. The readers
// themselves live in cpuload_<platform>.go.
//
// Every percentage here is of ALL cores: a machine with 8 cores and one core
// fully busy is at 12.5 %, the way the incumbent's preference of the same name
// counts, and the way `top` on the machine adds up.

// ErrNoBaseline is returned by a sampler's first call: cumulative counters
// need two readings before they can say anything about the interval between
// them. The yield monitor skips such a sample silently; it is not a failure.
var ErrNoBaseline = errors.New("cpu load: no baseline yet")

// MachineCPUSampler reports the whole machine's CPU busy percentage over the
// interval since its previous call (or ErrNoBaseline on the first).
type MachineCPUSampler interface {
	Sample() (busyPct float64, err error)
}

// OwnCPUSecondsFunc reports Lettuce's own cumulative CPU time in seconds —
// the daemon, its native task trees and its containers — since some fixed
// point. It must never decrease; a task that finishes keeps the seconds it
// used. An error means the figure is incomplete (a container whose stats
// cannot be read, a native task the daemon cannot attribute), and the monitor
// must not pause on the raw machine total in that case.
type OwnCPUSecondsFunc func() (seconds float64, err error)

// CPULoadSample is one reading: the machine's busy percentage and Lettuce's
// own share of it, both of all cores.
type CPULoadSample struct {
	MachinePct float64
	OwnPct     float64
}

// ForeignPct is the machine's busy percentage that is not Lettuce's: what
// other programs are using. Never negative — the two figures come from
// different clocks and can disagree by a little.
func (s CPULoadSample) ForeignPct() float64 {
	f := s.MachinePct - s.OwnPct
	if f < 0 {
		return 0
	}
	return f
}

// CPULoadSampler is what the yield monitor polls.
type CPULoadSampler interface {
	Sample() (CPULoadSample, error)
}

// cpuTimes is a cumulative (busy, total) pair in any unit — jiffies, 100 ns
// ticks — where total includes idle time.
type cpuTimes struct {
	busy, total uint64
}

// deltaSampler turns a cumulative counter reader into per-interval busy
// percentages.
type deltaSampler struct {
	read   func() (cpuTimes, error)
	prev   cpuTimes
	primed bool
}

func (d *deltaSampler) Sample() (float64, error) {
	cur, err := d.read()
	if err != nil {
		d.primed = false
		return 0, err
	}
	if !d.primed {
		d.prev = cur
		d.primed = true
		return 0, ErrNoBaseline
	}
	prev := d.prev
	d.prev = cur
	if cur.total <= prev.total || cur.busy < prev.busy {
		return 0, nil // counters reset or no time passed: say nothing rather than nonsense
	}
	dBusy := cur.busy - prev.busy
	dTotal := cur.total - prev.total
	pct := 100 * float64(dBusy) / float64(dTotal)
	if pct > 100 {
		pct = 100
	}
	return pct, nil
}

// parseProcStatCPU reads the aggregate "cpu" line of Linux's /proc/stat:
// user nice system idle iowait irq softirq steal … in jiffies. Busy is
// everything but idle and iowait; niced work counts, deliberately — the
// incumbent's gate ignores low-priority processes and its users file that as
// a bug.
func parseProcStatCPU(text string) (cpuTimes, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("/proc/stat cpu field %q: %w", f, err)
			}
			vals = append(vals, v)
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait
		}
		return cpuTimes{busy: total - idle, total: total}, nil
	}
	return cpuTimes{}, errors.New("/proc/stat has no aggregate cpu line")
}

// parseTopCPUUsage reads the LAST "CPU usage: 3.57% user, 7.14% sys, 89.28%
// idle" line of macOS `top -l 2` output. The first sample top prints is the
// average since boot; the second is the interval between the two, which is
// why the last line is the one wanted.
func parseTopCPUUsage(text string) (float64, error) {
	var last string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "CPU usage:") {
			last = line
		}
	}
	if last == "" {
		return 0, errors.New("top output has no CPU usage line")
	}
	var user, sys float64
	var haveUser, haveSys bool
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimSpace(last), "CPU usage:"), ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSuffix(fields[0], "%"), 64)
		if err != nil {
			return 0, fmt.Errorf("top CPU usage %q: %w", part, err)
		}
		switch fields[1] {
		case "user":
			user, haveUser = v, true
		case "sys":
			sys, haveSys = v, true
		}
	}
	if !haveUser || !haveSys {
		return 0, fmt.Errorf("top CPU usage line %q lacks user/sys", strings.TrimSpace(last))
	}
	pct := user + sys
	if pct > 100 {
		pct = 100
	}
	return pct, nil
}

// parsePSCPUTime parses the cputime column of BSD/macOS `ps` — "MM:SS.ss",
// "HH:MM:SS" or "D-HH:MM:SS" — into seconds.
func parsePSCPUTime(s string) (float64, error) {
	s = strings.TrimSpace(s)
	days := 0.0
	if i := strings.Index(s, "-"); i > 0 {
		d, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("ps cputime %q: %w", s, err)
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var secs float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, fmt.Errorf("ps cputime %q: %w", s, err)
		}
		secs = secs*60 + v
	}
	return days*86400 + secs, nil
}

// cpuLoadSampler combines a machine sampler with Lettuce's own cumulative CPU
// seconds, converting the latter to a percentage of all cores over the same
// interval.
type cpuLoadSampler struct {
	machine MachineCPUSampler
	own     OwnCPUSecondsFunc
	cores   int
	now     func() time.Time

	prevOwn float64
	prevAt  time.Time
	primed  bool
}

// NewCPULoadSampler builds the sampler the yield monitor polls: machine
// reports the whole machine, own reports Lettuce's cumulative CPU seconds,
// cores is the host's core count the percentages are of.
func NewCPULoadSampler(machine MachineCPUSampler, own OwnCPUSecondsFunc, cores int, now func() time.Time) CPULoadSampler {
	if now == nil {
		now = time.Now
	}
	if cores < 1 {
		cores = 1
	}
	return &cpuLoadSampler{machine: machine, own: own, cores: cores, now: now}
}

func (s *cpuLoadSampler) Sample() (CPULoadSample, error) {
	busy, mErr := s.machine.Sample()
	ownSec, oErr := s.own()
	at := s.now()
	if oErr != nil {
		s.primed = false
		return CPULoadSample{}, fmt.Errorf("Lettuce's own CPU use: %w", oErr)
	}
	if mErr != nil && !errors.Is(mErr, ErrNoBaseline) {
		s.primed = false
		return CPULoadSample{}, fmt.Errorf("machine CPU use: %w", mErr)
	}
	if !s.primed || errors.Is(mErr, ErrNoBaseline) {
		s.prevOwn, s.prevAt, s.primed = ownSec, at, true
		return CPULoadSample{}, ErrNoBaseline
	}
	elapsed := at.Sub(s.prevAt).Seconds()
	dOwn := ownSec - s.prevOwn
	s.prevOwn, s.prevAt = ownSec, at

	ownPct := 0.0
	if elapsed > 0 && dOwn > 0 {
		ownPct = 100 * dOwn / (elapsed * float64(s.cores))
	}
	if ownPct > 100 {
		ownPct = 100
	}
	return CPULoadSample{MachinePct: busy, OwnPct: ownPct}, nil
}
