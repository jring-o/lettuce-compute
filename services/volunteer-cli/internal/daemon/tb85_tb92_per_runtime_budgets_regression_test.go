package daemon

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-85 and TB-92 regression tests.
//
// TB-85, the reproduction: an 8 GB Mac mini with max_memory_mb 1024, two
// slots, and a Podman machine whose guest reports 1,280 MB — a container
// budget of 768. The VM clip (TB-63) was applied to the daemon's ONE memory
// figure, so it was booked at admission for every runtime, enforced on native
// tasks, and advertised as the host's only max_memory_mb. A 128 MB native
// Beyblade unit could not start beside a running 768 MB container Beyblade
// ("configured memory budget: 768 MB active + 128 MB unit exceeds
// max_memory_mb 768"), the second slot idled, and no native leaf above 768 MB
// was ever dispatched to a machine that could run it in its own RAM. The CPU
// clip (TB-75) had the same shape. Now container work is budgeted against
// the VM and everything together against the limit, native and WASM work
// against the limit alone, and heads are told both figures.
//
// TB-92, the reproduction: a MacBook whose VM reports 8,417 MB (a container
// budget of 7,905) and a Memory slider at 7,936. The container_memory_clipped
// WARN and "Needs attention" notice fired at every start for a 31 MB
// overshoot the slider's 256 MB stops could never close, although every leaf
// he had enabled fitted the budget. Now the notice is raised only when the
// VM keeps an enabled container leaf from running.

// headNativeUnit is a native work unit exactly as a head sends it (runtime
// "NATIVE"), converted the way the fetcher converts every assignment.
func headNativeUnit(id, leafID string, memMB int32) *runtime.WorkUnit {
	return runtime.WorkUnitFromProto(&lettucev1.WorkUnitAssignment{
		WorkUnitId: id,
		LeafId:     leafID,
		Runtime:    "NATIVE",
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Binaries:    map[string]string{"darwin-arm64": "https://example.org/beyblade"},
			MaxMemoryMb: memMB,
		},
	})
}

// tb85Leafs is the mini's catalog: the container Beyblade (768 MB) and the
// native one (128 MB, minCores cores).
func tb85Leafs(minCores int32) []CachedLeafInfo {
	return []CachedLeafInfo{
		{ID: "leaf-bb-c", Slug: "beyblade-container", Name: "Beyblade (container)", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/beyblade:1", MaxMemoryMB: 768}},
		{ID: "leaf-bb-n", Slug: "beyblade-native", Name: "Beyblade (native)", State: "ACTIVE",
			ExecutionSpec:        &CachedExecutionSpec{Binaries: map[string]string{"darwin-arm64": "https://example.org/beyblade"}, MaxMemoryMB: 128},
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: minCores}},
	}
}

// tb85Mini is the reproduction's host after the container engine is
// detected: max_memory_mb memMB and max_cpu_cores cores configured, a
// container engine whose VM reports vmMB of memory and vmCPUs CPUs, two
// slots, a real WASM runtime with the daemon's live ceilings wired in, and
// the admission guards the test is not about isolated away.
func tb85Mini(t *testing.T, memMB, cores, vmMB, vmCPUs int) (*Daemon, *reRegMockClient, *runtime.WasmRuntime, *bytes.Buffer) {
	t.Helper()
	d, rc, buf := tb63Daemon(t)
	d.cfg.ResourceLimits.MaxMemoryMB = memMB
	d.cfg.ResourceLimits.MaxCPUCores = cores
	d.cfg.MaxConcurrentTasks = 2
	d.cachedHW = &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: int32(cores), MemoryTotalMb: 8192, MaxMemoryMb: int32(memMB), Os: "darwin"}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: tb85Leafs(1)})
	f := tb63Factory(t, d, 0)
	f.SetEngineVMProbeForTest(func(runtime.Runtime) (int, int) { return vmMB, vmCPUs })
	d.containerFactory = f
	wr := runtime.NewWasmRuntime(t.TempDir(), d.logger)
	d.runtimeRegistry.Register(wr)
	d.wireRuntimeLimits(nil, &testLimiter{})
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	d.slotManager = NewSlotManager(2, d.logger)
	d.limiter = &testLimiter{} // the disk guard is not under test
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	return d, rc, wr, buf
}

