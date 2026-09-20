package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-88, the fix's own surface: the hold and how it is reported, released
// and reflected in notices. None of this existed before the fix, so these
// tests are compile-red on the pre-fix tree; the behavioural red half is
// TestTB88_LoopLeavesAMachineTheVolunteerStoppedByHand.

// TestTB88_HandStopIsHeldAndReportedOnce: after the terminal stop the daemon
// holds the machine (source "outside"), the outage notice is superseded by
// one WARN container_machine_stopped notice — raised once across the ticks,
// not once a minute — and "Check again now" (a request: the forced probe)
// releases the hold and starts the machine.
func TestTB88_HandStopIsHeldAndReportedOnce(t *testing.T) {
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	cr := tb88BootAndRegister(t, d, p)
	p.set("stopped")
	d.NoteContainerEngineUnreachable(cr, tb80Outage())
	if n, _ := countNoticesByCode(d.notices, "container_engine_unreachable"); n != 1 {
		t.Fatalf("premise: the outage raised %d unreachable notices, want 1", n)
	}

	for i := 0; i < 3; i++ {
		d.RedetectContainerRuntime(context.Background(), false)
	}
	held, source := d.PodmanMachineHeldStopped()
	if !held || source != MachineStopOutside {
		t.Errorf("PodmanMachineHeldStopped = %v/%q, want true/outside", held, source)
	}
	if le := d.ContainerDetectError(); !strings.Contains(le, "left stopped") {
		t.Errorf("ContainerDetectError = %q, want the hold named", le)
	}
	n, notice := countNoticesByCode(d.notices, "container_machine_stopped")
	if n != 1 || notice.ResolvedAt != nil || notice.Count != 1 {
		t.Errorf("container_machine_stopped: n=%d count=%d resolved=%v, want one live notice raised once across three ticks", n, notice.Count, notice.ResolvedAt)
	}
	if notice.Level != NoticeWarn {
		t.Errorf("a stop from outside the app is level %q, want warn (nothing on the app's screens confirmed it)", notice.Level)
	}
	if !strings.Contains(notice.Message, "will not start it by itself") || !strings.Contains(notice.Message, "outside Lettuce") {
		t.Errorf("notice message %q does not say the machine is left stopped, or where the stop came from", notice.Message)
	}
	if _, un := countNoticesByCode(d.notices, "container_engine_unreachable"); un.ResolvedAt == nil {
		t.Error("the outage notice was not resolved once the hold explained it")
	}
	if !d.ContainerRedetectActive() {
		t.Error("the loop must keep probing while held (a hand-started machine is picked up within a minute)")
	}

	// "Check again now": a person's request forces the probe, which releases
	// the hold and runs the bring-up.
	if err := d.RequestContainerRedetect(); err != nil {
		t.Fatalf("RequestContainerRedetect: %v", err)
	}
	if !d.RedetectContainerRuntime(context.Background(), d.containerRedetectForce.Swap(false)) {
		t.Fatal("the forced probe did not register the runtime")
	}
	if got := p.startCount(); got != 2 {
		t.Errorf("podman machine start ran %d times, want 2 (boot + the request)", got)
	}
	if held, _ := d.PodmanMachineHeldStopped(); held {
		t.Error("the hold survived the registration")
	}
	if _, notice := countNoticesByCode(d.notices, "container_machine_stopped"); notice.ResolvedAt == nil {
		t.Error("container_machine_stopped not resolved after the machine came back")
	}
}

