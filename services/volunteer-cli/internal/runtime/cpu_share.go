package runtime

import (
	"fmt"
	"math"
	"strconv"
)

// The CPU budget and how running tasks share it (TB-75).
//
// resource_limits.max_cpu_cores is the most CPU Lettuce may use on the whole
// machine — the meaning the memory and disk limits beside it have always had,
// and the meaning every volunteer read into "CPU Cores · 2 / 8". It used to be
// applied per TASK: every container and every native process was given the
// full figure as its own quota, nothing booked cores at admission, and so the
// real ceiling was max_cpu_cores × max_concurrent_tasks. A tester who "left
// half the machine" with 2 cores and allowed 2 tasks had left nothing: both
// tasks ran at two cores each and filled every vCPU of his Podman machine.
//
// Now the figure is a total that the running tasks share equally. A task alone
// is given the whole budget; when a second starts each is given half; when one
// finishes the survivor is given the whole again. Each task's quota is set to
// its share when it starts and adjusted live on every start and finish (the
// container engine's update call, a cgroup cpu.max rewrite, a Job Object rate
// change), and every task is told its share through the environment so it can
// size its worker pool to what it was actually given rather than to the count
// of CPUs it can see.

// CFSPeriodMicros is the CFS bandwidth period every CPU quota is expressed
// against: 100 ms, the engine and kernel default. A quota of
// share × CFSPeriodMicros microseconds per period is "share cores".
const CFSPeriodMicros int64 = 100000

// cpuShareGranularity is the resolution shares are rounded DOWN to: one
// hundredth of a core, the CFS quota granularity (1000 µs of a 100000 µs
// period). Rounding down keeps the sum of the shares at or below the budget.
const cpuShareGranularity = 100

// CPUGrant is the CPU a task is given when it starts: its equal share of the
// volunteer's CPU budget, and the budget itself. ShareCores may be fractional
// (a budget of 3 shared by two tasks is 1.5 each). A zero ShareCores means no
// CPU limit is in force — nothing is enforced and nothing is told the task.
type CPUGrant struct {
	// ShareCores is this task's share of the budget: budget / running tasks,
	// rounded down to a hundredth of a core.
	ShareCores float64
	// BudgetCores is the whole-machine budget the share is part of. The Linux
	// affinity fallback, which cannot express a fractional share, confines
	// every task to this many CPUs instead — the same set for all of them, so
	// the total still cannot exceed the budget.
	BudgetCores int
}

// CPUShareCores returns the share of budgetCores each of running tasks is
// given: an equal split, rounded down to a hundredth of a core so the shares
// never sum to more than the budget. A budget of 0 (no limit) yields 0; fewer
// than one running task is treated as one (the share a lone task would get).
func CPUShareCores(budgetCores, running int) float64 {
	if budgetCores <= 0 {
		return 0
	}
	if running < 1 {
		running = 1
	}
	return math.Floor(float64(budgetCores)/float64(running)*cpuShareGranularity) / cpuShareGranularity
}

// CFSQuota converts a share into the quota/period pair the container engine
// and cgroups v2 enforce: quota microseconds of CPU time per period. A share
// of 0 (no limit) yields 0/0, which the engine reads as unlimited. The quota
// is floored at the kernel's minimum of 1 ms so a very small share is still a
// valid quota rather than a rejected one.
func CFSQuota(shareCores float64) (quota, period int64) {
	if shareCores <= 0 {
		return 0, 0
	}
	quota = int64(math.Round(shareCores * float64(CFSPeriodMicros)))
	if quota < 1000 {
		quota = 1000
	}
	return quota, CFSPeriodMicros
}

// ContainerCPUBudget returns the CPU budget this machine can actually work to:
// the configured whole-machine budget (configCores,
// resource_limits.max_cpu_cores), clipped to the number of CPUs the container
// engine's virtual machine has when the engine runs inside one and reported
// the count (engineCPUs, the engine's NCPU). On macOS and Windows every
// container runs inside that VM, so a budget above its vCPU count is a
// promise the machine cannot keep — the CPU twin of the memory clip
// (ContainerMemoryBudgetMB). An engineCPUs of zero or less means "no VM, or
// its size is unknown" and leaves the configuration as it is; a Linux host,
// where containers share the host's CPUs, is never clipped here.
//
// This is the number to advertise to heads, to book admission against and to
// share among running tasks: one figure for all three.
func ContainerCPUBudget(configCores, engineCPUs int) int {
	if engineCPUs <= 0 || configCores <= 0 {
		return configCores
	}
	if engineCPUs < configCores {
		return engineCPUs
	}
	return configCores
}

// Threads is the whole number of worker threads the share amounts to, for the
// thread-count knobs: the share rounded to the nearest whole core, never
// below one. A task granted 1.5 cores is told 2 threads (CFS lets two threads
// share one and a half cores); one granted 0.5 is told 1.
func (g CPUGrant) Threads() int {
	n := int(math.Round(g.ShareCores))
	if n < 1 {
		n = 1
	}
	return n
}

// Env returns the environment entries (KEY=value) that tell a task its CPU
// share: LETTUCE_CPU_LIMIT carries the share exactly (it may be fractional,
// "1.5"), and the standard thread-pool knobs — OMP_NUM_THREADS,
// OPENBLAS_NUM_THREADS, MKL_NUM_THREADS, NUMEXPR_MAX_THREADS — carry it as a
// whole thread count, so a numeric library that reads them sizes its pool to
// the share rather than to every CPU it can see. Nil when no CPU limit is in
// force. The share is what the task was given when it STARTED; a share that
// later changes (another task starting or finishing) reaches only tasks
// started afterwards, since a running process cannot be told.
func (g CPUGrant) Env() []string {
	if g.ShareCores <= 0 {
		return nil
	}
	threads := strconv.Itoa(g.Threads())
	return []string{
		"LETTUCE_CPU_LIMIT=" + FormatCores(g.ShareCores),
		"OMP_NUM_THREADS=" + threads,
		"OPENBLAS_NUM_THREADS=" + threads,
		"MKL_NUM_THREADS=" + threads,
		"NUMEXPR_MAX_THREADS=" + threads,
	}
}

// FormatCores prints a core count the way a volunteer reads it: whole numbers
// without a decimal point ("2"), fractions to at most two places ("1.5",
// "0.67").
func FormatCores(cores float64) string {
	if cores == math.Trunc(cores) {
		return strconv.FormatInt(int64(cores), 10)
	}
	return strconv.FormatFloat(cores, 'f', -1, 64)
}

// staticCPUGrant returns a grant source that always hands out the whole
// budget — the grant a runtime uses until the daemon wires its live one, and
// the grant a runtime driven outside the daemon (the audit runner) works
// with, where one task runs at a time.
func staticCPUGrant(budgetCores int) func() CPUGrant {
	return func() CPUGrant {
		return CPUGrant{ShareCores: CPUShareCores(budgetCores, 1), BudgetCores: budgetCores}
	}
}

// String describes the grant for logs.
func (g CPUGrant) String() string {
	if g.ShareCores <= 0 {
		return "no CPU limit"
	}
	return fmt.Sprintf("%s of %d cores", FormatCores(g.ShareCores), g.BudgetCores)
}
