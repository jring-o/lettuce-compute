package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-63 regression tests, daemon half.
//
// The reproduction: a 16 GB MacBook with max_memory_mb 8192 and a Podman
// machine of 2 GiB. The client advertised 8192, the head's dispatch gate —
// which compares a leaf's max_memory_mb against the ADVERTISED figure on every
// poll — sent it 7000 MB GREP units, and the guest kernel killed each one at
// model load (exit 137, 21–56 s after start), billing the unit's copy budget
// every time. Nothing in the client compared the machine's memory with what it
// advertised, booked or enforced. Now the engine's VM memory clips the budget
// at the one place all three read it (MemoryBudgetMB), the advertisement is
// lowered to it before any head is told, a 137 says "killed for memory" with
// both figures, and the volunteer is told once, with the remedy.

// tb63Factory is a container-engine factory whose engine is up and whose VM
// has vmMB of memory (0 = not a VM, or unknown). The construction seam returns
// a real ContainerRuntime with no client, so the ceiling Build applies can be
// read back.
func tb63Factory(t *testing.T, d *Daemon, vmMB int) *ContainerRuntimeFactory {
	t.Helper()
	f := NewContainerRuntimeFactoryForTest(d.cfg, d.logger,
		func(runtime.ContainerBackend) runtime.BackendInfo {
			return runtime.BackendInfo{Backend: runtime.BackendPodman, Engine: "podman", Version: "5.8"}
		},
		func(runtime.BackendInfo) (runtime.Runtime, error) {
			return runtime.NewContainerRuntimeWithClient(t.TempDir(), d.logger, nil), nil
		})
	f.SetEngineMemoryProbeForTest(func(runtime.Runtime) int { return vmMB })
	return f
}

// tb63Daemon is the reproduction's host: max_memory_mb 8192 advertised, a head
// trusted for CONTAINER with one 7000 MB container leaf, WASM registered, and
// no container runtime yet. rc captures the re-registration the daemon sends
// once the runtime appears.
func tb63Daemon(t *testing.T) (*Daemon, *reRegMockClient, *bytes.Buffer) {
	t.Helper()
	mc, _ := tb49CountingHead()
	rc := &reRegMockClient{mockClient: mc,
		resp: &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}}
	head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d.notices = NewNoticeLog()
	d.leafFailures = newLeafFailureTracker(time.Now)
	d.cfg.Servers = []config.ServerConfig{head.Config}
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	d.cachedHW = &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: 4, MemoryTotalMb: 16384, MaxMemoryMb: 8192, Os: "darwin"}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{{ID: "leaf-grep", Slug: "grep-f14", Name: "GREP f14", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/grep:1.2", MaxMemoryMB: 7000}}},
		DefaultWeights: map[string]int{"grep-f14": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"grep-f14": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})
	return d, rc, &buf
}

