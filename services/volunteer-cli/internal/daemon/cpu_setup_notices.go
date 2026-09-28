package daemon

import (
	"fmt"
	"math"
	goruntime "runtime"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Notices about a CPU setup that works against the volunteer, each with the
// leaf it concerns (where it concerns one) and what would help. They are
// worked out afresh every buffer-maintenance tick, when the settings change
// and when the leaf catalog is refreshed, and resolved as soon as the
// condition is gone: none is left standing on the strength of a state that
// has passed.
//
//   - A leaf this machine cannot finish before its deadline (deadline_skip.go).
//   - A leaf that can never start here: its tasks need more cores or memory
//     than the limit allows, so heads do not send it. The container engine's
//     VM being the bound has its own notices (refreshContainerVMNotices).
//   - A CPU limit that runs one task at a time on a machine whose CPUs and
//     memory limit would run several — the setting read as "cores per task".
//   - A task held below the cores it wants is flagged by the throttle watch
//     (cpu_throttle.go).

// hostCPUCount is how many CPUs this machine has; a variable so tests can
// stand in for a larger machine.
var hostCPUCount = goruntime.NumCPU

// Notice codes of the CPU setup notices.
const (
	noticeLeafCannotStart = "leaf_cannot_start"
	noticeCPUOneTask      = "cpu_limit_one_task"
)

// refreshCPUSetupNotices re-evaluates the CPU setup notices.
func (d *Daemon) refreshCPUSetupNotices() {
	if d.cfg == nil || d.leafCache == nil {
		return
	}
	d.setupNoticeMu.Lock()
	defer d.setupNoticeMu.Unlock()
	d.refreshDeadlineNotices()
	d.refreshCannotStartNotices()
	d.refreshOneTaskNotice()
}

// refreshLeafNotices is what a refreshed leaf catalog re-evaluates: the
// container engine VM's notices and the CPU setup notices.
func (d *Daemon) refreshLeafNotices() {
	d.refreshContainerVMNotices()
	d.refreshCPUSetupNotices()
}

// cannotStartReason says why a leaf's tasks can never start on this machine
// under its limits, and the remedy, or "". Only the configured limits are
// judged: a head compares the leaf's minimum cores and per-task memory with
// them and sends nothing when either falls short.
func (d *Daemon) cannotStartReason(leaf CachedLeafInfo) string {
	name := leafNoticeLabel(leaf)
	if name == "" {
		name = leaf.ID
	}
	if minCores, _ := coreRangeOf(leaf); leaf.ResourceRequirements != nil {
		if limit := d.HostCPUBudgetCores(); limit > 0 && minCores > limit {
			return fmt.Sprintf("%s needs at least %s for each of its tasks, and your CPU limit is %s in total, so heads do not send it to this machine. To run it, raise the CPU limit to %d (Settings → CPU Cores, or `lettuce-volunteer config set resource_limits.max_cpu_cores %d`), or disable %s here.",
				name, plural(minCores, "core"), plural(limit, "core"), minCores, minCores, name)
		}
	}
	if leaf.ExecutionSpec != nil {
		if need, limit := int(leaf.ExecutionSpec.MaxMemoryMB), d.HostMemoryBudgetMB(); need > 0 && limit > 0 && need > limit {
			return fmt.Sprintf("%s needs %d MB of memory for each of its tasks, and your memory limit is %d MB in total, so heads do not send it to this machine. To run it, raise the memory limit to at least %d MB (Settings → Memory), or disable %s here.",
				name, need, limit, need, name)
		}
	}
	return ""
}

// refreshCannotStartNotices keeps one notice per enabled leaf that can never
// start here, and resolves those that can again.
func (d *Daemon) refreshCannotStartNotices() {
	live := make(map[headLeafKey]bool)
	for _, hl := range d.enabledLeafsByHead() {
		if d.leafNeedsAbsentGPU(hl.leaf) {
			continue // a missing GPU is reported per leaf elsewhere
		}
		msg := d.cannotStartReason(hl.leaf)
		if msg == "" {
			continue
		}
		key := headLeafKey{head: hl.head, leaf: hl.leaf.ID}
		live[key] = true
		if d.setupNotices.changed(noticeLeafCannotStart, key, msg) {
			d.notices.Notify(NoticeWarn, noticeLeafCannotStart, msg, key.head, key.leaf)
			d.logger.Warn("an enabled leaf can never start on this machine under its limits", "head", key.head, "leaf_id", key.leaf, "reason", msg)
		}
	}
	for _, key := range d.setupNotices.forgetExcept(noticeLeafCannotStart, live) {
		d.notices.Resolve(noticeLeafCannotStart, key.head, key.leaf)
	}
}

// oneTaskAdvice is the notice for a CPU limit that runs one task at a time
// while this machine's CPUs and memory limit would run several, or "". It
// judges the enabled leaf that runs the most tasks at once: with a limit of
// 1 core and one-core tasks, "set 7 to run 7". A volunteer who capped
// running tasks at one chose one at a time, and is not told otherwise.
func (d *Daemon) oneTaskAdvice() string {
	limit := d.HostCPUBudgetCores()
	cpus := hostCPUCount()
	if limit <= 0 || limit >= cpus || d.maxRunningTasks() == 1 {
		return ""
	}
	best, bestTasks, perTask := CachedLeafInfo{}, 0, 0
	for _, leaf := range d.allEnabledLeafs() {
		minCores, _ := d.effectiveCoreRange(leaf)
		if tasks := limit / minCores; tasks > bestTasks {
			best, bestTasks, perTask = leaf, tasks, minCores
		}
	}
	if bestTasks != 1 {
		return ""
	}
	// How many of that leaf the machine's CPUs and the memory limit would run.
	could := cpus / perTask
	if memMB := d.HostMemoryBudgetMB(); memMB > 0 && best.ExecutionSpec != nil {
		if booked := runtime.BookedMemMB(int(best.ExecutionSpec.MaxMemoryMB), memMB); booked > 0 {
			could = int(math.Min(float64(could), float64(memMB/booked)))
		}
	}
	if limit := d.leafMaxRunning(best.ID); limit > 0 && limit < could {
		could = limit
	}
	if could < 2 {
		return ""
	}
	each := "each task needs at least one core"
	if perTask > 1 {
		each = fmt.Sprintf("each %s task needs %d cores", leafNoticeLabel(best), perTask)
	}
	return fmt.Sprintf("Your CPU limit is %s in total, and %s, so Lettuce runs one task at a time here. The limit is shared by all running tasks, not given to each. This machine has %d CPUs and your memory limit would run %d such tasks at once: to run %d at once, set the CPU limit to %d (Settings → CPU Cores, or `lettuce-volunteer config set resource_limits.max_cpu_cores %d`).",
		plural(limit, "core"), each, cpus, could, could, could*perTask, could*perTask)
}

// refreshOneTaskNotice keeps the one-task-at-a-time notice in step with the
// settings.
func (d *Daemon) refreshOneTaskNotice() {
	key := headLeafKey{}
	msg := d.oneTaskAdvice()
	if msg == "" {
		if len(d.setupNotices.forgetExcept(noticeCPUOneTask, nil)) > 0 {
			d.notices.Resolve(noticeCPUOneTask, "", "")
		}
		return
	}
	if d.setupNotices.changed(noticeCPUOneTask, key, msg) {
		d.notices.Notify(NoticeInfo, noticeCPUOneTask, msg, "", "")
		d.logger.Info("the CPU limit runs one task at a time on a machine that could run several", "advice", msg)
	}
}
