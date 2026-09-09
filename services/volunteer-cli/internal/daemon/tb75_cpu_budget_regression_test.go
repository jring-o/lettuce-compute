package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-75 regression tests, daemon half.
//
// The reproduction: "CPU Cores · 2 / 8" beside "Concurrent Tasks · 2" on a
// tester's 4-vCPU Podman machine. max_cpu_cores was applied per TASK — every
// container got the whole figure as its own quota and admission booked no
// cores — so two tasks used four cores and filled his machine. Now the figure
// is a whole-machine budget: admission books each unit's minimum core
// requirement against it, the running tasks are given equal shares that are
// adjusted live on every start and finish, and the budget is clipped to the
// engine VM's vCPUs the way the memory budget is clipped to its RAM.

// tb75Daemon is the tester's configuration on a host with no VM: a 2-core
// budget, three slots, and every other admission guard isolated away.
func tb75Daemon(t *testing.T) *Daemon {
	t.Helper()
	// A limiter whose disk check always passes: the disk guard is not under
	// test, and the real one reads the test host's free space.
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{},
		resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger()))
	d.cfg.ResourceLimits.MaxCPUCores = 2
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.cfg.MaxConcurrentTasks = 3
	d.slotManager = NewSlotManager(3, d.logger)
	d.slotManager.SetCPUShareSource(d.currentCPUShare)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	// Two leaves: a one-core one (the tester's Beyblade) and one declaring
	// min_cpu_cores 2 (his GREP), which the head's gate compares against the
	// advertised max_cpu_cores.
	d.leafCache.PopulateForTest("test", &CachedHeadInfo{Name: "test", Leafs: []CachedLeafInfo{
		{ID: "leaf-bb", Slug: "beyblade", State: "ACTIVE", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1}},
		{ID: "leaf-grep", Slug: "grep", State: "ACTIVE", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2}},
	}})
	return d
}

// occupy marks slot i active with wu and the given handle, the state a slot
// is in while a unit runs, without a goroutine.
func occupy(d *Daemon, i int, wu *runtime.WorkUnit, h ProcessHandle) *ExecutionSlot {
	slot := d.slotManager.slots[i]
	slot.mu.Lock()
	slot.active = true
	slot.wu = wu
	slot.processHandle = h
	slot.mu.Unlock()
	return slot
}

// release marks slot i inactive, as runSlot's deferred cleanup does.
func release(d *Daemon, i int) {
	slot := d.slotManager.slots[i]
	slot.mu.Lock()
	slot.active = false
	slot.wu = nil
	slot.processHandle = nil
	slot.cpuShare = 0
	slot.mu.Unlock()
}

func bbUnit(id string) *runtime.WorkUnit {
	return &runtime.WorkUnit{ID: id, LeafID: "leaf-bb", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{Image: "ghcr.io/example/bb:1"}}
}

func grepUnit(id string) *runtime.WorkUnit {
	return &runtime.WorkUnit{ID: id, LeafID: "leaf-grep", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{Image: "ghcr.io/example/grep:1"}}
}