// TestTB63_EngineVMClipsBudgetCeilingAndAdvertisement is the reproduction
// with its outcome inverted. The engine appears with a 2 GiB VM: the runtime's
// ceiling, the daemon's budget and the advertised max_memory_mb all become
// 1536 MB (2048 less the 512 MB headroom), the re-registration and the next
// poll carry 1536, the original advertisement object is left untouched (the
// fetcher may be handing it to gRPC), and the volunteer gets one WARN and one
// notice naming both figures and the remedy. Pre-fix: every figure stayed
// 8192 and the head kept sending 7000 MB units.
func TestTB63_EngineVMClipsBudgetCeilingAndAdvertisement(t *testing.T) {
	d, rc, buf := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	before := d.AdvertisedHardware()

	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		t.Fatal("no container runtime registered")
	}
	if cr.EngineMemoryMB() != 2048 {
		t.Errorf("runtime EngineMemoryMB = %d, want 2048", cr.EngineMemoryMB())
	}
	if cr.MemoryCeilingMB() != 1536 {
		t.Errorf("runtime memory ceiling = %d MB, want 1536 (the VM's 2048 less %d headroom), not the 8192 configured", cr.MemoryCeilingMB(), runtime.ContainerVMHeadroomMB)
	}
	if got := d.MemoryBudgetMB(); got != 1536 {
		t.Errorf("MemoryBudgetMB = %d, want 1536", got)
	}
	if got := d.ContainerVMMemoryMB(); got != 2048 {
		t.Errorf("ContainerVMMemoryMB = %d, want 2048", got)
	}
	if !d.MemoryLimitedByVM() {
		t.Error("MemoryLimitedByVM = false; the VM is below the configuration")
	}
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 1536 {
		t.Errorf("advertised MaxMemoryMb = %d, want 1536", got)
	}
	if before.MaxMemoryMb != 8192 {
		t.Errorf("the previous advertisement object was written in place (MaxMemoryMb = %d); it must be replaced by a copy", before.MaxMemoryMb)
	}
	if got := d.AdvertisedHardware().MemoryTotalMb; got != 16384 {
		t.Errorf("MemoryTotalMb = %d after the clip, want 16384 (only the budget changes)", got)
	}

	// The notice and the WARN: once, both figures, the remedy.
	n, notice := countNoticesByCode(d.notices, "container_memory_clipped")
	if n != 1 || notice.Count != 1 {
		t.Fatalf("container_memory_clipped notices = %d (count %d), want exactly one", n, notice.Count)
	}
	for _, want := range []string{"1536 MB", "2048 MB", "8192 MB", "podman machine set --memory"} {
		if !strings.Contains(notice.Message, want) {
			t.Errorf("notice lacks %q: %s", want, notice.Message)
		}
	}
	if notice.ResolvedAt != nil {
		t.Errorf("notice resolved while the VM still clips: %+v", notice)
	}
	if c := strings.Count(buf.String(), "container engine's VM is smaller than the memory limit"); c != 1 {
		t.Errorf("clip WARN logged %d time(s), want exactly 1; log:\n%s", c, buf.String())
	}

	// The head is told 1536 on the re-registration and on every poll.
	var mu sync.Mutex
	var polled []int32
	rc.mockClient.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		polled = append(polled, req.GetCurrentAvailable().GetMaxMemoryMb())
		mu.Unlock()
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	saved := d.logger
	runFetcherFor(d, 200*time.Millisecond)
	d.logger = saved
	if rc.lastReq == nil {
		t.Fatal("no re-registration reached the head after the runtime appeared")
	}
	if got := rc.lastReq.GetHardware().GetMaxMemoryMb(); got != 1536 {
		t.Errorf("re-registration advertised max_memory_mb %d, want 1536", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(polled) == 0 {
		t.Fatal("no poll reached the head")
	}
	for _, mb := range polled {
		if mb != 1536 {
			t.Fatalf("a poll carried CurrentAvailable.max_memory_mb %d, want 1536 on every poll: %v", mb, polled)
		}
	}
}

// TestTB63_EngineSharingHostRAMDoesNotClip: on Linux the engine shares the
// host's RAM and reports no VM figure; the configuration stands, nothing is
// advertised differently, and no notice is raised.
func TestTB63_EngineSharingHostRAMDoesNotClip(t *testing.T) {
	d, _, buf := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 0)

	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	cr := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if cr.MemoryCeilingMB() != 8192 {
		t.Errorf("ceiling = %d, want the configured 8192", cr.MemoryCeilingMB())
	}
	if d.MemoryBudgetMB() != 8192 || d.ContainerVMMemoryMB() != 0 || d.MemoryLimitedByVM() {
		t.Errorf("budget %d, vm %d, limited %v; want 8192 / 0 / false", d.MemoryBudgetMB(), d.ContainerVMMemoryMB(), d.MemoryLimitedByVM())
	}
	if got := d.AdvertisedHardware().MaxMemoryMb; got != 8192 {
		t.Errorf("advertised MaxMemoryMb = %d, want 8192 unchanged", got)
	}
	if n, _ := countNoticesByCode(d.notices, "container_memory_clipped"); n != 0 {
		t.Errorf("container_memory_clipped raised %d time(s) with no VM", n)
	}
	if strings.Contains(buf.String(), "VM is smaller") {
		t.Errorf("clip WARN logged with no VM:\n%s", buf.String())
	}
}

