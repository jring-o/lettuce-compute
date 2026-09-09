package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-79 regression tests.
//
// The reproduction: a 16 GB MacBook whose volunteer lowered the memory limit
// from 7168 to 6912 MB in the desktop app and skipped the restart the app
// asked for. ApplyConfig swapped the configuration, so admission booked
// against 6912 at once — but the advertisement heads compare a leaf's
// max_memory_mb against on every poll stayed at the start-up figure, so the
// head kept sending 7000 MB GREP units for 46 hours (25 of them in one day),
// and the container runtime's memory ceiling, the WASM ceiling and the native
// limiter's ceiling all stayed at the start-up figure too, so the units ran
// at 7000 while admission accounted 6912. Now a resource_limits change is
// applied everywhere in one step: the advertisement is rebuilt from the live
// budget (the next poll carries it, no restart), every runtime reads its
// memory ceiling from the live budget, and a unit whose declaration exceeds
// the budget is given back un-run — never started clamped below what its
// leaf asked for.

// tb79Daemon is the reproduction's host with no engine VM (the engine shares
// the host's RAM, so nothing clips the configuration): max_memory_mb 8192
// advertised, a container runtime registered late through the detector, a
// real WASM runtime beside it, and the daemon's live ceilings wired into both.
func tb79Daemon(t *testing.T) (*Daemon, *reRegMockClient, *runtime.ContainerRuntime, *runtime.WasmRuntime) {
	t.Helper()
	d, rc, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 0)
	wr := runtime.NewWasmRuntime(t.TempDir(), d.logger)
	d.runtimeRegistry.Register(wr)
	d.wireRuntimeLimits(nil, &testLimiter{})
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		t.Fatal("no container runtime registered")
	}
	return d, rc, cr, wr
}

// TestTB79_LoweredMemoryLimitReachesHeadsAndCeilingsAtOnce is the
// reproduction with its outcome inverted: lowering max_memory_mb from 8192 to
// 6912 through ApplyConfig lowers, in the same call, the advertisement the
// next poll carries, the container runtime's ceiling, the WASM ceiling and the
// figure the native limiter clamps to — the same 6912 admission books. Raising
// it works the same way in reverse. Pre-fix every one of those stayed 8192.
func TestTB79_LoweredMemoryLimitReachesHeadsAndCeilingsAtOnce(t *testing.T) {
	d, rc, cr, wr := tb79Daemon(t)
	before := d.AdvertisedHardware()
	if before.MaxMemoryMb != 8192 || cr.MemoryCeilingMB() != 8192 || wr.MemoryCeilingMB() != 8192 {
		t.Fatalf("start state: advertised %d, container ceiling %d, wasm ceiling %d; want 8192 each",
			before.MaxMemoryMb, cr.MemoryCeilingMB(), wr.MemoryCeilingMB())
	}
	if got := d.taskLimits(7000, runtime.CPUGrant{}).MaxMemoryMB; got != 7000 {
		t.Fatalf("start state: native ceiling for a 7000 MB unit = %d, want 7000", got)
	}

	lowered := *d.cfg
	lowered.ResourceLimits.MaxMemoryMB = 6912
	d.ApplyConfig(&lowered)

	if got := d.MemoryBudgetMB(); got != 6912 {
		t.Errorf("MemoryBudgetMB = %d, want 6912", got)
	}
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 6912 {
		t.Errorf("advertised MaxMemoryMb after lowering = %d, want 6912 (the head's gate must see the new budget on the next poll)", got)
	}
	if before.MaxMemoryMb != 8192 {
		t.Errorf("the previous advertisement object was written in place (MaxMemoryMb = %d); it must be replaced by a copy", before.MaxMemoryMb)
	}
	if got := d.AdvertisedHardware().MemoryTotalMb; got != 16384 {
		t.Errorf("MemoryTotalMb = %d after the change, want 16384 (only the budget changes)", got)
	}
	if got := cr.MemoryCeilingMB(); got != 6912 {
		t.Errorf("container runtime ceiling after lowering = %d, want 6912 — a 7000 MB unit would otherwise run above what admission booked", got)
	}
	if got := wr.MemoryCeilingMB(); got != 6912 {
		t.Errorf("wasm runtime ceiling after lowering = %d, want 6912", got)
	}
	if got := d.taskLimits(7000, runtime.CPUGrant{}).MaxMemoryMB; got != 6912 {
		t.Errorf("native ceiling for a 7000 MB unit after lowering = %d, want 6912", got)
	}

	// The next poll carries 6912.
	var mu sync.Mutex
	var polled []int32
	rc.mockClient.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		polled = append(polled, req.GetCurrentAvailable().GetMaxMemoryMb())
		mu.Unlock()
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	saved := d.logger
	_, buf := runFetcherFor(d, 200*time.Millisecond)
	d.logger = saved
	mu.Lock()
	if len(polled) == 0 {
		mu.Unlock()
		t.Fatal("no poll reached the head")
	}
	for _, mb := range polled {
		if mb != 6912 {
			mu.Unlock()
			t.Fatalf("a poll carried CurrentAvailable.max_memory_mb %d, want 6912 on every poll: %v", mb, polled)
		}
	}
	mu.Unlock()
	if strings.Contains(buf.String(), "re-registration") {
		t.Errorf("a resource-limit change must not re-register the host; log:\n%s", buf.String())
	}

	// Raising is live in the same way: the head starts sending bigger leafs
	// on the next poll, and the ceilings follow.
	raised := *d.cfg
	raised.ResourceLimits.MaxMemoryMB = 12288
	d.ApplyConfig(&raised)
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 12288 {
		t.Errorf("advertised MaxMemoryMb after raising = %d, want 12288", got)
	}
	if cr.MemoryCeilingMB() != 12288 || wr.MemoryCeilingMB() != 12288 {
		t.Errorf("ceilings after raising = %d / %d, want 12288 each", cr.MemoryCeilingMB(), wr.MemoryCeilingMB())
	}
}