// TestTB85_NativeUnitStartsBesideAContainerWithinTheLimit is the reproduction
// with its outcome inverted: beside a running 768 MB container unit, a 128 MB
// native unit is admitted (768 + 128 fits the 1,024 MB limit, and the native
// unit is not inside the VM), and the starved-backfill round (TB-32) sees the
// native leaf as able to fill the idle slot. Container work is still bounded
// by the VM — a second container unit is refused — and everything together
// by the limit. Pre-fix the native unit was refused with "768 MB active + 128
// MB unit exceeds max_memory_mb 768" and the second slot idled.
func TestTB85_NativeUnitStartsBesideAContainerWithinTheLimit(t *testing.T) {
	d, _, _, _ := tb85Mini(t, 1024, 2, 1280, 2)
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 768 {
		t.Fatalf("advertised max_memory_mb = %d, want the VM's 768 (1280 less %d headroom)", got, runtime.ContainerVMHeadroomMB)
	}

	container := headContainerUnit("c-1", "leaf-bb-c", "ghcr.io/example/beyblade:1", 768)
	if ok, why := d.canAccommodateWU(container); !ok {
		t.Fatalf("a 768 MB container unit refused on an idle machine: %s", why)
	}
	occupy(d, 0, container, nil)

	native := headNativeUnit("n-1", "leaf-bb-n", 128)
	if ok, why := d.canAccommodateWU(native); !ok {
		t.Errorf("a 128 MB native unit was refused beside a 768 MB container on a 1024 MB limit: %s", why)
	}
	if ok, why := d.leafFitGate(tb85Leafs(1)[1]); !ok {
		t.Errorf("the starved-backfill gate says the native leaf cannot fill the idle slot: %s", why)
	}
	if d.mayDelayAdmission(container, native) {
		t.Error("mayDelayAdmission = true: a 768 MB container and a 128 MB native unit fit the budgets together")
	}

	// Container work stays inside the VM: 768 + 128 of container work does
	// not fit its 768 MB.
	second := headContainerUnit("c-2", "leaf-bb-c", "", 128)
	if ok, _ := d.canAccommodateWU(second); ok {
		t.Error("a second container unit was admitted beyond the 768 MB the VM can hold")
	}
	if !d.mayDelayAdmission(container, second) {
		t.Error("mayDelayAdmission = false: two container units that do not fit the VM together")
	}

	// And everything together stays within the limit: 768 + 300 > 1024.
	ok, why := d.canAccommodateWU(headNativeUnit("n-2", "leaf-bb-n", 300))
	if ok {
		t.Error("a 300 MB native unit was admitted beside a 768 MB container on a 1024 MB limit")
	} else if !strings.Contains(why, "max_memory_mb 1024") {
		t.Errorf("refusal does not name the 1024 MB limit: %s", why)
	}
}

// TestTB85_NativeWorkIsBoundedByTheLimitNotTheVM: a native unit declaring
// 900 MB — above the VM's 768, within the 1,024 MB limit — is neither given
// back at arrival nor swept from the buffer, is admitted, and the native
// limiter enforces it at its declaration; the WASM runtime's ceiling is the
// limit; the container runtime's is still the VM's. Pre-fix every one of
// these was the VM's 768, and a head that sent such a unit had it returned.
func TestTB85_NativeWorkIsBoundedByTheLimitNotTheVM(t *testing.T) {
	d, _, wr, _ := tb85Mini(t, 1024, 2, 1280, 2)

	native := headNativeUnit("n-900", "leaf-bb-n", 900)
	if ok, why := d.memoryDeclarationFits(native); !ok {
		t.Errorf("a 900 MB native unit would be given back at arrival on a 1024 MB limit: %s", why)
	}
	if why := d.unfitBuffered(native); why != "" {
		t.Errorf("a buffered 900 MB native unit would be swept: %s", why)
	}
	if ok, why := d.canAccommodateWU(native); !ok {
		t.Errorf("a 900 MB native unit refused on an idle machine with a 1024 MB limit: %s", why)
	}
	if got := d.taskLimits(900, runtime.CPUGrant{}).MaxMemoryMB; got != 900 {
		t.Errorf("native limiter ceiling for a 900 MB unit = %d MB, want 900", got)
	}
	if got := wr.MemoryCeilingMB(); got != 1024 {
		t.Errorf("WASM ceiling = %d MB, want the 1024 MB limit", got)
	}

	// A container unit of the same size still cannot run in the VM.
	big := headContainerUnit("c-900", "leaf-bb-c", "", 900)
	ok, why := d.memoryDeclarationFits(big)
	if ok {
		t.Error("a 900 MB container unit fits a 768 MB container budget")
	}
	for _, want := range []string{"900 MB", "768 MB"} {
		if !strings.Contains(why, want) {
			t.Errorf("give-back reason lacks %q: %s", want, why)
		}
	}
	cr := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if got := cr.MemoryCeilingMB(); got != 768 {
		t.Errorf("container ceiling = %d MB, want the VM's 768", got)
	}
}

