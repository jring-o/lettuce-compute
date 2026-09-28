package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// checkCPUTimeLimit reports the CPU time limit (resource_limits.max_cpu_time_pct)
// when one is set: running work is paused and resumed in turn so it runs
// that share of the time, which stretches every unit's run by the inverse.
// Nothing is printed at 100 %, no limit.
func checkCPUTimeLimit(rep *doctorReport, limits config.ResourceLimits) {
	pct := limits.CPUTimePct()
	if pct >= 100 {
		return
	}
	run, pause := daemon.CPUTimeCycle(pct)
	rep.add(docInfo, "cpu time",
		fmt.Sprintf("%s (resource_limits.max_cpu_time_pct %d) — every running task is paused and resumed in turn, so a unit takes about %.1f times as long as it would running continuously; the work buffer is sized for that",
			daemon.DescribeCPUTimeLimit(pct, run, pause), pct, 100/float64(pct)), "")
}

// checkLeafOverrides lists the volunteer's per-leaf CPU overrides
// (leaf_preferences.cores and .max_running) and, with the daemon running,
// what each comes to on this machine: the cores each of the leaf's tasks is
// given, kept within the leaf's range and the CPU limit, and how many run at
// once. Nothing is printed when no override is set.
func checkLeafOverrides(rep *doctorReport, servers []config.ServerConfig, live *leafsAPIResponse) {
	var parts []string
	for _, srv := range servers {
		lp := srv.LeafPreferences
		slugs := make(map[string]bool)
		for slug := range lp.Cores {
			slugs[slug] = true
		}
		for slug := range lp.MaxRunning {
			slugs[slug] = true
		}
		ordered := make([]string, 0, len(slugs))
		for slug := range slugs {
			ordered = append(ordered, slug)
		}
		sort.Strings(ordered)
		for _, slug := range ordered {
			var set []string
			if n := lp.Cores[slug]; n > 0 {
				set = append(set, pluralCount(n, "core")+" per task")
			}
			if n := lp.MaxRunning[slug]; n > 0 {
				set = append(set, fmt.Sprintf("at most %d running", n))
			}
			part := fmt.Sprintf("%s on %s: %s", slug, srv.DisplayName(), strings.Join(set, ", "))
			if c := liveLeafCPU(live, srv, slug); c != nil {
				part += fmt.Sprintf(" (here: %s each, %d at once)", coreRangeLabel(c.TaskCoresMin, c.TaskCoresMax), c.RunsAtOnce)
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return
	}
	rep.add(docInfo, "leaf cpu overrides",
		strings.Join(parts, "; ")+" — a leaf's tasks are given the cores set here within the range the leaf declares, they apply to tasks started afterwards (`lettuce-volunteer tasks restart <id>` restarts a running one), and the work buffer holds no more of a leaf than they let run", "")
}

// liveLeafCPU finds a leaf's CPU arrangement in the running daemon's leaf
// list; nil when there is no daemon or it does not list the leaf.
func liveLeafCPU(live *leafsAPIResponse, srv config.ServerConfig, slug string) *leafsAPICPU {
	if live == nil {
		return nil
	}
	for _, h := range live.Heads {
		if !strings.EqualFold(h.GRPCAddress, srv.GRPCAddress) && h.Name != srv.DisplayName() {
			continue
		}
		for _, l := range h.Leafs {
			if l.Slug == slug {
				return l.CPU
			}
		}
	}
	return nil
}
