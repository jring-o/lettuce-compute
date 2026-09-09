package daemon

import (
	"fmt"
	"os/exec"

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
//   - CPUBudgetCores is the one figure heads are told, admission books
//     against and running tasks share: the configuration, clipped to the
//     container engine VM's vCPUs where the engine runs inside one (the CPU
//     twin of the TB-63 memory clip).
//   - cpuGrant is what a task starting now is given: budget / running tasks.
//     The runtimes read it at start; rebalanceCPUShares pushes the new split
//     to every running task through its process handle whenever a task
//     starts or finishes.
//   - bookedCPUCores is what admission books per unit: the leaf's minimum
//     core requirement (floor 1), so the equal share never drops below what a
//     leaf declared it needs, and at most budget tasks run at once whatever
//     max_concurrent_tasks says.

// CPUBudgetCores is the whole-machine CPU budget this daemon works to: the
// configured max_cpu_cores, clipped to the number of vCPUs the container
// engine's VM has where the engine runs inside one (runtime.ContainerCPUBudget).
// It is the figure advertised to heads, booked at admission and shared among
// the running tasks, so all three agree. With no container engine, or one
// that shares the host's CPUs (Linux), it is the configuration.
func (d *Daemon) CPUBudgetCores() int {
	cfgCores := 0
	if d.cfg != nil {
		cfgCores = d.cfg.ResourceLimits.MaxCPUCores
	}
	return runtime.ContainerCPUBudget(cfgCores, d.ContainerVMCPUs())
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
// configuration, is what bounds this machine's CPU budget.
func (d *Daemon) CPULimitedByVM() bool {
	if d.cfg == nil {
		return false
	}
	return d.ContainerVMCPUs() > 0 && d.CPUBudgetCores() < d.cfg.ResourceLimits.MaxCPUCores
}

// cpuGrant is what a task starting now is given: its equal share of the
// budget among the tasks running once it has started (the caller's slot is
// already active when a runtime asks), and the budget itself. Wired into
// every runtime as its grant source.
func (d *Daemon) cpuGrant() runtime.CPUGrant {
	budget := d.CPUBudgetCores()
	running := 1
	if d.slotManager != nil {
		running = d.slotManager.ActiveCount()
	}
	return runtime.CPUGrant{ShareCores: runtime.CPUShareCores(budget, running), BudgetCores: budget}
}

// currentCPUShare is the share every running task should have right now.
func (d *Daemon) currentCPUShare() float64 {
	return d.cpuGrant().ShareCores
}

// rebalanceCPUShares gives every running task the current equal share of the
// budget. Called after a slot starts and after one finishes — the two events
// that change the split — and when the budget itself changes (a config
// change, the engine VM's clip).
func (d *Daemon) rebalanceCPUShares() {
	if d.slotManager == nil {
		return
	}
	share := d.currentCPUShare()
	if share <= 0 {
		return
	}
	d.slotManager.ApplyCPUShare(share)
}

// nativeSetCPU is how a native process handle rewrites its process's CPU cap:
// through the limiter that enforced it, with the current budget so the
// affinity fallback can re-pin to a changed budget.
func (d *Daemon) nativeSetCPU(pid int, shareCores float64) error {
	if d.limiter == nil {
		return nil
	}
	return d.limiter.SetCPU(pid, runtime.CPUGrant{ShareCores: shareCores, BudgetCores: d.CPUBudgetCores()})
}

// bookedCPUCores is the number of cores admission books for a unit: its
// leaf's minimum core requirement (resource_requirements.min_cpu_cores, the
// figure the head's dispatch gate compares against max_cpu_cores), never
// below one, and never above the budget (a VM clip can put the budget under
// a requirement the head already admitted; such a unit runs alone). Booking
// the minimum keeps every running task's equal share at or above what its
// leaf declared it needs.
func (d *Daemon) bookedCPUCores(wu *runtime.WorkUnit) int {
	cores := 1
	if wu != nil {
		if minCores := d.leafMinCPUCores(wu.LeafID); minCores > cores {
			cores = minCores
		}
	}
	if budget := d.CPUBudgetCores(); budget > 0 && cores > budget {
		cores = budget
	}
	return cores
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
// memory ceiling is BookedMemMB(declared, budget) — the same clamped number
// admission books — so native enforcement matches admission instead of
// always capping at the whole configured budget (BG-16); the CPU is the
// task's share of the budget at the moment it starts (TB-75), the same grant
// the task is told through its environment. Both are read when the task
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
// task starting now: its declared memory clamped to the live memory budget
// (the figure admission booked it at), and the CPU grant it was given.
func (d *Daemon) taskLimits(declaredMemMB int, cpu runtime.CPUGrant) *resource.TaskLimits {
	return &resource.TaskLimits{
		MaxMemoryMB: runtime.BookedMemMB(declaredMemMB, d.MemoryBudgetMB()),
		CPU:         cpu,
	}
}

// wireRuntimeMemory gives one runtime the daemon's live memory budget as the
// ceiling it clamps a unit's declaration to at start (TB-79), the memory twin
// of wireRuntimeCPU. Also called for a container runtime that appears after
// start (registerContainerRuntime). The native runtime's ceiling travels
// through the limiter closure (taskLimits) instead.
func (d *Daemon) wireRuntimeMemory(rt runtime.Runtime) {
	switch r := rt.(type) {
	case *runtime.ContainerRuntime:
		if r != nil {
			r.SetMemoryCeilingSource(d.MemoryBudgetMB)
		}
	case *runtime.WasmRuntime:
		if r != nil {
			r.SetMemoryCeilingSource(d.MemoryBudgetMB)
		}
	}
}

// wireRuntimeCPU gives one runtime the daemon's live CPU grant as the source
// it consults when a task starts. Also called for a container runtime that
// appears after start (registerContainerRuntime).
func (d *Daemon) wireRuntimeCPU(rt runtime.Runtime) {
	switch r := rt.(type) {
	case *runtime.ContainerRuntime:
		if r != nil {
			r.SetCPUGrantSource(d.cpuGrant)
		}
	case *runtime.NativeRuntime:
		if r != nil {
			r.SetCPUGrantSource(d.cpuGrant)
		}
	case *runtime.WasmRuntime:
		if r != nil {
			r.SetCPUGrantSource(d.cpuGrant)
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
// volunteer-facing notice, and re-splits the (possibly smaller) budget among
// the tasks already running. Called once a container runtime is registered —
// at start and on a late detection, before the heads are re-told — beside
// its memory twin (applyContainerMemoryBudget).
func (d *Daemon) applyContainerCPUBudget() {
	if d.CPULimitedByVM() {
		d.setAdvertisedCPUCores(d.CPUBudgetCores())
	}
	d.refreshContainerCPUNotice()
	d.rebalanceCPUShares()
}

// refreshContainerCPUNotice keeps the "container_cpu_clipped" notice in step
// with the facts: raised with one WARN whenever the configured CPU limit
// exceeds the vCPUs of the container engine's VM, naming both figures and the
// remedy; resolved when the limit fits.
func (d *Daemon) refreshContainerCPUNotice() {
	if !d.CPULimitedByVM() {
		d.notices.Resolve("container_cpu_clipped", "", "")
		return
	}
	cfgCores := d.cfg.ResourceLimits.MaxCPUCores
	budget := d.CPUBudgetCores()
	vmCPUs := d.ContainerVMCPUs()
	d.logger.Warn("container engine's VM has fewer CPUs than the CPU limit; work on this machine is limited to the VM's CPUs — heads are told the smaller figure and only send leafs that fit it",
		"engine_vm_cpus", vmCPUs, "cpu_budget_cores", budget, "max_cpu_cores", cfgCores,
		"remedy", "give the VM more CPUs (Podman: `podman machine stop`, `podman machine set --cpus <n>`, `podman machine start`; Podman Desktop or Docker Desktop: Settings → Resources) — or lower the CPU limit to the VM's count so the two agree")
	d.notices.Notify(NoticeWarn, "container_cpu_clipped",
		fmt.Sprintf("Work on this machine is limited to %d CPU cores: the container engine runs inside a virtual machine with %d CPUs. Your CPU limit of %d cores is not what heads are told — they see %d and only send leafs that fit. To use more cores, give the machine more CPUs (Podman: `podman machine set --cpus`; Podman Desktop or Docker Desktop: Settings → Resources) and restart Lettuce.",
			budget, vmCPUs, cfgCores, budget),
		"", "")
}