// TestTB85_HeadsAreToldBothBudgets: the advertisement carries the container
// budgets as max_memory_mb / max_cpu_cores — what every runtime's work fits,
// so a head that knows nothing else stays safe — and the configured limits as
// host_max_memory_mb / host_max_cpu_cores, the figures a head compares a
// native or WASM leaf with. Every poll carries both. Pre-fix the host figures
// did not exist and the head only ever saw the VM's.
func TestTB85_HeadsAreToldBothBudgets(t *testing.T) {
	d, rc, _, _ := tb85Mini(t, 1024, 2, 1280, 2)

	// A live change rebuilds the advertisement from the budgets (TB-79):
	// four cores on a two-vCPU VM.
	changed := *d.cfg
	changed.ResourceLimits.MaxCPUCores = 4
	d.ApplyConfig(&changed)

	hw := d.AdvertisedHardware()
	if hw.MaxMemoryMb != 768 || hw.HostMaxMemoryMb != 1024 {
		t.Errorf("advertised memory = %d / host %d, want 768 / 1024", hw.MaxMemoryMb, hw.HostMaxMemoryMb)
	}
	if hw.MaxCpuCores != 2 || hw.HostMaxCpuCores != 4 {
		t.Errorf("advertised cores = %d / host %d, want 2 (the VM's) / 4", hw.MaxCpuCores, hw.HostMaxCpuCores)
	}

	var mu sync.Mutex
	var polled []*lettucev1.HardwareCapabilities
	rc.mockClient.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		polled = append(polled, req.GetCurrentAvailable())
		mu.Unlock()
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	// No limiter: the fetch-side disk gate would ask the (client-less) test
	// container runtime whether images are cached.
	d.limiter = nil
	saved := d.logger
	runFetcherFor(d, 200*time.Millisecond)
	d.logger = saved
	mu.Lock()
	defer mu.Unlock()
	if len(polled) == 0 {
		t.Fatal("no poll reached the head")
	}
	for _, p := range polled {
		if p.GetHostMaxMemoryMb() != 1024 || p.GetHostMaxCpuCores() != 4 || p.GetMaxMemoryMb() != 768 {
			t.Fatalf("a poll carried max %d / host %d MB, host %d cores; want 768 / 1024 / 4", p.GetMaxMemoryMb(), p.GetHostMaxMemoryMb(), p.GetHostMaxCpuCores())
		}
	}
}

// TestTB85_CPUBudgetIsPerRuntimeToo is the CPU twin: four cores configured,
// a two-vCPU VM. A native unit whose leaf needs two cores is admitted beside
// a running one-core container unit (1 + 2 fits the four-core limit), while a
// third container core does not exist in the VM. Running, two container
// tasks share the VM's two CPUs — one each — and the native task is given
// what they leave of the four, two. Pre-fix the whole budget was the VM's
// two cores: the native unit was refused, and three tasks were given 0.66 of
// a core each.
func TestTB85_CPUBudgetIsPerRuntimeToo(t *testing.T) {
	d, _, _, _ := tb85Mini(t, 8192, 4, 8192+runtime.ContainerVMHeadroomMB, 2)
	d.slotManager = NewSlotManager(3, d.logger)
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: tb85Leafs(2)})

	c1 := headContainerUnit("c-1", "leaf-bb-c", "", 1024)
	occupy(d, 0, c1, nil)
	native := headNativeUnit("n-1", "leaf-bb-n", 128)
	if ok, why := d.canAccommodateWU(native); !ok {
		t.Errorf("a two-core native unit refused beside a one-core container unit on a four-core limit: %s", why)
	}
	c2 := headContainerUnit("c-2", "leaf-bb-c", "", 1024)
	occupy(d, 1, c2, nil)
	if ok, _ := d.canAccommodateWU(headContainerUnit("c-3", "leaf-bb-c", "", 1024)); ok {
		t.Error("a third container unit was admitted with both of the VM's CPUs booked")
	}

	h1, h2, hn := &mockProcessHandle{pid: 1}, &mockProcessHandle{pid: 2}, &mockProcessHandle{pid: 3}
	release(d, 0)
	release(d, 1)
	occupy(d, 0, c1, h1)
	occupy(d, 1, c2, h2)
	occupy(d, 2, native, hn)
	d.rebalanceCPUShares()
	if len(h1.cpuShares) != 1 || h1.cpuShares[0] != 1 || len(h2.cpuShares) != 1 || h2.cpuShares[0] != 1 {
		t.Errorf("container tasks' shares = %v / %v, want 1 each (the VM's two CPUs between them)", h1.cpuShares, h2.cpuShares)
	}
	if len(hn.cpuShares) != 1 || hn.cpuShares[0] != 2 {
		t.Errorf("native task's share = %v, want 2 (what the containers leave of the four-core limit)", hn.cpuShares)
	}
}