// TestTB75_AdmissionBooksCoresAgainstTheBudget: under max_cpu_cores 2, two
// one-core units are admitted and a third is refused, whatever
// max_concurrent_tasks says; a unit whose leaf declares two cores takes the
// whole budget, so nothing is admitted beside it, and the backfill delay test
// sees the same bookings. Pre-fix canAccommodateWU had no CPU guard at all:
// three units were admitted and each was given two cores.
func TestTB75_AdmissionBooksCoresAgainstTheBudget(t *testing.T) {
	d := tb75Daemon(t)

	if ok, reason := d.canAccommodateWU(bbUnit("bb-1")); !ok {
		t.Fatalf("first one-core unit refused on an idle 2-core budget: %s", reason)
	}
	occupy(d, 0, bbUnit("bb-1"), nil)
	if ok, reason := d.canAccommodateWU(bbUnit("bb-2")); !ok {
		t.Fatalf("second one-core unit refused with 1 of 2 cores booked: %s", reason)
	}
	occupy(d, 1, bbUnit("bb-2"), nil)
	ok, reason := d.canAccommodateWU(bbUnit("bb-3"))
	if ok {
		t.Fatal("a third one-core unit was admitted under max_cpu_cores 2 with three slots free — the budget is not a total")
	}
	for _, want := range []string{"configured CPU budget", "2 core(s) booked", "max_cpu_cores 2"} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal lacks %q: %s", want, reason)
		}
	}
	if got := d.slotManager.TotalActiveCPUCores(d.bookedCPUCores); got != 2 {
		t.Errorf("TotalActiveCPUCores = %d, want 2", got)
	}

	// The tester's policy: GREP (min_cpu_cores 2) alone, or two Beyblades.
	release(d, 0)
	release(d, 1)
	if got := d.bookedCPUCores(grepUnit("g")); got != 2 {
		t.Errorf("bookedCPUCores(grep) = %d, want the leaf's min_cpu_cores 2", got)
	}
	if ok, reason := d.canAccommodateWU(grepUnit("g-1")); !ok {
		t.Fatalf("GREP refused on an idle budget: %s", reason)
	}
	occupy(d, 0, grepUnit("g-1"), nil)
	if ok, _ := d.canAccommodateWU(bbUnit("bb-1")); ok {
		t.Error("a one-core unit was admitted beside a running two-core unit under a 2-core budget")
	}
	if !d.mayDelayAdmission(grepUnit("g-2"), bbUnit("bb-1")) {
		t.Error("mayDelayAdmission = false: a one-core candidate and a two-core blocked unit cannot fit a 2-core budget together")
	}
	if d.mayDelayAdmission(bbUnit("bb-1"), bbUnit("bb-2")) {
		t.Error("mayDelayAdmission = true: two one-core units fit the budget together and cannot delay each other")
	}

	// A leaf the cache does not know books one core; a leaf declaring more
	// than the budget is clamped to it and runs alone.
	unknown := &runtime.WorkUnit{ID: "u", LeafID: "leaf-unknown"}
	if got := d.bookedCPUCores(unknown); got != 1 {
		t.Errorf("bookedCPUCores(unknown leaf) = %d, want 1", got)
	}
	d.leafCache.PopulateForTest("test", &CachedHeadInfo{Name: "test", Leafs: []CachedLeafInfo{
		{ID: "leaf-big", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 8}},
	}})
	if got := d.bookedCPUCores(&runtime.WorkUnit{ID: "b", LeafID: "leaf-big"}); got != 2 {
		t.Errorf("bookedCPUCores(8-core leaf) = %d, want the 2-core budget", got)
	}
}

// TestTB75_RunningTasksShareTheBudgetLive is the decided rule 2, driven
// through the daemon's rebalance: a task alone is given the whole 2-core
// budget; when a second starts each is given 1; when one finishes the
// survivor is given 2 again — and at every step the shares sum to at most
// the budget. A handle attached after the split changed is brought to the
// current share at once, and a rebalance that changes nothing touches no
// task. Pre-fix nothing ever adjusted a running task, and every task was
// created with the whole budget.
func TestTB75_RunningTasksShareTheBudgetLive(t *testing.T) {
	d := tb75Daemon(t)

	h0 := &mockProcessHandle{pid: 1}
	occupy(d, 0, bbUnit("bb-1"), h0)
	if got := d.cpuGrant(); got.ShareCores != 2 || got.BudgetCores != 2 {
		t.Fatalf("grant with one task running = %+v, want 2 of 2", got)
	}
	d.rebalanceCPUShares()

	h1 := &mockProcessHandle{pid: 2}
	occupy(d, 1, bbUnit("bb-2"), h1)
	if got := d.cpuGrant().ShareCores; got != 1 {
		t.Fatalf("grant with two tasks running = %v, want 1 (half of 2)", got)
	}
	d.rebalanceCPUShares()
	d.rebalanceCPUShares() // unchanged split: no second call to any handle

	release(d, 1)
	d.rebalanceCPUShares()

	if want := []float64{2, 1, 2}; !reflect.DeepEqual(h0.cpuShares, want) {
		t.Errorf("first task's shares over start/second start/second finish = %v, want %v", h0.cpuShares, want)
	}
	if want := []float64{1}; !reflect.DeepEqual(h1.cpuShares, want) {
		t.Errorf("second task's shares = %v, want %v", h1.cpuShares, want)
	}

	// A handle registered after the split changed (the newcomer's container
	// created before its registration, or a container adopted from the
	// previous session) is brought to the current share on attach.
	occupy(d, 1, bbUnit("bb-3"), nil)
	late := &mockProcessHandle{pid: 3}
	d.slotManager.attachProcessHandle(d.slotManager.slots[1], late)
	if want := []float64{1}; !reflect.DeepEqual(late.cpuShares, want) {
		t.Errorf("late-attached handle's shares = %v, want %v (the current half-split)", late.cpuShares, want)
	}

	// The environment a task starting now would be told: the same share.
	if got := d.cpuGrant().Env()[0]; got != "LETTUCE_CPU_LIMIT=1" {
		t.Errorf("grant env = %q, want LETTUCE_CPU_LIMIT=1", got)
	}

	// A raised limit is re-split at once (ApplyConfig), a lone survivor gets
	// the whole new budget.
	release(d, 1)
	raised := *d.cfg
	raised.ResourceLimits.MaxCPUCores = 3
	d.ApplyConfig(&raised)
	if last := h0.cpuShares[len(h0.cpuShares)-1]; last != 3 {
		t.Errorf("after raising max_cpu_cores to 3 the running task's share = %v, want 3", last)
	}
}

