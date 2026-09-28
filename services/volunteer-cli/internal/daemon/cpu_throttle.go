package daemon

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Flagging a task that wants more cores than it was granted.
//
// Each task is held to its grant. A task that runs no more threads than the
// cores it was given is never slowed by that; one that runs more is stopped by
// its quota in most CPU periods, and every stop stalls the whole task — the
// stall a tester saw as "CPU pressure" on his hypervisor while a GREP unit
// crawled past its deadline. The watch below reads each running task's
// throttling once a minute and, while a leaf's task spends most of its periods
// stopped, keeps a notice naming the leaf, its grant and the remedy; the
// notice is resolved once no task of the leaf is being stopped. Readings come
// from the engine or the container's own cgroup for a container task and from
// the task's cgroup for a native task on Linux; a native task on Windows or
// macOS has no throttle count to read and is not flagged.

// cpuThrottleInterval is how often each running task's throttling is read.
const cpuThrottleInterval = time.Minute

// cpuThrottledShare is the share of a task's CPU periods, over one interval,
// in which its quota must have stopped it for the task to count as wanting
// more cores than its grant. A task at its grant with no more threads than
// cores is stopped in few or none.
const cpuThrottledShare = 0.5

// cpuThrottleMinPeriods is the fewest periods (100 ms each) a task must have
// run in over an interval for its share to say anything: a task that barely
// ran, or was suspended, is not judged.
const cpuThrottleMinPeriods = 100

// cpuThrottleNoticeCode is the notice a leaf gets while its tasks are stopped
// by their grants.
const cpuThrottleNoticeCode = "task_cpu_throttled"

// cpuThrottleReader is a limiter that can say how often a native task's quota
// stopped it (the Linux cgroup path).
type cpuThrottleReader interface {
	CPUThrottling(pid int) (runtime.CPUThrottle, bool)
}

// throttleTarget is one running task whose throttling can be read.
type throttleTarget struct {
	wu        *runtime.WorkUnit
	head      string
	container *OwnContainer // a container task's engine and container
	pid       int           // a native task's process
}

// throttleTargets lists the running tasks with a process to read: container
// tasks and native tasks whose handle is attached, not suspended.
func (sm *SlotManager) throttleTargets() []throttleTarget {
	var out []throttleTarget
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.processHandle != nil && !slot.suspended {
			t := throttleTarget{wu: slot.wu}
			if slot.conn != nil {
				t.head = slot.conn.Name
			}
			switch h := slot.processHandle.(type) {
			case *containerProcessHandle:
				t.container = &OwnContainer{Client: h.client, ContainerID: h.containerID}
			case *nativeProcessHandle:
				t.pid = h.PID()
			}
			if t.container != nil || t.pid > 0 {
				out = append(out, t)
			}
		}
		slot.mu.Unlock()
	}
	return out
}

// cpuThrottleWatch keeps each task's last reading, to measure the next
// interval against, and the leafs currently flagged.
type cpuThrottleWatch struct {
	mu      sync.Mutex
	last    map[string]runtime.CPUThrottle // work unit id -> last reading
	flagged map[headLeafKey]bool
}

type headLeafKey struct{ head, leaf string }

// throttledTask is a task its quota stopped in share of its periods over the
// last interval.
type throttledTask struct {
	wu    *runtime.WorkUnit
	share float64
}

// runCPUThrottleWatch reads the running tasks' throttling every
// cpuThrottleInterval until ctx ends.
func (d *Daemon) runCPUThrottleWatch(ctx context.Context) {
	ticker := time.NewTicker(cpuThrottleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.checkCPUThrottling(ctx)
		}
	}
}

// readThrottle reads one task's throttling; false when there is no reading.
func (d *Daemon) readThrottle(ctx context.Context, t throttleTarget) (runtime.CPUThrottle, bool) {
	if t.container != nil {
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		r, err := runtime.ContainerCPUThrottling(readCtx, t.container.Client, t.container.ContainerID)
		if err != nil {
			d.logger.Debug("no CPU throttling reading for container task", "work_unit_id", t.wu.ID, "error", err)
			return runtime.CPUThrottle{}, false
		}
		return r, true
	}
	if reader, ok := d.limiter.(cpuThrottleReader); ok {
		return reader.CPUThrottling(t.pid)
	}
	return runtime.CPUThrottle{}, false
}

