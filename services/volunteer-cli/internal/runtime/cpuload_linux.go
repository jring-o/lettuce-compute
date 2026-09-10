//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procStatPath is /proc/stat; a variable so tests can point it at a fixture.
var procStatPath = "/proc/stat"

// procRoot is /proc; a variable so the process-group scan can be pointed at
// a synthetic tree in tests.
var procRoot = "/proc"

// clockTicksPerSecond is the kernel's USER_HZ, which /proc/[pid]/stat reports
// CPU time in. 100 on every Linux Go supports (the same assumption
// procmetrics makes).
const clockTicksPerSecond = 100.0

// NewMachineCPUSampler reads whole-machine CPU time from /proc/stat.
func NewMachineCPUSampler() MachineCPUSampler {
	return &deltaSampler{read: func() (cpuTimes, error) {
		data, err := os.ReadFile(procStatPath)
		if err != nil {
			return cpuTimes{}, err
		}
		return parseProcStatCPU(string(data))
	}}
}

// ProcessGroupsCPUSeconds sums the CPU time (user + system, plus the time of
// children each process has already reaped) of every process whose process
// group is one of pgids, keyed by that group. A native task is its own group
// leader (Setpgid), so this is the whole tree of every native task the daemon
// started, however many helpers it forked. A group with no live process is
// simply absent from the result.
func ProcessGroupsCPUSeconds(pgids []int) (map[int]float64, error) {
	want := make(map[int]bool, len(pgids))
	for _, pgid := range pgids {
		want[pgid] = true
	}
	out := make(map[int]float64)
	if len(want) == 0 {
		return out, nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", procRoot, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		pgrp, secs, ok := parseProcPIDStat(string(data))
		if !ok || !want[pgrp] {
			continue
		}
		out[pgrp] += secs
	}
	return out, nil
}

// parseProcPIDStat extracts the process group and the CPU seconds (utime +
// stime + cutime + cstime) from one /proc/[pid]/stat line. The comm field is
// parenthesised and may itself contain spaces and parentheses, so fields are
// counted from the LAST ")" rather than split from the start.
func parseProcPIDStat(stat string) (pgrp int, cpuSeconds float64, ok bool) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, 0, false
	}
	// After ")": state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime … (stat fields 3 onward).
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 15 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	var ticks float64
	for _, i := range []int{11, 12, 13, 14} { // utime stime cutime cstime
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, 0, false
		}
		ticks += v
	}
	return pgrp, ticks / clockTicksPerSecond, true
}