// TestTB75_FractionalSharesAreExact: a 3-core budget shared by two tasks is
// 1.5 each — the CFS quota is 150000 of 100000 — and by three tasks 1 each.
func TestTB75_FractionalSharesAreExact(t *testing.T) {
	d := tb75Daemon(t)
	d.cfg.ResourceLimits.MaxCPUCores = 3
	h0, h1 := &mockProcessHandle{pid: 1}, &mockProcessHandle{pid: 2}
	occupy(d, 0, bbUnit("bb-1"), h0)
	occupy(d, 1, bbUnit("bb-2"), h1)
	d.rebalanceCPUShares()
	if h0.cpuShares[0] != 1.5 || h1.cpuShares[0] != 1.5 {
		t.Errorf("shares of 3 cores over two tasks = %v / %v, want 1.5 each", h0.cpuShares, h1.cpuShares)
	}
	if q, p := runtime.CFSQuota(h0.cpuShares[0]); q != 150000 || p != 100000 {
		t.Errorf("CFS quota for 1.5 cores = %d/%d, want 150000/100000", q, p)
	}
}

// TestTB75_EngineVMClipsTheCPUBudget: a 4-vCPU Podman machine bounds a
// 6-core limit. The budget, the advertisement and every booking become 4,
// the volunteer is told once with both figures and the remedy, and lowering
// the limit to the VM's count resolves the notice. The machine's memory is
// large enough not to clip, so this is the CPU clip alone. Pre-fix the VM's
// CPU count was read for the Settings card and used for nothing.
func TestTB75_EngineVMClipsTheCPUBudget(t *testing.T) {
	d, _, buf := tb63Daemon(t)
	d.cfg.ResourceLimits.MaxCPUCores = 6
	d.cachedHW.MaxCpuCores = 6
	f := tb63Factory(t, d, 0)
	f.SetEngineVMProbeForTest(func(runtime.Runtime) (int, int) { return 8192 + runtime.ContainerVMHeadroomMB, 4 })
	d.containerFactory = f
	before := d.AdvertisedHardware()

	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	if got := d.ContainerVMCPUs(); got != 4 {
		t.Errorf("ContainerVMCPUs = %d, want 4", got)
	}
	if got := d.CPUBudgetCores(); got != 4 {
		t.Errorf("CPUBudgetCores = %d, want 4 (the VM's), not the 6 configured", got)
	}
	if !d.CPULimitedByVM() {
		t.Error("CPULimitedByVM = false; the VM is below the configuration")
	}
	if got := d.AdvertisedHardware().MaxCpuCores; got != 4 {
		t.Errorf("advertised MaxCpuCores = %d, want 4", got)
	}
	if before.MaxCpuCores != 6 {
		t.Errorf("the previous advertisement object was written in place (MaxCpuCores = %d); it must be replaced by a copy", before.MaxCpuCores)
	}
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 8192 {
		t.Errorf("MaxMemoryMb = %d after the CPU clip, want 8192 (the VM's memory honors it)", got)
	}
	if got := d.cpuGrant(); got.BudgetCores != 4 || got.ShareCores != 4 {
		t.Errorf("grant after the clip = %+v, want 4 of 4", got)
	}
	if budget, cpus := f.ContainerCPUs(); budget != 4 || cpus != 4 {
		t.Errorf("factory ContainerCPUs = %d/%d, want 4/4", budget, cpus)
	}

	n, notice := countNoticesByCode(d.notices, "container_cpu_clipped")
	if n != 1 || notice.Count != 1 {
		t.Fatalf("container_cpu_clipped notices = %d (count %d), want exactly one", n, notice.Count)
	}
	for _, want := range []string{"4 CPU cores", "4 CPUs", "6 cores", "podman machine set --cpus"} {
		if !strings.Contains(notice.Message, want) {
			t.Errorf("notice lacks %q: %s", want, notice.Message)
		}
	}
	if c := strings.Count(buf.String(), "fewer CPUs than the CPU limit"); c != 1 {
		t.Errorf("clip WARN logged %d time(s), want exactly 1; log:\n%s", c, buf.String())
	}
	if n, _ := countNoticesByCode(d.notices, "container_memory_clipped"); n != 0 {
		t.Errorf("container_memory_clipped raised %d time(s) though the VM's memory honors the limit", n)
	}

	// Admission books against the clipped budget: a leaf declaring 4 cores
	// runs alone; a fifth core does not exist.
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = defaultFreeSystemMemoryMB }()
	d.limiter = &testLimiter{} // the disk guard is not under test
	d.slotManager = NewSlotManager(2, d.logger)
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: []CachedLeafInfo{
		{ID: "leaf-four", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 4}},
	}})
	four := &runtime.WorkUnit{ID: "four-1", LeafID: "leaf-four", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}
	if ok, reason := d.canAccommodateWU(four); !ok {
		t.Errorf("a 4-core unit refused against a 4-core budget: %s", reason)
	}
	occupy(d, 0, four, nil)
	if ok, _ := d.canAccommodateWU(&runtime.WorkUnit{ID: "one", LeafID: "leaf-grep", ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}); ok {
		t.Error("a unit was admitted beside a 4-core unit under the VM's 4-core budget")
	}

	// Lowering the limit to the VM's count resolves the notice.
	lowered := *d.cfg
	lowered.ResourceLimits.MaxCPUCores = 4
	d.ApplyConfig(&lowered)
	if d.CPULimitedByVM() {
		t.Error("CPULimitedByVM = true with the limit at the VM's count")
	}
	if _, notice := countNoticesByCode(d.notices, "container_cpu_clipped"); notice.ResolvedAt == nil {
		t.Errorf("notice not resolved after the limit was lowered to the VM's count: %+v", notice)
	}
}

