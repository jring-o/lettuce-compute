//go:build darwin

package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// NewMachineCPUSampler reads whole-machine CPU use on macOS from `top`.
//
// macOS has no /proc and exposes host CPU counters only through Mach calls
// this CGo-free build cannot make, so the sampler shells out to `top -l 2 -n
// 0 -s 1`: two samples one second apart with no per-process rows, of which
// the second's "CPU usage:" line is the machine's busy share over that
// second. It costs about a second of wall time per poll, spent inside the
// monitor's goroutine, and nothing else.
func NewMachineCPUSampler() MachineCPUSampler {
	return topSampler{}
}

type topSampler struct{}

func (topSampler) Sample() (float64, error) {
	out, err := CommandExecutor("top", "-l", "2", "-n", "0", "-s", "1")
	if err != nil {
		return 0, fmt.Errorf("top: %w", err)
	}
	return parseTopCPUUsage(string(out))
}

// ProcessGroupsCPUSeconds sums the accumulated CPU time of every process in
// each of pgids, keyed by group, from one `ps -A -o pgid=,cputime=` listing.
// A native task is its own group leader (Setpgid), so this is the whole tree
// of every native task the daemon started. A group with no live process is
// absent from the result.
func ProcessGroupsCPUSeconds(pgids []int) (map[int]float64, error) {
	want := make(map[int]bool, len(pgids))
	for _, pgid := range pgids {
		want[pgid] = true
	}
	out := make(map[int]float64)
	if len(want) == 0 {
		return out, nil
	}
	listing, err := CommandExecutor("ps", "-A", "-o", "pgid=,cputime=")
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePSProcessGroups(string(listing), want)
}

// parsePSProcessGroups reads "pgid cputime" rows and sums the wanted groups.
func parsePSProcessGroups(listing string, want map[int]bool) (map[int]float64, error) {
	out := make(map[int]float64)
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pgid, err := strconv.Atoi(fields[0])
		if err != nil || !want[pgid] {
			continue
		}
		secs, err := parsePSCPUTime(fields[1])
		if err != nil {
			return nil, err
		}
		out[pgid] += secs
	}
	return out, nil
}
