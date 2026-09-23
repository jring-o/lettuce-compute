package daemon

import (
	"fmt"
	"os/exec"
	"strings"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The CPU budget (TB-75): resource_limits.max_cpu_cores is the most CPU
// Lettuce may use on the whole machine, shared equally by the running tasks.
//
// It used to be a per-task figure — every container and native process was
// given the full max_cpu_cores as its own quota and nothing booked cores at
// admission — so the real ceiling was max_cpu_cores × max_concurrent_tasks,
// while the Settings page showed the number as a share of the machine beside
// the memory budget, which admission has always summed bookings against. A
// tester who allowed 2 cores and 2 tasks filled all four vCPUs of his Podman
// machine. This file is the daemon's side of the fix:
//
//   - HostCPUBudgetCores is the configuration: the most CPU all running tasks
//     together may use, and the budget a task that runs directly on the
//     machine (native, WASM) draws on. ContainerCPUBudgetCores is that clipped
//     to the container engine VM's vCPUs where the engine runs inside one (the
//     CPU twin of the TB-63 memory clip): container tasks all run inside the
//     VM and can never together use more. Heads are told both (TB-85), and
//     admission books a unit against each that applies to it. The clip used to
//     be the one figure for everything, so a Mac whose VM had fewer CPUs than
//     the limit held its native work to the VM's size too.
//   - cpuGrantFor is what a task starting now is given: its runtime's share of
//     the split (runtime.SplitCPUBudget) — an equal share of the host budget,
//     the container tasks together capped at the container budget. The
//     runtimes read it at start; rebalanceCPUShares pushes the new split to
//     every running task through its process handle whenever a task starts or
//     finishes.
//   - bookedCPUCores is what admission books per unit: the leaf's minimum
//     core requirement (floor 1), so the equal share never drops below what a
//     leaf declared it needs, and at most budget tasks run at once whatever
//     max_concurrent_tasks says.

// HostCPUBudgetCores is the whole-machine CPU budget this daemon works to:
// the configured max_cpu_cores. Every running task together stays within it,
// and a task that runs directly on the machine (native, WASM) is measured
// against it alone (TB-85).
func (d *Daemon) HostCPUBudgetCores() int {
	if d.cfg == nil {
		return 0
	}
	return d.cfg.ResourceLimits.MaxCPUCores
}

// ContainerCPUBudgetCores is the CPU budget container work gets: the
// configured max_cpu_cores, clipped to the number of vCPUs the container
// engine's VM has where the engine runs inside one (runtime.ContainerCPUBudget).
// It is the max_cpu_cores heads are told — every runtime's work fits it — and
// the budget container tasks are booked against and share. With no container
// engine, or one that shares the host's CPUs (Linux), it is the configuration.
func (d *Daemon) ContainerCPUBudgetCores() int {
	return runtime.ContainerCPUBudget(d.HostCPUBudgetCores(), d.ContainerVMCPUs())
}

// cpuBudgetFor is the CPU budget a unit is measured against: the container
// budget for a container unit, the host budget for any other (TB-85).
func (d *Daemon) cpuBudgetFor(wu *runtime.WorkUnit) int {
	if isContainerUnit(wu) {
		return d.ContainerCPUBudgetCores()
	}
	return d.HostCPUBudgetCores()
}

// ContainerVMCPUs reports the number of vCPUs of the VM the registered
// container engine runs inside (0 when there is no container runtime, the
// engine shares the host's CPUs, or the count is unknown).
func (d *Daemon) ContainerVMCPUs() int {
	if d.runtimeRegistry == nil || d.runtimeRegistry.GetRuntime("container") == nil {
		return 0
	}
	_, cpus := d.containerFactory.ContainerCPUs()
	return cpus
}

// CPULimitedByVM reports whether the container engine's VM, not the
// configuration, is what bounds container work's CPU budget.
func (d *Daemon) CPULimitedByVM() bool {
	if d.cfg == nil {
		return false
	}
	return d.ContainerVMCPUs() > 0 && d.ContainerCPUBudgetCores() < d.cfg.ResourceLimits.MaxCPUCores
}

// currentCPUShares is the split of the CPU budgets among the tasks running
// right now (runtime.SplitCPUBudget, TB-85). Wired into the slot manager as
// its share source.
func (d *Daemon) currentCPUShares() runtime.CPUShares {
	hostTasks, containerTasks := d.activeTaskCounts()
	return runtime.SplitCPUBudget(d.HostCPUBudgetCores(), d.ContainerCPUBudgetCores(), hostTasks, containerTasks)
}

// activeTaskCounts is the number of running tasks of each kind: those that
// run directly on the machine and container ones.
func (d *Daemon) activeTaskCounts() (host, container int) {
	if d.slotManager == nil {
		return 0, 0
	}
	return d.slotManager.ActiveCountsByKind()
}

// cpuGrantFor is what a task starting now is given: its runtime's share of
// the budgets among the tasks running once it has started (the caller's slot
// is already active when a runtime asks; a count of zero for its kind is read
// as one, the share a lone task would get), and the budget the share is part
// of — the container budget for a container task, the host budget for a task
// that runs directly on the machine. Wired into every runtime as its grant
// source (wireRuntimeCPU).
func (d *Daemon) cpuGrantFor(container bool) runtime.CPUGrant {
	hostTasks, containerTasks := d.activeTaskCounts()
	if container && containerTasks < 1 {
		containerTasks = 1
	}
	if !container && hostTasks < 1 {
		hostTasks = 1
	}
	shares := runtime.SplitCPUBudget(d.HostCPUBudgetCores(), d.ContainerCPUBudgetCores(), hostTasks, containerTasks)
	if container {
		return runtime.CPUGrant{ShareCores: shares.Container, BudgetCores: d.ContainerCPUBudgetCores()}
	}
	return runtime.CPUGrant{ShareCores: shares.Host, BudgetCores: d.HostCPUBudgetCores()}
}

// rebalanceCPUShares gives every running task its current share of the
// budgets. Called after a slot starts and after one finishes — the two events
// that change the split — and when a budget itself changes (a config change,
// the engine VM's clip).
func (d *Daemon) rebalanceCPUShares() {
	if d.slotManager == nil {
		return
	}
	shares := d.currentCPUShares()
	if shares.Host <= 0 && shares.Container <= 0 {
		return
	}
	d.slotManager.ApplyCPUShares(shares)
}

// nativeSetCPU is how a native process handle rewrites its process's CPU cap:
// through the limiter that enforced it, with the host budget so the affinity
// fallback can re-pin to a changed budget.
func (d *Daemon) nativeSetCPU(pid int, shareCores float64) error {
	if d.limiter == nil {
		return nil
	}
	return d.limiter.SetCPU(pid, runtime.CPUGrant{ShareCores: shareCores, BudgetCores: d.HostCPUBudgetCores()})
}

// bookedCPUCores is the number of cores admission books for a unit: its
// leaf's minimum core requirement (resource_requirements.min_cpu_cores, the
// figure the head's dispatch gate compares against the budget of the leaf's
// runtime), never below one, and never above that budget (cpuBudgetFor — a
// VM clip can put the container budget under a requirement the head already
// admitted; such a unit runs alone). Booking the minimum keeps every running
// task's equal share at or above what its leaf declared it needs.
func (d *Daemon) bookedCPUCores(wu *runtime.WorkUnit) int {
	cores := 1
	if wu != nil {
		if minCores := d.leafMinCPUCores(wu.LeafID); minCores > cores {
			cores = minCores
		}
	}
	if budget := d.cpuBudgetFor(wu); budget > 0 && cores > budget {
		cores = budget
	}
	return cores
}

// bookedContainerCPUCores is bookedCPUCores for a container unit and 0 for
// any other — the bookings the container budget is summed from (TB-85).
func (d *Daemon) bookedContainerCPUCores(wu *runtime.WorkUnit) int {
	if !isContainerUnit(wu) {
		return 0
	}
	return d.bookedCPUCores(wu)
}

// leafMinCPUCores is the leaf's declared minimum core requirement from the
// leaf cache, 0 when unknown (no cache, a leaf the cache does not hold, or a
// head too old to send requirements).
func (d *Daemon) leafMinCPUCores(leafID string) int {
	if d.leafCache == nil || leafID == "" {
		return 0
	}
	for _, leafs := range d.leafCache.AllLeafs() {
		for _, l := range leafs {
			if l.ID == leafID && l.ResourceRequirements != nil {
				return int(l.ResourceRequirements.MinCPUCores)
			}
		}
	}
	return 0
}

// wireRuntimeLimits attaches the resource limiter, the process group, the
// live CPU grant and the live memory ceiling to the registered runtimes. The
// limiter is enforced against a PER-UNIT set of limits (taskLimits): the
// memory ceiling is BookedMemMB(declared, host budget) — the same clamped
// number admission books a native unit at — so native enforcement matches
// admission instead of always capping at the whole configured budget (BG-16);
// the CPU is the task's share of the budget at the moment it starts (TB-75),
// the same grant the task is told through its environment. Both are read when the task
// starts, so a limit changed while the daemon runs bounds the next task
// (TB-79); the closure used to hold the start-up configuration's struct.
func (d *Daemon) wireRuntimeLimits(pg ProcessGroup, limiter resource.Limiter) {
	if d.runtimeRegistry == nil {
		return
	}
	for _, rt := range d.runtimeRegistry.runtimes {
		d.wireRuntimeCPU(rt)
		d.wireRuntimeMemory(rt)
		nr, ok := rt.(*runtime.NativeRuntime)
		if !ok {
			continue
		}
		nr.SetCommandModifier(func(cmd *exec.Cmd, declaredMemMB int, cpu runtime.CPUGrant) error {
			if pg != nil {
				pg.ConfigureCommand(cmd)
			}
			return limiter.Apply(cmd, d.taskLimits(declaredMemMB, cpu))
		})
		nr.SetProcessNotifier(func(pid int, declaredMemMB int, cpu runtime.CPUGrant) (func(), error) {
			if pg != nil {
				if err := pg.Add(pid); err != nil {
					d.logger.Warn("failed to add process to group", "pid", pid, "error", err)
				}
			}
			return limiter.Enforce(pid, d.taskLimits(declaredMemMB, cpu))
		})
	}
}

// taskLimits is the per-unit limit set the native limiter enforces on a
// task starting now: its declared memory clamped to the live host memory
// budget (the figure admission booked it at — a native unit runs on the
// machine, not inside the container engine's VM, TB-85), and the CPU grant it
// was given.
func (d *Daemon) taskLimits(declaredMemMB int, cpu runtime.CPUGrant) *resource.TaskLimits {
	return &resource.TaskLimits{
		MaxMemoryMB: runtime.BookedMemMB(declaredMemMB, d.HostMemoryBudgetMB()),
		CPU:         cpu,
	}
}

// wireRuntimeMemory gives one runtime the daemon's live memory budget for its
// kind of work as the ceiling it clamps a unit's declaration to at start
// (TB-79), the memory twin of wireRuntimeCPU: the container budget for the
// container runtime, the host budget for WASM, which runs inside the daemon
// process on the machine itself (TB-85). Also called for a container runtime
// that appears after start (registerContainerRuntime). The native runtime's
// ceiling travels through the limiter closure (taskLimits) instead.
func (d *Daemon) wireRuntimeMemory(rt runtime.Runtime) {
	switch r := rt.(type) {
	case *runtime.ContainerRuntime:
		if r != nil {
			r.SetMemoryCeilingSource(d.ContainerMemoryBudgetMB)
		}
	case *runtime.WasmRuntime:
		if r != nil {
			r.SetMemoryCeilingSource(d.HostMemoryBudgetMB)
		}
	}
}

// wireRuntimeCPU gives one runtime the daemon's live CPU grant for its kind
// of work (cpuGrantFor) as the source it consults when a task starts. Also
// called for a container runtime that appears after start
// (registerContainerRuntime).
func (d *Daemon) wireRuntimeCPU(rt runtime.Runtime) {
	containerGrant := func() runtime.CPUGrant { return d.cpuGrantFor(true) }
	hostGrant := func() runtime.CPUGrant { return d.cpuGrantFor(false) }
	switch r := rt.(type) {
	case *runtime.ContainerRuntime:
		if r != nil {
			r.SetCPUGrantSource(containerGrant)
		}
	case *runtime.NativeRuntime:
		if r != nil {
			r.SetCPUGrantSource(hostGrant)
		}
	case *runtime.WasmRuntime:
		if r != nil {
			r.SetCPUGrantSource(hostGrant)
		}
	}
}

// setAdvertisedCPUCores replaces the advertised hardware with a copy whose
// CPU budget is cores (the late-detection clip; see updateAdvertisedHardware).
func (d *Daemon) setAdvertisedCPUCores(cores int) {
	d.updateAdvertisedHardware(func(hw *lettucev1.HardwareCapabilities) { hw.MaxCpuCores = int32(cores) })
}

// applyContainerCPUBudget lowers the advertised CPU budget to the container
// engine VM's vCPU count when that is below the configuration, raises the
// volunteer-facing notice, and re-splits the (possibly smaller) container
// budget among the tasks already running. Called once a container runtime is
// registered — at start and on a late detection, before the heads are
// re-told — beside its memory twin (applyContainerMemoryBudget).
func (d *Daemon) applyContainerCPUBudget() {
	if d.CPULimitedByVM() {
		d.setAdvertisedCPUCores(d.ContainerCPUBudgetCores())
	}
	d.refreshContainerCPUNotice()
	d.rebalanceCPUShares()
}

// refreshContainerCPUNotice keeps the "container_cpu_clipped" notice in step
// with the facts. The VM clip is worth the volunteer's attention only when it
// costs something (TB-92): when an enabled container leaf needs more cores
// than the container engine's VM has, but no more than the CPU limit — the VM,
// not the limit, is then what keeps heads from sending it. The notice and one
// WARN name the leaf, both figures and the remedy; the notice is resolved once
// no enabled leaf is held back (the VM was enlarged, or the limit or the
// enabled leafs changed). A clip that holds nothing back is logged once at
// Info: native and WebAssembly work still get the whole limit (TB-85), so a VM
// smaller than the limit is an ordinary, often deliberate, setup.
func (d *Daemon) refreshContainerCPUNotice() {
	const code = "container_cpu_clipped"
	if !d.CPULimitedByVM() {
		d.vmNotices.forget(code)
		d.notices.Resolve(code, "", "")
		return
	}
	cfgCores := d.cfg.ResourceLimits.MaxCPUCores
	budget := d.ContainerCPUBudgetCores()
	vmCPUs := d.ContainerVMCPUs()
	held := d.vmHeldContainerLeafs(func(l CachedLeafInfo) int {
		if l.ResourceRequirements == nil {
			return 0
		}
		return int(l.ResourceRequirements.MinCPUCores)
	}, budget, cfgCores, "cores")
	if !d.vmNotices.changed(code, fmt.Sprint(budget, vmCPUs, cfgCores, held)) {
		return
	}
	if len(held) == 0 {
		d.notices.Resolve(code, "", "")
		d.logger.Info("container work is limited to the CPUs of the container engine's VM, and every enabled container leaf fits them; native and WebAssembly work use the whole CPU limit",
			"engine_vm_cpus", vmCPUs, "container_cpu_budget_cores", budget, "max_cpu_cores", cfgCores)
		return
	}
	d.logger.Warn("container engine's VM has fewer CPUs than an enabled container leaf needs; heads do not send this machine that leaf until the VM has more",
		"engine_vm_cpus", vmCPUs, "container_cpu_budget_cores", budget, "max_cpu_cores", cfgCores,
		"held_back_leafs", strings.Join(held, ", "),
		"remedy", "give the VM more CPUs (Podman: `podman machine stop`, `podman machine set --cpus <n>`, `podman machine start`; Podman Desktop or Docker Desktop: Settings → Resources)")
	d.notices.Notify(NoticeWarn, code,
		fmt.Sprintf("Container work on this machine is limited to %d CPU cores: the container engine runs inside a virtual machine with %d CPUs. %s, so heads will not send %s here. Native and WebAssembly work still use your full %d cores. To run %s, give the machine more CPUs (Podman: `podman machine set --cpus`; Podman Desktop or Docker Desktop: Settings → Resources) and restart Lettuce.",
			budget, vmCPUs, heldPhrase(held), heldObject(held), cfgCores, heldObject(held)),
		"", "")
}