// TestTB79_WholeResourceLimitsBlockIsLive: cores, disk, bandwidth and the GPU
// share follow the configuration the same way — the advertisement is the
// block, not one field of it. A GPU disabled by a zero share leaves the
// advertisement; a share set again brings it back at the new percentage.
func TestTB79_WholeResourceLimitsBlockIsLive(t *testing.T) {
	d, _, _, _ := tb79Daemon(t)
	d.detectedGPUs = []*runtime.GpuDetectionResult{{Model: "RTX 3060", Vendor: "nvidia", VRAMMB: 12288}}
	changed := *d.cfg
	changed.ResourceLimits.MaxCPUCores = 2
	changed.ResourceLimits.MaxDiskGB = 30
	changed.ResourceLimits.MaxBandwidthMbps = 50
	changed.ResourceLimits.MaxGPUVRAMPct = 25
	d.ApplyConfig(&changed)

	hw := d.AdvertisedHardware()
	if hw.MaxCpuCores != 2 || hw.MaxDiskMb != 30*1024 || hw.MaxBandwidthMbps != 50 {
		t.Errorf("advertised cores/disk/bandwidth = %d / %d / %d, want 2 / 30720 / 50", hw.MaxCpuCores, hw.MaxDiskMb, hw.MaxBandwidthMbps)
	}
	if len(hw.Gpus) != 1 || hw.Gpus[0].MaxVramPct != 25 || hw.Gpus[0].VramMb != 12288 || hw.Gpus[0].Model != "RTX 3060" {
		t.Errorf("advertised GPUs = %v, want the detected card at 25%%", hw.Gpus)
	}
	if got := d.CPUBudgetCores(); got != 2 {
		t.Errorf("CPUBudgetCores = %d, want 2", got)
	}

	off := *d.cfg
	off.ResourceLimits.MaxGPUVRAMPct = 0
	d.ApplyConfig(&off)
	if got := d.AdvertisedHardware().Gpus; len(got) != 0 {
		t.Errorf("advertised GPUs with a zero share = %v, want none", got)
	}

	// An unchanged block rebuilds nothing: the advertisement object is kept.
	same := *d.cfg
	same.WorkBufferHours = 3
	current := d.AdvertisedHardware()
	d.ApplyConfig(&same)
	if d.AdvertisedHardware() != current {
		t.Error("a change outside resource_limits replaced the advertisement")
	}
}

// TestTB79_EngineVMClipStillBoundsALiveChange: on a machine whose engine VM
// clips the budget (TB-63), a raised limit is still advertised at the VM's
// budget — the clip is applied to the live figure, not bypassed by it — and a
// limit lowered below the VM's budget is advertised as lowered.
func TestTB79_EngineVMClipStillBoundsALiveChange(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	cr := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if d.AdvertisedHardware().MaxMemoryMb != 1536 || cr.MemoryCeilingMB() != 1536 {
		t.Fatalf("start state: advertised %d, ceiling %d; want 1536 each", d.AdvertisedHardware().MaxMemoryMb, cr.MemoryCeilingMB())
	}

	raised := *d.cfg
	raised.ResourceLimits.MaxMemoryMB = 12288
	d.ApplyConfig(&raised)
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 1536 {
		t.Errorf("advertised MaxMemoryMb after raising above the VM = %d, want 1536 (the VM still clips)", got)
	}
	if got := cr.MemoryCeilingMB(); got != 1536 {
		t.Errorf("container ceiling after raising above the VM = %d, want 1536", got)
	}

	lowered := *d.cfg
	lowered.ResourceLimits.MaxMemoryMB = 1024
	d.ApplyConfig(&lowered)
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 1024 {
		t.Errorf("advertised MaxMemoryMb after lowering below the VM = %d, want 1024", got)
	}
	if got := cr.MemoryCeilingMB(); got != 1024 {
		t.Errorf("container ceiling after lowering below the VM = %d, want 1024", got)
	}
}