// checkCPUThrottling reads every running task once, keeps a notice for each
// leaf with a task its quota stopped in at least cpuThrottledShare of its
// periods since the last reading, and resolves the notice of a leaf with no
// such task. Never sticky: each pass judges only the interval just ended, and
// a leaf whose task it cannot judge (a first reading, a minute it barely ran)
// keeps the state it had.
func (d *Daemon) checkCPUThrottling(ctx context.Context) {
	sm := d.slotManager // one read: the daemon clears it when it stops
	if sm == nil {
		return
	}
	targets := sm.throttleTargets()
	readings := make(map[string]runtime.CPUThrottle, len(targets))
	for _, t := range targets {
		if r, ok := d.readThrottle(ctx, t); ok {
			readings[t.wu.ID] = r
		}
	}

	w := &d.cpuThrottle
	w.mu.Lock()
	if w.last == nil {
		w.last = make(map[string]runtime.CPUThrottle)
		w.flagged = make(map[headLeafKey]bool)
	}
	throttled := make(map[headLeafKey]throttledTask)
	// undecided: leafs with a running task this pass could not judge (a first
	// reading, a task that barely ran or had no reading): their state stands.
	undecided := make(map[headLeafKey]bool)
	for _, t := range targets {
		key := headLeafKey{head: t.head, leaf: t.wu.LeafID}
		r, ok := readings[t.wu.ID]
		prev, had := w.last[t.wu.ID]
		if !ok || !had || r.Periods < prev.Periods || r.Throttled < prev.Throttled || r.Periods-prev.Periods < cpuThrottleMinPeriods {
			undecided[key] = true
			continue
		}
		share := float64(r.Throttled-prev.Throttled) / float64(r.Periods-prev.Periods)
		if share >= cpuThrottledShare && share > throttled[key].share {
			throttled[key] = throttledTask{wu: t.wu, share: share}
		}
	}
	w.last = readings
	var raise, resolve []headLeafKey
	for key := range throttled {
		if !w.flagged[key] {
			raise = append(raise, key)
		}
	}
	flagged := make(map[headLeafKey]bool, len(throttled))
	for key := range w.flagged {
		if _, still := throttled[key]; still {
			continue
		}
		if undecided[key] {
			flagged[key] = true // not judged this pass: the notice stands
			continue
		}
		resolve = append(resolve, key)
	}
	for key := range throttled {
		flagged[key] = true
	}
	w.flagged = flagged
	w.mu.Unlock()

	newly := make(map[headLeafKey]bool, len(raise))
	for _, key := range raise {
		newly[key] = true
	}
	keys := make([]headLeafKey, 0, len(throttled))
	for key := range throttled {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].head+keys[i].leaf < keys[j].head+keys[j].leaf })
	for _, key := range keys {
		d.flagCPUThrottled(key, throttled[key], newly[key])
	}
	for _, key := range resolve {
		d.notices.Resolve(cpuThrottleNoticeCode, key.head, key.leaf)
	}
}

// flagCPUThrottled keeps the notice for one leaf whose task wants more cores
// than its grant, and logs once when the condition begins.
func (d *Daemon) flagCPUThrottled(key headLeafKey, t throttledTask, begins bool) {
	leafName, _ := d.resolveLeafInfo(key.leaf)
	minCores, maxCores := d.declaredCoreRange(key.leaf)
	granted := t.wu.CPUGrant.Cores
	pct := int(t.share*100 + 0.5)
	why := fmt.Sprintf("That is all its leaf declares it can use (max_cpu_cores %d), so the leaf's program runs more threads than it declares: its owner can size the program's threads to LETTUCE_CPU_LIMIT, or raise the leaf's max_cpu_cores.", maxCores)
	switch override := d.leafOverride(key.leaf).cores; {
	case granted < maxCores && override > 0 && override < maxCores:
		why = fmt.Sprintf("Its leaf can use up to %d; you set its tasks to %s each. Raise that (lettuce-volunteer leafs cores, or the leaf's card in the app) to give its tasks more.", maxCores, plural(override, "core"))
	case granted < maxCores:
		why = fmt.Sprintf("Its leaf can use up to %d; the task was given %d because other tasks needed the rest of your CPU limit when it started. A higher max_cpu_cores lets such tasks be given more.", maxCores, granted)
	}
	message := fmt.Sprintf("A %s task is being slowed by its CPU grant: in the last minute its CPU limit stopped it in %d%% of its scheduling periods, so it tries to use more than the %s it was given (the leaf asks for %s). Lettuce holds each task to the cores it grants. %s",
		leafName, pct, plural(granted, "core"), coreRangeText(minCores, maxCores), why)
	d.notices.Notify(NoticeWarn, cpuThrottleNoticeCode, message, key.head, key.leaf)
	if begins {
		d.logger.Warn("a task is being slowed by its CPU grant: it wants more cores than it was given",
			"work_unit_id", t.wu.ID, "leaf_id", key.leaf, "leaf_name", leafName, "head", key.head,
			"cpu_cores", granted, "leaf_min_cpu_cores", minCores, "leaf_max_cpu_cores", maxCores,
			"throttled_share", fmt.Sprintf("%.2f", t.share))
	}
}

// coreRangeText prints a leaf's core range: "2–4 cores", "1 core".
func coreRangeText(minCores, maxCores int) string {
	if maxCores > minCores {
		return fmt.Sprintf("%d–%d cores", minCores, maxCores)
	}
	return plural(minCores, "core")
}