// TestTB63_VMLargeEnoughKeepsTheConfiguration: a VM that can honor the
// configuration (the tester after resizing to 8 GB with 4096 configured, or a
// machine Lettuce created at configuration plus headroom) clips nothing and
// raises no notice — the VM is reported, the budget is the configuration.
func TestTB63_VMLargeEnoughKeepsTheConfiguration(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 8192+runtime.ContainerVMHeadroomMB)

	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	if d.MemoryBudgetMB() != 8192 || d.MemoryLimitedByVM() {
		t.Errorf("budget %d, limited %v; want 8192 / false", d.MemoryBudgetMB(), d.MemoryLimitedByVM())
	}
	if got := d.ContainerVMMemoryMB(); got != 8192+runtime.ContainerVMHeadroomMB {
		t.Errorf("ContainerVMMemoryMB = %d, want the VM's figure reported even when it does not clip", got)
	}
	if n, _ := countNoticesByCode(d.notices, "container_memory_clipped"); n != 0 {
		t.Errorf("container_memory_clipped raised %d time(s) though the VM honors the configuration", n)
	}
}

// TestTB63_AdmissionBooksAgainstTheClippedBudget: the admission budget is the
// clipped figure, so two units cannot be admitted against the configured 8192
// when the VM only holds 1536 — the same denominator the heads were told.
func TestTB63_AdmissionBooksAgainstTheClippedBudget(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = defaultFreeSystemMemoryMB }()
	d.slotManager = NewSlotManager(2, d.logger)

	one := &runtime.WorkUnit{ID: "wu-1", LeafID: "leaf-grep", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}
	two := &runtime.WorkUnit{ID: "wu-2", LeafID: "leaf-grep", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}
	if !d.mayDelayAdmission(one, two) {
		t.Error("mayDelayAdmission = false: two 1024 MB units fit the configured 8192 but not the VM's 1536 budget")
	}
	if ok, reason := d.canAccommodateWU(one); !ok {
		t.Errorf("a 1024 MB unit refused against a 1536 MB budget: %s", reason)
	}
}

// TestTB63_Exit137NamesTheVMShortfall: a container unit that declares more
// than the VM can hold and dies with 137 is abandoned with a reason that says
// killed for memory and names the declaration, the budget and the VM; the
// leaf-failing notice carries the same text. Pre-fix the head's log said
// "non-zero exit code 137; output: …" and the volunteer was told to report it
// to the head's operator.
func TestTB63_Exit137NamesTheVMShortfall(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	mc := &mockClient{}
	wu := &runtime.WorkUnit{ID: "wu-137", LeafID: "leaf-grep", Runtime: "container",
		ExecutionSpec: runtime.ExecutionSpec{Image: "ghcr.io/example/grep:1.2", MaxMemoryMB: 7000}}
	for i := 0; i < leafFailurePauseThreshold; i++ {
		d.handleSlotResult(context.Background(), SlotResult{
			WU: wu, Conn: handleSlotResultTestConn(mc),
			Result:         &runtime.ExecutionResult{ExitCode: 137},
			FailureLogTail: "[lettuce-student] providers=['CPUExecutionProvider']",
		})
	}
	if mc.lastAbandonReq == nil {
		t.Fatal("no abandon request recorded")
	}
	reason := mc.lastAbandonReq.Reason
	for _, want := range []string{"non-zero exit code 137", "killed for memory", "7000 MB", "1536 MB", "2048 MB", "providers="} {
		if !strings.Contains(reason, want) {
			t.Errorf("abandon reason lacks %q: %s", want, reason)
		}
	}
	_, notice := countNoticesByCode(d.notices, "leaf_failing")
	if !strings.Contains(notice.Message, "killed for memory") || !strings.Contains(notice.Message, "2048 MB") {
		t.Errorf("leaf_failing notice does not carry the memory diagnosis: %s", notice.Message)
	}

	// A 137 within the budget is still explained as a kill, at its own limit;
	// any other exit code is reported as before.
	mc = &mockClient{}
	small := &runtime.WorkUnit{ID: "wu-small", LeafID: "leaf-grep", Runtime: "container",
		ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}
	d.handleSlotResult(context.Background(), SlotResult{WU: small, Conn: handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 137}})
	if got := mc.lastAbandonReq.Reason; !strings.Contains(got, "usually out of memory at its 1024 MB limit") {
		t.Errorf("in-budget 137 reason = %q, want the out-of-memory hint at the unit's own limit", got)
	}
	mc = &mockClient{}
	d.handleSlotResult(context.Background(), SlotResult{WU: small, Conn: handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 2}})
	if got := mc.lastAbandonReq.Reason; got != "non-zero exit code 2" {
		t.Errorf("exit 2 reason = %q, want the bare reason", got)
	}
}

