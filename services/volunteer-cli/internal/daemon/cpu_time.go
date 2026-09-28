package daemon

import (
	"context"
	"fmt"
	"math"
	"time"
)

// The CPU time limit (resource_limits.max_cpu_time_pct).
//
// "Use at most N % of CPU time" lowers the machine's load and heat without
// changing how many tasks run or the cores each is given: below 100 %, all of
// Lettuce's running work is paused for part of every period and resumed for
// the rest, so it runs N % of the time. It is a fourth automatic pause source
// beside the schedule, the thermal monitor and the yield monitor, with the
// same freeze and thaw, and it is released whenever the setting is raised to
// 100.
//
// Pausing is used rather than a lower CPU quota because a quota slows a task
// by stopping it inside every scheduling period — measured as CPU pressure
// that stalls the whole task — while a paused task simply does not run. A
// task that pauses and resumes shows no pressure from the limit at all.
//
// The period is long because pausing and resuming a container through its
// engine takes about a quarter of a second each way: a 1-second cycle would
// spend half of it switching. It is at least cpuTimeMinPeriod, and longer at
// the ends of the range so that neither part of it is shorter than
// cpuTimeMinPhase: 50 % runs 5 s of every 10 s, 10 % 1 s of every 10 s,
// 5 % 1 s of every 20 s.
//
// Unlike the other sources, the limit's paused part is not reported as the
// daemon being paused (IsPaused, PauseReason): it is a standing setting that
// runs work part of the time, and a status that said "paused" for a few
// seconds at a time would only flicker. `status`, the app and each running
// task name the limit instead (CPUTimeLimit). Nor does it stop the fetcher,
// as the other automatic pauses do: the work buffer is sized for the limit
// (bufferSecondsPerSlot) and keeps being filled.
//
// Every running task is paused, GPU tasks included. WebAssembly tasks run
// inside the daemon's own process and cannot be paused, so they are not
// limited.

// cpuTimeMinPeriod is the shortest cycle of the CPU time limit, and
// cpuTimeMinPhase the shortest running or paused part of it. Variables so
// tests can run the cycle quickly.
var (
	cpuTimeMinPeriod = 10 * time.Second
	cpuTimeMinPhase  = time.Second
)

// cpuTimePoll is how often the limit's cycle re-reads the setting while it
// waits, so a change takes effect within about a second.
var cpuTimePoll = time.Second

// cpuTimeCycle is the running and paused parts of one period of the CPU time
// limit at pct percent; zero for both when pct sets no limit.
func cpuTimeCycle(pct int) (run, pause time.Duration) {
	if pct <= 0 || pct >= 100 {
		return 0, 0
	}
	share := float64(pct) / 100
	period := cpuTimeMinPeriod
	if p := time.Duration(float64(cpuTimeMinPhase) / math.Min(share, 1-share)).Round(10 * time.Millisecond); p > period {
		period = p
	}
	run = time.Duration(float64(period) * share).Round(10 * time.Millisecond)
	return run, period - run
}

// CPUTimeCycle is cpuTimeCycle for other packages: the running and paused
// parts of one period at pct percent.
func CPUTimeCycle(pct int) (run, pause time.Duration) {
	return cpuTimeCycle(pct)
}

// cpuTimePct is the live max_cpu_time_pct (100 when no limit is set).
func (d *Daemon) cpuTimePct() int {
	cfg := d.cfg
	if cfg == nil {
		return 100
	}
	return cfg.ResourceLimits.CPUTimePct()
}

// CPUTimeLimit reports the CPU time limit in force: the percentage of the
// time work runs and the running and paused parts of each period. pct is 100
// and both parts zero when no limit is set.
func (d *Daemon) CPUTimeLimit() (pct int, run, pause time.Duration) {
	pct = d.cpuTimePct()
	run, pause = cpuTimeCycle(pct)
	return pct, run, pause
}

// DescribeCPUTimeLimit is the one-line description of a CPU time limit that
// `status`, the app and each running task show: "Runs 50 % of the time (5 s
// of every 10 s): CPU time limit".
func DescribeCPUTimeLimit(pct int, run, pause time.Duration) string {
	return fmt.Sprintf("Runs %d %% of the time (%s of every %s): CPU time limit", pct, roundedSeconds(run), roundedSeconds(run+pause))
}

// roundedSeconds prints a cycle length: "5 s", "0.5 s".
func roundedSeconds(d time.Duration) string {
	s := d.Seconds()
	if s == math.Trunc(s) {
		return fmt.Sprintf("%d s", int(s))
	}
	return fmt.Sprintf("%.1f s", s)
}

// runCPUTimeLimit runs the CPU time limit's cycle until ctx ends: it lets
// work run for the running part of each period, then signals the run loop
// to pause everything for the paused part. The setting is re-read every
// cpuTimePoll; a change starts a new cycle, and a limit lifted mid-pause
// releases it at once.
func (d *Daemon) runCPUTimeLimit(ctx context.Context) {
	holding := false
	release := func() {
		if holding {
			holding = false
			d.signalCPUTimePause(ctx, false)
		}
	}
	defer release()
	for ctx.Err() == nil {
		pct := d.cpuTimePct()
		run, pause := cpuTimeCycle(pct)
		if pause <= 0 {
			release()
			if !sleepCtx(ctx, cpuTimePoll) {
				return
			}
			continue
		}
		if !d.waitWhileCPUTimePct(ctx, pct, run) {
			continue // the setting changed (or ctx ended): start over
		}
		holding = true
		d.signalCPUTimePause(ctx, true)
		d.waitWhileCPUTimePct(ctx, pct, pause)
		release()
	}
}

// waitWhileCPUTimePct waits for d in steps of cpuTimePoll and reports whether
// it waited it all out with the setting still at pct and ctx still live.
func (d *Daemon) waitWhileCPUTimePct(ctx context.Context, pct int, dur time.Duration) bool {
	deadline := time.Now().Add(dur)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return true
		}
		if left > cpuTimePoll {
			left = cpuTimePoll
		}
		if !sleepCtx(ctx, left) || d.cpuTimePct() != pct {
			return false
		}
	}
}

// signalCPUTimePause delivers one of the limit's transitions to the run loop,
// blocking until it is received or ctx ends — a lost resume would leave work
// paused (the thermal monitor's rule, #61).
func (d *Daemon) signalCPUTimePause(ctx context.Context, pause bool) {
	select {
	case d.cpuTimePauseCh <- pause:
	case <-ctx.Done():
	}
}

// sleepCtx waits for d or until ctx ends; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
