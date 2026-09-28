package daemon

import (
	"fmt"
	"math"
	"os/exec"
	"strings"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The CPU budget and the grant each task is given.
//
// resource_limits.max_cpu_cores is the most CPU Lettuce may use on the whole
// machine (TB-75). It used to be shared EQUALLY by the running tasks and
// re-split live on every start and finish, so a GREP unit that could use three
// cores got the same share as the one-core Beyblade beside it and ran past its
// deadline, while a volunteer who wanted N one-core tasks had to raise both
// the core total and max_concurrent_tasks. Now:
//
//   - Each leaf declares a core RANGE: min_cpu_cores, the head's dispatch gate
//     and the fewest a unit runs on, and max_cpu_cores, the most it can use
//     (declaredCoreRange; a head too old to send the max, or a leaf that
//     declares none, is min–min). A volunteer's cores override for the leaf
//     narrows it to one figure inside it (leafCoreRange, leaf_override.go).
//   - A task starting is GRANTED whole cores between the two (grantCPU): its
//     minimum, plus as many of the budget's free cores as are left once the
//     units waiting in the buffer that could start beside it have their
//     minimums set aside, up to its maximum. It keeps the grant until it
//     finishes; the runtime holds it there and tells it the figure. The grants
//     of the running tasks together never exceed the budget.
//   - Admission books a running task at its grant and a unit about to start at
//     its minimum (bookedCPUCores), so how many run at once follows from the
//     budget and the grants; max_running_tasks is only an optional extra cap.
//   - A unit waiting only for cores is not starved by narrower units behind
//     it: none may start on cores it is waiting for (coreWait).
//
// HostCPUBudgetCores is the configuration, the budget a task that runs
// directly on the machine (native, WASM) draws on; ContainerCPUBudgetCores is
// that clipped to the container engine VM's vCPUs where the engine runs inside
// one, which bounds container tasks together (TB-85).

// HostCPUBudgetCores is the whole-machine CPU budget this daemon works to:
// the configured max_cpu_cores. Every running task's grant together stays
// within it, and a task that runs directly on the machine (native, WASM) is
// measured against it alone (TB-85).
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
// the budget container tasks' grants are summed against. With no container
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

// containerCPUBudgetBinds reports whether the container budget is tighter than
// the host budget, so container tasks are summed against it too.
func (d *Daemon) containerCPUBudgetBinds() bool {
	host, vm := d.HostCPUBudgetCores(), d.ContainerCPUBudgetCores()
	return vm > 0 && (host <= 0 || vm < host)
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

// declaredCoreRange is the leaf's declared core range per unit from the leaf
// cache: min_cpu_cores (at least 1) and max_cpu_cores (at least the min). A
// leaf the cache does not hold, or a head too old to send requirements, is
// 1–1; a head too old to send the max is min–min.
func (d *Daemon) declaredCoreRange(leafID string) (minCores, maxCores int) {
	l, _ := d.cachedLeaf(leafID)
	return coreRangeOf(l)
}

// leafCoreRange is the range a task of the leaf is granted from on this
// machine: its declared range, or the volunteer's cores override for the leaf
// brought inside it (applyCoresOverride).
func (d *Daemon) leafCoreRange(leafID string) (minCores, maxCores int) {
	minCores, maxCores = d.declaredCoreRange(leafID)
	return applyCoresOverride(minCores, maxCores, d.leafOverride(leafID).cores)
}

// effectiveCoreRange is leafCoreRange for a leaf already in hand.
func (d *Daemon) effectiveCoreRange(l CachedLeafInfo) (minCores, maxCores int) {
	minCores, maxCores = coreRangeOf(l)
	return applyCoresOverride(minCores, maxCores, d.leafOverride(l.ID).cores)
}

// coreRangeOf is leafCoreRange for a leaf already in hand.
func coreRangeOf(l CachedLeafInfo) (minCores, maxCores int) {
	minCores, maxCores = 1, 0
	if l.ResourceRequirements != nil {
		if n := int(l.ResourceRequirements.MinCPUCores); n > minCores {
			minCores = n
		}
		maxCores = int(l.ResourceRequirements.MaxCPUCores)
	}
	if maxCores < minCores {
		maxCores = minCores
	}
	return minCores, maxCores
}

// cachedLeaf finds a leaf in the leaf cache by id, across every head.
func (d *Daemon) cachedLeaf(leafID string) (CachedLeafInfo, bool) {
	if d.leafCache == nil || leafID == "" {
		return CachedLeafInfo{}, false
	}
	for _, leafs := range d.leafCache.AllLeafs() {
		for _, l := range leafs {
			if l.ID == leafID {
				return l, true
			}
		}
	}
	return CachedLeafInfo{}, false
}

// unitCoreRange is the core range a unit can be granted: its leaf's range,
// clipped to the budget of the unit's runtime (cpuBudgetFor — a VM clip can
// put the container budget under a minimum the head already admitted; such a
// unit runs on the whole budget, alone).
func (d *Daemon) unitCoreRange(wu *runtime.WorkUnit) (minCores, maxCores int) {
	minCores, maxCores = 1, 1
	if wu != nil {
		minCores, maxCores = d.leafCoreRange(wu.LeafID)
	}
	if budget := d.cpuBudgetFor(wu); budget > 0 {
		if minCores > budget {
			minCores = budget
		}
		if maxCores > budget {
			maxCores = budget
		}
	}
	return minCores, maxCores
}

// bookedCPUCores is the number of cores a unit holds against the CPU budget:
// a running task's grant, and for a unit not started yet the fewest it would
// be granted (unitCoreRange's minimum). Admission sums the running tasks'
// grants and adds the candidate's minimum, so a unit starts only when its
// minimum fits beside what is already granted.
func (d *Daemon) bookedCPUCores(wu *runtime.WorkUnit) int {
	if wu != nil && wu.CPUGrant.Cores > 0 {
		return wu.CPUGrant.Cores
	}
	minCores, _ := d.unitCoreRange(wu)
	return minCores
}

// bookedContainerCPUCores is bookedCPUCores for a container unit and 0 for
// any other — the bookings the container budget is summed from (TB-85).
func (d *Daemon) bookedContainerCPUCores(wu *runtime.WorkUnit) int {
	if !isContainerUnit(wu) {
		return 0
	}
	return d.bookedCPUCores(wu)
}

// grantCPU decides the CPU a unit about to start is given: its minimum, plus
// the cores the budget still has free once the running tasks' grants, its own
// minimum and the minimums of the units in waiting that could start beside it
// are counted, up to its maximum. waiting is the buffer in order (the unit
// itself, if present, is skipped); a waiting unit is counted only if it fits
// what is left of the CPU, memory and GPU budgets and the running-task cap
// (budgetLedger), so cores are never held back for a unit that could not start
// anyway. A unit that finds less than its minimum free (a task resumed after
// the budget was lowered) is given its minimum. Zero when no CPU budget is set.
//
// This is the one place a grant is decided: the slot filler and the task
// resumer call it for the unit they start, and every surface that shows a
// grant shows the one recorded on the unit.
func (d *Daemon) grantCPU(wu *runtime.WorkUnit, waiting []*runtime.WorkUnit) runtime.CPUGrant {
	return d.grantFrom(d.runningLedger(), wu, waiting)
}

// grantFrom is grantCPU against a given ledger: what the budgets have free
// before wu starts. The slot filler passes the running tasks' ledger; the
// preview of what would run together (run_preview.go) passes the ledger of
// the tasks it has placed so far, so both decide a grant the same way.
func (d *Daemon) grantFrom(ledger budgetLedger, wu *runtime.WorkUnit, waiting []*runtime.WorkUnit) runtime.CPUGrant {
	budget := d.cpuBudgetFor(wu)
	if budget <= 0 {
		return runtime.CPUGrant{}
	}
	minCores, maxCores := d.unitCoreRange(wu)
	ledger = ledger.clone()
	ledger.take(d, wu, minCores)
	for _, w := range waiting {
		if ledger.coresFreeFor(wu) <= 0 {
			break // nothing left to hold back or to add
		}
		if w == nil || (wu != nil && w.ID == wu.ID) || !ledger.fits(d, w) {
			continue
		}
		ledger.take(d, w, d.bookedCPUCores(w))
	}
	extra := ledger.coresFreeFor(wu)
	if extra < 0 {
		extra = 0
	}
	cores := minCores + extra
	if cores > maxCores {
		cores = maxCores
	}
	return runtime.CPUGrant{Cores: cores, BudgetCores: budget}
}

// adoptedCPUGrant is the grant a task adopted from the previous session (a
// frozen process, a paused container) is booked at: the cores it was granted
// when it started, which it is still held to, or its minimum when the state
// file came from a client that did not record them.
func (d *Daemon) adoptedCPUGrant(wu *runtime.WorkUnit, cores int) runtime.CPUGrant {
	budget := d.cpuBudgetFor(wu)
	if budget <= 0 {
		return runtime.CPUGrant{}
	}
	if cores <= 0 {
		cores, _ = d.unitCoreRange(wu)
	}
	return runtime.CPUGrant{Cores: cores, BudgetCores: budget}
}

// budgetLedger is what the configured budgets have free: CPU cores and memory
// on the machine and, where they are tighter, inside the container engine's
// VM; physical GPUs; room under max_running_tasks; and how many of each leaf
// run, against the volunteer's max_running for it. A figure goes negative
// when what runs exceeds a budget lowered since it started; noBound stands
// for a budget that is not set. Built from the running tasks (runningLedger)
// and charged with each unit taken, it lets grantCPU ask of a waiting unit the
// budget questions admission asks (canAccommodateWU).
type budgetLedger struct {
	hostCores, containerCores int
	hostMemMB, containerMemMB int
	gpus, tasks               int
	leafRunning               map[string]int
}

// noBound is a budgetLedger figure for a budget that is not set: larger than
// anything is ever charged against it.
const noBound = math.MaxInt32

// clone is a copy of the ledger whose charges do not reach the original (the
// per-leaf counts are a map).
func (l budgetLedger) clone() budgetLedger {
	c := l
	c.leafRunning = make(map[string]int, len(l.leafRunning))
	for k, v := range l.leafRunning {
		c.leafRunning[k] = v
	}
	return c
}

// runningLedger is the budgets less what the running tasks hold; unbounded
// while the daemon has no slot manager (not running).
func (d *Daemon) runningLedger() budgetLedger {
	sm := d.slotManager // one read: the daemon clears it when it stops
	if sm == nil {
		return unboundedLedger()
	}
	return d.ledgerFor(sm)
}

// emptyLedger is the budgets with nothing running: where the preview of
// what would run together starts.
func (d *Daemon) emptyLedger() budgetLedger {
	return d.ledgerFor(&SlotManager{})
}

// unboundedLedger is a ledger with no budget set.
func unboundedLedger() budgetLedger {
	return budgetLedger{hostCores: noBound, containerCores: noBound, hostMemMB: noBound, containerMemMB: noBound, gpus: noBound, tasks: noBound,
		leafRunning: make(map[string]int)}
}

// ledgerFor is the budgets less what sm's running tasks hold.
func (d *Daemon) ledgerFor(sm *SlotManager) budgetLedger {
	l := unboundedLedger()
	for _, wu := range sm.ActiveWorkUnits() {
		if wu != nil {
			l.leafRunning[wu.LeafID]++
		}
	}
	if host := d.HostCPUBudgetCores(); host > 0 {
		l.hostCores = host - sm.TotalActiveCPUCores(d.bookedCPUCores)
	}
	if d.containerCPUBudgetBinds() {
		l.containerCores = d.ContainerCPUBudgetCores() - sm.TotalActiveCPUCores(d.bookedContainerCPUCores)
	}
	hostMemMB := d.HostMemoryBudgetMB()
	if hostMemMB > 0 {
		l.hostMemMB = hostMemMB - sm.TotalActiveMemoryMB(d.bookedMemMB)
	}
	if vmMemMB := d.ContainerMemoryBudgetMB(); vmMemMB > 0 && (hostMemMB <= 0 || vmMemMB < hostMemMB) {
		l.containerMemMB = vmMemMB - sm.TotalActiveMemoryMB(d.bookedContainerMemMB)
	}
	if n := len(d.advertisedHardware().GetGpus()); n > 0 {
		l.gpus = n - sm.ActiveGPUCount()
	}
	if limit := d.maxRunningTasks(); limit > 0 {
		l.tasks = limit - sm.ActiveCount()
	}
	return l
}

// fits reports whether wu, at its minimum cores and booked memory, fits what
// the ledger has left.
func (l budgetLedger) fits(d *Daemon, wu *runtime.WorkUnit) bool {
	if wu == nil {
		return false
	}
	cores, memMB := d.bookedCPUCores(wu), d.bookedMemMB(wu)
	container := isContainerUnit(wu)
	switch {
	case cores > l.hostCores,
		container && cores > l.containerCores,
		memMB > l.hostMemMB,
		container && memMB > l.containerMemMB,
		wu.ExecutionSpec.GPURequired && l.gpus <= 0,
		l.tasks <= 0:
		return false
	}
	if limit := d.leafMaxRunning(wu.LeafID); limit > 0 && l.leafRunning[wu.LeafID] >= limit {
		return false
	}
	return true
}

// take charges the ledger with wu running on cores.
func (l *budgetLedger) take(d *Daemon, wu *runtime.WorkUnit, cores int) {
	memMB := d.bookedMemMB(wu)
	l.hostCores -= cores
	l.hostMemMB -= memMB
	if isContainerUnit(wu) {
		l.containerCores -= cores
		l.containerMemMB -= memMB
	}
	if wu != nil && wu.ExecutionSpec.GPURequired {
		l.gpus--
	}
	l.tasks--
	if wu != nil && l.leafRunning != nil {
		l.leafRunning[wu.LeafID]++
	}
}

// coresFreeFor is how many more cores the ledger could give wu: the host
// cores it has left, and for a container unit no more than the container
// budget has left. Negative when a budget is overcommitted.
func (l budgetLedger) coresFreeFor(wu *runtime.WorkUnit) int {
	free := l.hostCores
	if isContainerUnit(wu) && l.containerCores < free {
		free = l.containerCores
	}
	return free
}

// The two admission refusals that mean "not enough free cores", by kind
// (refusalKind): the whole-machine budget and the container engine VM's.
const (
	refusalHostCPU      = "configured CPU budget"
	refusalContainerCPU = "container CPU budget"
)

// coreWait is a buffered unit refused only for cores: every other admission
// check passed (the CPU checks come last), so it starts as soon as enough
// cores are free. pool names the budget it is short of (one of the two
// refusal kinds above); need is its minimum.
type coreWait struct {
	pool string
	need int
}

// coreWaitFor reads a unit's admission refusal: a coreWait when the refusal
// was for cores, else false.
func (d *Daemon) coreWaitFor(wu *runtime.WorkUnit, reason string) (coreWait, bool) {
	switch kind := refusalKind(reason); kind {
	case refusalHostCPU, refusalContainerCPU:
		return coreWait{pool: kind, need: d.bookedCPUCores(wu)}, true
	}
	return coreWait{}, false
}

// holdsBack reports whether starting candidate now would take cores the
// waiting unit needs. The waiting unit is already short of cores in
// its pool, so a candidate drawing on that pool puts its start further off
// for as long as the candidate runs — a stream of narrow units could keep a
// wide one waiting indefinitely, since every narrow unit fits the moment one
// core frees. The one harmless case is a task on the machine itself behind a
// container unit short of the VM's cores: it does not draw on the VM, and is
// allowed when the machine keeps enough cores for the container unit.
func (w coreWait) holdsBack(d *Daemon, candidate *runtime.WorkUnit) bool {
	if w.pool == refusalContainerCPU && !isContainerUnit(candidate) {
		return d.runningLedger().hostCores-d.bookedCPUCores(candidate) < w.need
	}
	return true
}

// wireRuntimeLimits attaches the resource limiter, the process group and the
// live memory ceiling to the registered runtimes. The limiter is enforced
// against a PER-UNIT set of limits (taskLimits): the memory ceiling is
// BookedMemMB(declared, host budget) — the same clamped number admission books
// a native unit at — so native enforcement matches admission instead of always
// capping at the whole configured budget (BG-16); the CPU is the grant the
// daemon gave the unit, the same one the task is told through its
// environment. Both are read when the task starts, so a limit changed while
// the daemon runs bounds the next task (TB-79).
func (d *Daemon) wireRuntimeLimits(pg ProcessGroup, limiter resource.Limiter) {
	if d.runtimeRegistry == nil {
		return
	}
	for _, rt := range d.runtimeRegistry.runtimes {
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
// (TB-79): the container budget for the container runtime, the host budget for
// WASM, which runs inside the daemon process on the machine itself (TB-85).
// Also called for a container runtime that appears after start
// (registerContainerRuntime). The native runtime's ceiling travels through the
// limiter closure (taskLimits) instead.
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

// setAdvertisedCPUCores replaces the advertised hardware with a copy whose
// CPU budget is cores (the late-detection clip; see updateAdvertisedHardware).
func (d *Daemon) setAdvertisedCPUCores(cores int) {
	d.updateAdvertisedHardware(func(hw *lettucev1.HardwareCapabilities) { hw.MaxCpuCores = int32(cores) })
}

// applyContainerCPUBudget lowers the advertised CPU budget to the container
// engine VM's vCPU count when that is below the configuration and raises the
// volunteer-facing notice. Called once a container runtime is registered — at
// start and on a late detection, before the heads are re-told — beside its
// memory twin (applyContainerMemoryBudget). Running tasks keep their grants;
// the clip bounds the grants of tasks started afterwards.
func (d *Daemon) applyContainerCPUBudget() {
	if d.CPULimitedByVM() {
		d.setAdvertisedCPUCores(d.ContainerCPUBudgetCores())
	}
	d.refreshContainerCPUNotice()
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