// TestTB92_ClipNoticeOnlyWhenAnEnabledContainerLeafIsHeldBack is TB-92's
// reproduction with its outcome inverted: a VM budget of 7,905 MB under a
// 7,936 MB limit, with the only enabled leaf a 7,000 MB container leaf that
// fits it, raises no notice and no WARN — the heads are still told 7,905. The
// same holds for a deliberately small VM (TB-85's mini: 768 MB of container
// budget under 1,024, both Beyblades fitting). A leaf the VM does keep from
// running — 7,920 MB, above the budget, within the limit — raises the notice
// naming it once the catalog carries it, and looking again with nothing
// changed does not raise it again. Pre-fix the notice was raised at every
// start for any overshoot, 31 MB included.
func TestTB92_ClipNoticeOnlyWhenAnEnabledContainerLeafIsHeldBack(t *testing.T) {
	d, _, buf := tb63Daemon(t) // one enabled container leaf, GREP f14 at 7000 MB
	d.cfg.ResourceLimits.MaxMemoryMB = 7936
	d.cachedHW.MaxMemoryMb = 7936
	d.containerFactory = tb63Factory(t, d, 8417)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	if !d.MemoryLimitedByVM() {
		t.Fatal("MemoryLimitedByVM = false with a 7905 MB budget under a 7936 MB limit")
	}
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 7905 {
		t.Errorf("advertised max_memory_mb = %d, want the VM's 7905", got)
	}
	if n, notice := countNoticesByCode(d.notices, "container_memory_clipped"); n != 0 {
		t.Errorf("container_memory_clipped raised for a 31 MB overshoot although the only enabled leaf fits the VM: %s", notice.Message)
	}
	if n := vmClipWarns(buf); n != 0 {
		t.Errorf("%d VM-clip WARN(s) logged although nothing is held back:\n%s", n, buf.String())
	}

	// A leaf the VM keeps from running arrives with the next catalog.
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{Name: "server-a", Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep-f14", Name: "GREP f14", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/grep:1.2", MaxMemoryMB: 7000}},
		{ID: "leaf-grep15", Slug: "grep-f15", Name: "GREP f15", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/grep:1.3", MaxMemoryMB: 7920}},
	}})
	d.refreshContainerMemoryNotice()
	d.refreshContainerMemoryNotice() // nothing changed: no second emission
	n, notice := countNoticesByCode(d.notices, "container_memory_clipped")
	if n != 1 || notice.Count != 1 || notice.ResolvedAt != nil {
		t.Fatalf("container_memory_clipped: %d notice(s), count %d, resolved %v; want one live notice raised once", n, notice.Count, notice.ResolvedAt)
	}
	for _, want := range []string{"GREP f15", "7920 MB", "7905 MB", "7936 MB", "podman machine set --memory"} {
		if !strings.Contains(notice.Message, want) {
			t.Errorf("notice lacks %q: %s", want, notice.Message)
		}
	}
	if strings.Contains(notice.Message, "GREP f14") {
		t.Errorf("notice names GREP f14, which fits the VM: %s", notice.Message)
	}

	// The deliberately small VM: nothing enabled is held back.
	mini, _, _, miniBuf := tb85Mini(t, 1024, 2, 1280, 2)
	if n, notice := countNoticesByCode(mini.notices, "container_memory_clipped"); n != 0 {
		t.Errorf("container_memory_clipped raised on the mini though both Beyblades fit their budgets: %s", notice.Message)
	}
	if n := vmClipWarns(miniBuf); n != 0 {
		t.Errorf("%d VM-clip WARN(s) logged on the mini although nothing is held back:\n%s", n, miniBuf.String())
	}
}

// vmClipWarns counts the WARN lines in a JSON log that are about the
// container engine's VM — the clip notices' WARNs, before and after TB-92.
func vmClipWarns(buf *bytes.Buffer) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "VM") {
			n++
		}
	}
	return n
}