// TestTB63_ConfigChangeRefreshesTheNotice: lowering the limit to the budget
// resolves the notice; raising it above the VM again re-raises it with the
// new figure — a raised limit does not raise what the VM can hold.
func TestTB63_ConfigChangeRefreshesTheNotice(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	if n, _ := countNoticesByCode(d.notices, "container_memory_clipped"); n != 1 {
		t.Fatalf("notices after the clip = %d, want 1", n)
	}

	lowered := *d.cfg
	lowered.ResourceLimits.MaxMemoryMB = 1536
	d.ApplyConfig(&lowered)
	if d.MemoryLimitedByVM() {
		t.Error("MemoryLimitedByVM = true with the limit at the budget")
	}
	if _, notice := countNoticesByCode(d.notices, "container_memory_clipped"); notice.ResolvedAt == nil {
		t.Errorf("notice not resolved after the limit was lowered to the budget: %+v", notice)
	}

	raised := *d.cfg
	raised.ResourceLimits.MaxMemoryMB = 12288
	d.ApplyConfig(&raised)
	n, notice := countNoticesByCode(d.notices, "container_memory_clipped")
	if n != 1 || notice.ResolvedAt != nil || notice.Count != 2 {
		t.Errorf("after raising the limit: n=%d resolved=%v count=%d, want the one notice reopened with count 2", n, notice.ResolvedAt, notice.Count)
	}
	if !strings.Contains(notice.Message, "12288 MB") {
		t.Errorf("reopened notice does not name the new limit: %s", notice.Message)
	}
}

// TestTB63_ClampAdvertisedMemory: the start-up clamp lowers an advertisement
// only when a built runtime's budget is below it, and reports whether it did.
func TestTB63_ClampAdvertisedMemory(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	f := tb63Factory(t, d, 2048)
	hw := &lettucev1.HardwareCapabilities{MaxMemoryMb: 8192}
	if f.ClampAdvertisedMemory(hw) {
		t.Error("clamped before any runtime was built")
	}
	if _, _, err := f.Build(false); err != nil {
		t.Fatal(err)
	}
	if !f.ClampAdvertisedMemory(hw) || hw.MaxMemoryMb != 1536 {
		t.Errorf("after Build: clamp reported %v, MaxMemoryMb %d; want true / 1536", f.ClampAdvertisedMemory(hw), hw.MaxMemoryMb)
	}
	low := &lettucev1.HardwareCapabilities{MaxMemoryMb: 1024}
	if f.ClampAdvertisedMemory(low) || low.MaxMemoryMb != 1024 {
		t.Errorf("an advertisement below the budget was changed: %d", low.MaxMemoryMb)
	}
	var nilFactory *ContainerRuntimeFactory
	if nilFactory.ClampAdvertisedMemory(hw) {
		t.Error("a nil factory clamped")
	}
}

// TestTB63_MachineSizeForAddsTheHeadroom: a machine Lettuce creates is sized
// at the configured memory plus the headroom, so the budget after the clip is
// the configured figure; the old floors still apply.
func TestTB63_MachineSizeForAddsTheHeadroom(t *testing.T) {
	cpus, mem, disk := MachineSizeFor(config.ResourceLimits{MaxCPUCores: 4, MaxMemoryMB: 8192, MaxDiskGB: 50})
	if cpus != 4 || mem != 8192+runtime.ContainerVMHeadroomMB || disk != 50 {
		t.Errorf("MachineSizeFor = %d/%d/%d, want 4/%d/50", cpus, mem, disk, 8192+runtime.ContainerVMHeadroomMB)
	}
	if got := runtime.ContainerMemoryBudgetMB(8192, mem); got != 8192 {
		t.Errorf("a machine sized by MachineSizeFor yields a budget of %d, want the configured 8192", got)
	}
	cpus, mem, disk = MachineSizeFor(config.ResourceLimits{})
	if cpus != 2 || mem != 4096+runtime.ContainerVMHeadroomMB || disk != 20 {
		t.Errorf("MachineSizeFor(zero) = %d/%d/%d, want the floors 2/%d/20", cpus, mem, disk, 4096+runtime.ContainerVMHeadroomMB)
	}
}