// TestTB79_UnitDeclaringMoreThanTheBudgetIsGivenBackNotClamped: with the
// limit at 6912, a 7000 MB unit — one the head handed out before it saw the
// new figure — is refused at admission with both figures, returned at
// arrival as an un-run give-back (budget-neutral, before any image pull),
// and swept out of the buffer the same way if it was already there. Pre-fix
// admission clamped it to 6912 and started it.
func TestTB79_UnitDeclaringMoreThanTheBudgetIsGivenBackNotClamped(t *testing.T) {
	d, rc, _ := tb63Daemon(t)
	container := &mockRuntime{canHandle: true, name: "container"}
	d.runtimeRegistry.Register(container)
	d.cfg.MaxConcurrentTasks = 2
	d.slotManager = NewSlotManager(2, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.limiter = &testLimiter{}
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = defaultFreeSystemMemoryMB }()

	lowered := *d.cfg
	lowered.ResourceLimits.MaxMemoryMB = 6912
	d.ApplyConfig(&lowered)

	// Admission: refused, with both figures, not clamped.
	big := headContainerUnit("00000000-0000-4000-8000-0000000000a1", "leaf-grep", "ghcr.io/example/grep:1.2", 7000)
	ok, reason := d.canAccommodateWU(big)
	if ok {
		t.Fatal("a 7000 MB unit was admitted against a 6912 MB budget — it would run clamped below its declaration")
	}
	for _, want := range []string{"7000 MB", "6912 MB"} {
		if !strings.Contains(reason, want) {
			t.Errorf("admission reason lacks %q: %s", want, reason)
		}
	}
	if ok, why := d.canAccommodateWU(headContainerUnit("00000000-0000-4000-8000-0000000000a2", "leaf-grep", "ghcr.io/example/grep:1.2", 6912)); !ok {
		t.Errorf("a unit at exactly the budget was refused: %s", why)
	}

	// Arrival: given back before Prepare, flagged un-run.
	rc.mockClient.requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		return &lettucev1.RequestWorkUnitResponse{Assignments: []*lettucev1.WorkUnitAssignment{
			headContainerAssignment("00000000-0000-4000-8000-0000000000a1", "leaf-grep", "ghcr.io/example/grep:1.2", 7000),
		}}, nil
	}
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	got, err := f.fetchOne(context.Background())
	if err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if got != 0 || d.prefetchQueue.Len() != 0 {
		t.Errorf("fetchOne buffered %d unit(s) (queue %d), want 0: the unit cannot run at its declaration here", got, d.prefetchQueue.Len())
	}
	container.mu.Lock()
	prepared := container.prepareCalls
	container.mu.Unlock()
	if prepared != 0 {
		t.Errorf("Prepare called %d time(s) for a unit that was going to be given back", prepared)
	}
	req := rc.mockClient.lastAbandonReq
	if req == nil || req.WorkUnitId != "00000000-0000-4000-8000-0000000000a1" {
		t.Fatalf("no give-back reached the head: %+v", req)
	}
	if !req.UnrunGiveback {
		t.Errorf("the give-back was billed (unrun_giveback=false): %+v", req)
	}
	for _, want := range []string{"7000 MB", "6912 MB"} {
		if !strings.Contains(req.Reason, want) {
			t.Errorf("give-back reason lacks %q: %s", want, req.Reason)
		}
	}

	// Sweep: a unit already buffered when the limit dropped is given back on
	// the next buffer sweep, and its prepared work dir cleaned up.
	rc.mockClient.lastAbandonReq = nil
	prep := &runtime.PrepareResult{WorkDir: t.TempDir()}
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: big, Runtime: container, Prep: prep,
		Conn: d.multiClient.Servers()[0], FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.sweepBuffer()
	if n := d.prefetchQueue.Len(); n != 0 {
		t.Errorf("queue holds %d unit(s) after the sweep, want 0", n)
	}
	req = rc.mockClient.lastAbandonReq
	if req == nil || !req.UnrunGiveback || !strings.Contains(req.Reason, "6912 MB") {
		t.Errorf("swept unit not given back un-run with the budget figure: %+v", req)
	}
	container.mu.Lock()
	cleaned := container.cleanupCalls
	container.mu.Unlock()
	if cleaned != 1 {
		t.Errorf("Cleanup called %d time(s) for the swept unit, want 1", cleaned)
	}

	// A unit that fits stays.
	rc.mockClient.lastAbandonReq = nil
	fits := headContainerUnit("00000000-0000-4000-8000-0000000000a3", "leaf-grep", "ghcr.io/example/grep:1.2", 4096)
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: fits, Runtime: container, Prep: &runtime.PrepareResult{},
		Conn: d.multiClient.Servers()[0], FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.sweepBuffer()
	if d.prefetchQueue.Len() != 1 || rc.mockClient.lastAbandonReq != nil {
		t.Errorf("a fitting unit was swept: queue %d, abandon %+v", d.prefetchQueue.Len(), rc.mockClient.lastAbandonReq)
	}
}