// TestTB88_AppStopRetiresTheRuntimeAtOnceAndStartReleasesTheHold is the
// bridge's sequence for the Stop Machine button: NotePodmanMachineStopped
// before the stop begins takes the runtime out of service now — buffered
// container units go back un-run, the head is flagged for re-registration —
// with no outage WARN notice (the volunteer's own action, an Info notice);
// the loop holds the machine (source "app"); the Start button's release plus
// its request bring the machine back.
func TestTB88_AppStopRetiresTheRuntimeAtOnceAndStartReleasesTheHold(t *testing.T) {
	hc := newTB80RecordingHead()
	rc := &reRegMockClient{mockClient: hc.mockClient,
		resp: &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}}
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d.multiClient = NewMultiServerClient([]*ServerConnection{head}, d.logger)
	d.prefetchQueue = NewPreFetchQueue(64, d.logger)
	d.slotManager = NewSlotManager(2, d.logger)
	d.fetcher = NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	cr := tb88BootAndRegister(t, d, p)

	wasm := d.runtimeRegistry.GetRuntime("wasm")
	buffered := func(id, rtName string, rt runtime.Runtime) *PreFetchItem {
		wu := &runtime.WorkUnit{ID: id, LeafID: "leaf-" + rtName, Runtime: rtName}
		if rtName == "container" {
			wu.ExecutionSpec = runtime.ExecutionSpec{Image: "ghcr.io/example/img:tag"}
		}
		return &PreFetchItem{WU: wu, WUResp: &lettucev1.WorkUnitAssignment{}, Prep: &runtime.PrepareResult{WorkDir: t.TempDir()},
			Runtime: rt, Conn: head, FetchedAt: time.Now()}
	}
	for _, it := range []*PreFetchItem{
		buffered("00000000-0000-4000-8000-000000000001", "container", cr),
		buffered("00000000-0000-4000-8000-000000000002", "wasm", wasm),
	} {
		if err := d.prefetchQueue.Push(it); err != nil {
			t.Fatal(err)
		}
	}

	// The Stop button: the bridge notes the stop, then runs `podman machine
	// stop` asynchronously.
	d.NotePodmanMachineStopped(MachineStopFromApp)
	if d.runtimeRegistry.GetRuntime("container") != nil {
		t.Fatal("the container runtime is still registered after the app's stop was noted")
	}
	if held, source := d.PodmanMachineHeldStopped(); !held || source != MachineStopFromApp {
		t.Errorf("PodmanMachineHeldStopped = %v/%q, want true/app", held, source)
	}
	abandons := hc.recorded()
	if len(abandons) != 1 || !abandons[0].UnrunGiveback {
		t.Fatalf("buffered container unit: %d abandons (%+v), want the one container unit returned un-run", len(abandons), abandons)
	}
	if got := d.prefetchQueue.Len(); got != 1 {
		t.Errorf("buffer holds %d unit(s), want 1 (the WASM unit stays)", got)
	}
	d.readvertiseMu.Lock()
	pending := d.readvertisePending["server-a"]
	d.readvertiseMu.Unlock()
	if !pending {
		t.Error("the head was not flagged for re-registration without CONTAINER")
	}
	if n, _ := countNoticesByCode(d.notices, "container_engine_unreachable"); n != 0 {
		t.Errorf("the app's own stop raised %d container_engine_unreachable notice(s), want none", n)
	}

	// The stop completes; the loop's probe notes the hold.
	p.set("stopped")
	for i := 0; i < 3; i++ {
		if d.RedetectContainerRuntime(context.Background(), false) {
			t.Fatalf("probe %d registered a runtime on the stopped machine", i+1)
		}
	}
	if got := p.startCount(); got != 1 {
		t.Errorf("podman machine start ran %d times after the app's stop, want 1 (the boot)", got)
	}
	n, notice := countNoticesByCode(d.notices, "container_machine_stopped")
	if n != 1 || notice.Level != NoticeInfo || !strings.Contains(notice.Message, "from the app") {
		t.Errorf("container_machine_stopped: n=%d level=%q message=%q; want one Info notice naming the app", n, notice.Level, notice.Message)
	}

	// The Start button: the bridge releases the hold, starts the machine and
	// requests a probe.
	d.ReleasePodmanMachineHold()
	p.set("running")
	if err := d.RequestContainerRedetect(); err != nil {
		t.Fatalf("RequestContainerRedetect: %v", err)
	}
	if !d.RedetectContainerRuntime(context.Background(), d.containerRedetectForce.Swap(false)) {
		t.Fatal("the probe after the Start button did not register the runtime")
	}
	if held, _ := d.PodmanMachineHeldStopped(); held {
		t.Error("held after the Start button")
	}
	if got := d.advertisedRuntimesFor(head.Config); strings.Join(got, ",") != "CONTAINER,WASM" {
		t.Errorf("advertisedRuntimesFor after the restart = %v, want [CONTAINER WASM]", got)
	}
}

// TestTB88_RunLoopWakesUnforcedForAStopAndForcedForARequest drives the
// daemon's own loop: the outage wake after a hand stop must probe without
// the bring-up (the machine stays stopped), and a request wake must force it
// (the machine starts).
func TestTB88_RunLoopWakesUnforcedForAStopAndForcedForARequest(t *testing.T) {
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	cr := tb88BootAndRegister(t, d, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.runContainerRedetect(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)

	p.set("stopped")
	if !d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Fatal("outage not recorded")
	}
	// The wake probes at once; give it time, then check nothing started.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if held, _ := d.PodmanMachineHeldStopped(); held {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if held, _ := d.PodmanMachineHeldStopped(); !held {
		t.Fatal("the loop's probe after the outage did not hold the stopped machine")
	}
	if got := p.startCount(); got != 1 {
		t.Fatalf("the loop started the machine after the volunteer stopped it (%d starts, want 1)", got)
	}

	// "Check again now".
	if err := d.RequestContainerRedetect(); err != nil {
		t.Fatalf("RequestContainerRedetect: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for d.runtimeRegistry.GetRuntime("container") == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Fatal("the request did not bring the machine up and register the runtime")
	}
	if got := p.startCount(); got != 2 {
		t.Errorf("podman machine start ran %d times, want 2 (boot + the request)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runContainerRedetect did not stop on context cancellation")
	}
}