// TestTB75_VMWithEnoughCPUsKeepsTheConfiguration: a 4-vCPU machine under a
// 2-core limit (the tester's actual setup) clips nothing and raises no notice;
// no VM at all (Linux) likewise.
func TestTB75_VMWithEnoughCPUsKeepsTheConfiguration(t *testing.T) {
	for _, vmCPUs := range []int{4, 0} {
		d, _, buf := tb63Daemon(t)
		d.cfg.ResourceLimits.MaxCPUCores = 2
		d.cachedHW.MaxCpuCores = 2
		f := tb63Factory(t, d, 0)
		f.SetEngineVMProbeForTest(func(runtime.Runtime) (int, int) { return 0, vmCPUs })
		d.containerFactory = f
		if !d.RedetectContainerRuntime(context.Background(), false) {
			t.Fatal("RedetectContainerRuntime = false with the engine up")
		}
		if d.CPUBudgetCores() != 2 || d.CPULimitedByVM() || d.AdvertisedHardware().MaxCpuCores != 2 {
			t.Errorf("vm cpus %d: budget %d, limited %v, advertised %d; want 2 / false / 2", vmCPUs, d.CPUBudgetCores(), d.CPULimitedByVM(), d.AdvertisedHardware().MaxCpuCores)
		}
		if got := d.ContainerVMCPUs(); got != vmCPUs {
			t.Errorf("ContainerVMCPUs = %d, want %d", got, vmCPUs)
		}
		if n, _ := countNoticesByCode(d.notices, "container_cpu_clipped"); n != 0 {
			t.Errorf("vm cpus %d: container_cpu_clipped raised %d time(s)", vmCPUs, n)
		}
		if strings.Contains(buf.String(), "fewer CPUs") {
			t.Errorf("vm cpus %d: clip WARN logged:\n%s", vmCPUs, buf.String())
		}
	}
}

// TestTB75_StartupAdvertisementIsClamped: the factory clamp start-up applies
// to the advertisement registration sends, the CPU twin of
// ClampAdvertisedMemory.
func TestTB75_StartupAdvertisementIsClamped(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.cfg.ResourceLimits.MaxCPUCores = 6
	f := tb63Factory(t, d, 0)
	f.SetEngineVMProbeForTest(func(runtime.Runtime) (int, int) { return 0, 4 })
	if _, _, err := f.Build(false); err != nil {
		t.Fatalf("Build: %v", err)
	}
	hw := &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: 6, MaxMemoryMb: 8192}
	if !f.ClampAdvertisedCPU(hw) || hw.MaxCpuCores != 4 {
		t.Errorf("ClampAdvertisedCPU: MaxCpuCores = %d, want 4", hw.MaxCpuCores)
	}
	if f.ClampAdvertisedCPU(hw) {
		t.Error("a second clamp of an already-clamped advertisement reported a change")
	}
	small := &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: 2}
	if f.ClampAdvertisedCPU(small) || small.MaxCpuCores != 2 {
		t.Errorf("a limit under the VM's count was clamped to %d", small.MaxCpuCores)
	}
}
