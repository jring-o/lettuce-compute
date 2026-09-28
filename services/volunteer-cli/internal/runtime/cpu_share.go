package runtime

import (
	"fmt"
	"strconv"
)

// The CPU budget and the grant each task is given.
//
// resource_limits.max_cpu_cores is the most CPU Lettuce may use on the whole
// machine — the meaning the memory and disk limits beside it have always had.
// It used to be applied per TASK (every task was given the whole figure, so the
// real ceiling was the figure times the number of tasks), and then as a total
// the running tasks shared EQUALLY, re-split live on every start and finish.
// The equal split held a task that could use three cores to the same share as
// a single-threaded one beside it, so wide work ran past its deadline while
// the narrow work left its share partly idle.
//
// Now each task is given a GRANT when it starts: whole cores between its
// leaf's declared minimum and maximum, decided by the daemon against what the
// budget has free, and kept until the task finishes. The task is held to its
// grant (a CFS quota for a container, a cgroup cpu.max, a Job Object rate)
// and told it through the environment, so it sizes its worker pool to what it
// was given rather than to the count of CPUs it can see. The grants of the
// running tasks together never exceed the budget.

// CFSPeriodMicros is the CFS bandwidth period every CPU quota is expressed
// against: 100 ms, the engine and kernel default. A quota of
// cores × CFSPeriodMicros microseconds per period is "cores cores".
const CFSPeriodMicros int64 = 100000

// CPUGrant is the CPU a task is given when it starts: whole cores of the
// volunteer's CPU budget, and the budget itself. A zero Cores means no CPU
// limit is in force — nothing is enforced and nothing is told the task.
type CPUGrant struct {
	// Cores is the number of cores this task is given: a whole number
	// between its leaf's minimum and maximum. It stays fixed for the task's
	// run.
	Cores int
	// BudgetCores is the budget the grant is part of: the whole-machine
	// budget for a task that runs directly on the machine, the container
	// budget for a container task. The Linux affinity fallback, which cannot
	// hold one process to a CPU quota, confines every task to this many CPUs
	// instead — the same set for all of them, so the total still cannot
	// exceed the budget.
	BudgetCores int
}

// CFSQuota converts a grant into the quota/period pair the container engine
// and cgroups v2 enforce: quota microseconds of CPU time per period. A grant
// of 0 (no limit) yields 0/0, which the engine reads as unlimited.
func CFSQuota(cores int) (quota, period int64) {
	if cores <= 0 {
		return 0, 0
	}
	return int64(cores) * CFSPeriodMicros, CFSPeriodMicros
}

// ContainerCPUBudget returns the CPU budget container work can actually get on
// this machine: the configured whole-machine budget (configCores,
// resource_limits.max_cpu_cores), clipped to the number of CPUs the container
// engine's virtual machine has when the engine runs inside one and reported
// the count (engineCPUs, the engine's NCPU). On macOS and Windows every
// container runs inside that VM, so a budget above its vCPU count is a
// promise the machine cannot keep — the CPU twin of the memory clip
// (ContainerMemoryBudgetMB). An engineCPUs of zero or less means "no VM, or
// its size is unknown" and leaves the configuration as it is; a Linux host,
// where containers share the host's CPUs, is never clipped here.
//
// This is container work's budget: advertised to heads as max_cpu_cores, and
// the bound the container tasks' grants are summed against. Native and WASM
// work runs on the machine itself and is bounded by the configuration alone.
func ContainerCPUBudget(configCores, engineCPUs int) int {
	if engineCPUs <= 0 || configCores <= 0 {
		return configCores
	}
	if engineCPUs < configCores {
		return engineCPUs
	}
	return configCores
}

// Threads is the number of worker threads the grant amounts to, for the
// thread-count knobs: one per granted core, never below one.
func (g CPUGrant) Threads() int {
	if g.Cores < 1 {
		return 1
	}
	return g.Cores
}

// Env returns the environment entries (KEY=value) that tell a task its CPU
// grant: LETTUCE_CPU_LIMIT carries the cores granted, and the standard
// thread-pool knobs — OMP_NUM_THREADS, OPENBLAS_NUM_THREADS, MKL_NUM_THREADS,
// NUMEXPR_MAX_THREADS, and DOCLING_NUM_THREADS for document-conversion leafs —
// carry it as a thread count, so a library that reads them sizes its
// pool to the grant rather than to every CPU it can see. Nil when no CPU limit
// is in force.
func (g CPUGrant) Env() []string {
	if g.Cores <= 0 {
		return nil
	}
	threads := strconv.Itoa(g.Threads())
	return []string{
		"LETTUCE_CPU_LIMIT=" + strconv.Itoa(g.Cores),
		"OMP_NUM_THREADS=" + threads,
		"OPENBLAS_NUM_THREADS=" + threads,
		"MKL_NUM_THREADS=" + threads,
		"NUMEXPR_MAX_THREADS=" + threads,
		"DOCLING_NUM_THREADS=" + threads,
	}
}

// staticCPUGrant returns a grant source that always hands out the whole
// budget — the grant for a unit the daemon has not given one (the audit
// runner, where one task runs at a time).
func staticCPUGrant(budgetCores int) func() CPUGrant {
	return func() CPUGrant {
		if budgetCores <= 0 {
			return CPUGrant{}
		}
		return CPUGrant{Cores: budgetCores, BudgetCores: budgetCores}
	}
}

// grantFor is the grant a unit is started with: the one the daemon gave it
// (WorkUnit.CPUGrant), or else the runtime's own fixed budget (fallback, nil
// for none).
func grantFor(wu *WorkUnit, fallback func() CPUGrant) CPUGrant {
	if wu != nil && wu.CPUGrant.Cores > 0 {
		return wu.CPUGrant
	}
	if fallback == nil {
		return CPUGrant{}
	}
	return fallback()
}

// String describes the grant for logs.
func (g CPUGrant) String() string {
	if g.Cores <= 0 {
		return "no CPU limit"
	}
	return fmt.Sprintf("%d of %d cores", g.Cores, g.BudgetCores)
}
