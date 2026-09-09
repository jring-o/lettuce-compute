package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-80 regression tests, lifecycle half: what the daemon does with an
// engine outage once it is classified. None of this existed before the fix
// (the re-detection loop exited for good once a runtime was registered,
// nothing unregistered a runtime, and detection never pinged), so these
// tests are compile-red on the pre-fix tree.

// tb80Engine is a container engine a test can switch off and on: the
// factory's detection seam reports it as a Podman engine, and its
// registration ping answers only while up. Each successful Build returns a
// NEW mock runtime, so a test can tell the recovered runtime from the one
// the outage removed.
type tb80Engine struct {
	mu     sync.Mutex
	up     bool
	builds int
}

func (e *tb80Engine) set(up bool) { e.mu.Lock(); e.up = up; e.mu.Unlock() }

// TestTB80_OutageUnregistersReturnsBufferedContainerUnitsAndRecovers is the
// whole cycle. A daemon with a registered container runtime holds two
// container units and one WASM unit in its buffer. The engine stops
// answering: the runtime leaves the registry, both container units are
// returned un-run (the WASM unit stays), every head is flagged for
// re-registration without CONTAINER, and ONE notice names the socket. The
// engine answers again: the next probe registers a fresh runtime, the notice
// resolves, the heads are flagged again, and the runtime-blocked verdict
// clears.
func TestTB80_OutageUnregistersReturnsBufferedContainerUnitsAndRecovers(t *testing.T) {
	hc := newTB80RecordingHead()
	rc := &reRegMockClient{mockClient: hc.mockClient,
		resp: &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}}
	head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	engine := newTB80EngineFactory(d, true)
	d.containerRedetectCh = make(chan struct{}, 1)
	d.prefetchQueue = NewPreFetchQueue(64, d.logger)
	d.slotManager = NewSlotManager(2, d.logger)
	d.fetcher = NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)

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
		buffered("00000000-0000-4000-8000-000000000002", "container", cr),
		buffered("00000000-0000-4000-8000-000000000003", "wasm", wasm),
	} {
		if err := d.prefetchQueue.Push(it); err != nil {
			t.Fatal(err)
		}
	}

	// The outage.
	engine.set(false)
	if !d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Fatal("NoteContainerEngineUnreachable = false for the registered runtime")
	}
	if d.runtimeRegistry.GetRuntime("container") != nil {
		t.Fatal("container runtime still registered after the outage")
	}
	if !d.containerEngineDown() {
		t.Error("containerEngineDown = false during the outage")
	}
	if !d.ContainerRedetectActive() {
		t.Error("ContainerRedetectActive = false during the outage: the loop must probe again")
	}
	abandons := hc.recorded()
	if len(abandons) != 2 {
		t.Fatalf("head received %d abandons for the buffered units, want 2 (the container units)", len(abandons))
	}
	for _, a := range abandons {
		if !a.UnrunGiveback {
			t.Errorf("buffered unit %s returned WITHOUT the un-run flag (reason %q)", a.WorkUnitId, a.Reason)
		}
		if !strings.Contains(a.Reason, "container engine unreachable") {
			t.Errorf("buffered unit %s reason %q does not name the outage", a.WorkUnitId, a.Reason)
		}
	}
	if got := d.prefetchQueue.Len(); got != 1 {
		t.Errorf("buffer holds %d unit(s) after the outage, want 1 (the WASM unit stays)", got)
	}
	d.readvertiseMu.Lock()
	pending := d.readvertisePending["server-a"]
	d.readvertiseMu.Unlock()
	if !pending {
		t.Error("the head was not flagged for re-registration without CONTAINER")
	}
	if got := d.advertisedRuntimesFor(head.Config); strings.Join(got, ",") != "WASM" {
		t.Errorf("advertisedRuntimesFor during the outage = %v, want [WASM] only (WASM is always trusted; CONTAINER is out of service)", got)
	}
	n, notice := countNoticesByCode(d.notices, "container_engine_unreachable")
	if n != 1 || notice.ResolvedAt != nil {
		t.Fatalf("container_engine_unreachable: n=%d resolved=%v, want one live notice", n, notice.ResolvedAt)
	}
	if n, rb := countNoticesByCode(d.notices, "runtime_blocked"); n != 1 || rb.ResolvedAt != nil {
		t.Errorf("runtime_blocked during the outage: n=%d %+v; the only leaf needs the engine", n, rb)
	}

	// A second report of the same outage — a unit that was running when the
	// engine died — changes nothing and raises nothing more.
	if d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Error("a second outage report for the same runtime claimed to unregister it again")
	}
	if n, notice := countNoticesByCode(d.notices, "container_engine_unreachable"); n != 1 || notice.Count != 1 {
		t.Errorf("a repeated report bumped the notice: n=%d count=%d", n, notice.Count)
	}

	// While down, a probe finds the engine but its ping fails: nothing is
	// registered and the detector says why.
	if d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime registered a runtime whose engine does not answer")
	}
	if le := d.ContainerDetectError(); !strings.Contains(le, "container engine unreachable") {
		t.Errorf("ContainerDetectError = %q, want the ping failure", le)
	}

	// Recovery.
	engine.set(true)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false after the engine came back")
	}
	fresh := d.runtimeRegistry.GetRuntime("container")
	if fresh == nil {
		t.Fatal("container runtime not registered after recovery")
	}
	if fresh == runtime.Runtime(cr) {
		t.Error("the recovered runtime is the one the outage removed; a fresh one is built for the answering engine")
	}
	if d.containerEngineDown() {
		t.Error("containerEngineDown still true after recovery")
	}
	if _, notice := countNoticesByCode(d.notices, "container_engine_unreachable"); notice.ResolvedAt == nil {
		t.Error("container_engine_unreachable not resolved after recovery")
	}
	if _, rb := countNoticesByCode(d.notices, "runtime_blocked"); rb.ResolvedAt == nil {
		t.Error("runtime_blocked not resolved after recovery")
	}
	if got := d.advertisedRuntimesFor(head.Config); strings.Join(got, ",") != "CONTAINER,WASM" {
		t.Errorf("advertisedRuntimesFor after recovery = %v, want [CONTAINER WASM]", got)
	}

	// A stale report from a unit of the OLD runtime must not remove the new one.
	if d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Error("a stale report for the replaced runtime unregistered the recovered one")
	}
	if d.runtimeRegistry.GetRuntime("container") != fresh {
		t.Error("the recovered runtime was removed by a stale report")
	}
}

// TestTB80_RedetectLoopOutlivesRegistration: the loop used to return the
// moment a runtime was registered. Started with a registered runtime it now
// sleeps, and an outage wakes it into probing; when the engine answers it
// registers a fresh runtime and goes back to sleep.
func TestTB80_RedetectLoopOutlivesRegistration(t *testing.T) {
	head := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	engine := newTB80EngineFactory(d, true)
	d.containerRedetectCh = make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.runContainerRedetect(ctx); close(done) }()

	// Registered at start: the loop must not have exited.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("runContainerRedetect returned with a runtime registered; an outage would never be re-probed")
	default:
	}

	// The engine dies and comes back before the loop's probe: the wake-up
	// probes at once and registers a fresh runtime.
	engine.set(false)
	if !d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Fatal("outage not recorded")
	}
	engine.set(true)
	deadline := time.Now().Add(3 * time.Second)
	for d.runtimeRegistry.GetRuntime("container") == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Fatal("the loop did not register a runtime after the engine came back")
	}
	if got := engine.buildCount(); got < 1 {
		t.Errorf("factory Build ran %d times after the outage, want at least 1", got)
	}
	select {
	case <-done:
		t.Fatal("runContainerRedetect returned after re-registering")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runContainerRedetect did not stop on context cancellation")
	}
}

// TestTB80_FactoryRefusesEngineWhoseSocketDoesNotAnswer is the Linux
// instance: detection finds a socket FILE (a Podman socket nobody listens
// on), and used to register CONTAINER on that alone — advertised to every
// head, failing every Prepare, 188 billed copies in 30 hours. A runtime is
// registered only after its engine answers a ping.
func TestTB80_FactoryRefusesEngineWhoseSocketDoesNotAnswer(t *testing.T) {
	head := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d := tb80Daemon(t, head, &mockRuntime{canHandle: true, name: "container"})
	d.runtimeRegistry = NewRuntimeRegistry() // no container runtime yet: start-up found nothing
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})
	engine := newTB80EngineFactory(d, false)

	rt, backend, err := d.containerFactory.Build(false)
	if rt != nil {
		t.Fatal("Build returned a runtime for an engine that does not answer")
	}
	if backend.Backend != runtime.BackendPodman {
		t.Errorf("Build backend = %v, want the Podman engine detection found", backend.Backend)
	}
	if err == nil || !runtime.IsEngineUnreachable(err) {
		t.Errorf("Build error = %v, want the registration ping's EngineUnreachableError", err)
	}
	if d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime registered a runtime whose socket nobody answers")
	}
	if got := d.advertisedRuntimesFor(head.Config); strings.Join(got, ",") != "WASM" {
		t.Errorf("advertisedRuntimesFor = %v, want [WASM] only: CONTAINER must not be advertised on a socket file alone", got)
	}

	engine.set(true)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false once the socket answers")
	}
}

// TestTB80_HandleSlotResultOutageUnregistersTheSlotRuntime: the execute
// path names the runtime the slot ran on, and the outage takes THAT runtime
// out of service — not one a probe registered since.
func TestTB80_HandleSlotResultOutageUnregistersTheSlotRuntime(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	newTB80EngineFactory(d, true)
	d.containerRedetectCh = make(chan struct{}, 1)
	d.slotManager = NewSlotManager(2, d.logger)

	wu := &runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000009", LeafID: "leaf-container", Runtime: "container",
		ExecutionSpec: runtime.ExecutionSpec{Image: "ghcr.io/example/img:tag"}}
	d.handleSlotResult(context.Background(), SlotResult{SlotID: 1, WU: wu, Conn: head, Runtime: cr,
		Err: fmt.Errorf("create container: %w", tb80Outage())})
	if d.runtimeRegistry.GetRuntime("container") != nil {
		t.Error("the execute-path outage did not take the runtime out of service")
	}
	if n, _ := countNoticesByCode(d.notices, "container_engine_unreachable"); n != 1 {
		t.Errorf("container_engine_unreachable notices = %d, want 1", n)
	}

	// The engine recovers and a fresh runtime is registered; a late result
	// from a unit of the OLD runtime must not remove it.
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("recovery probe did not register")
	}
	fresh := d.runtimeRegistry.GetRuntime("container")
	d.handleSlotResult(context.Background(), SlotResult{SlotID: 2, WU: wu, Conn: head, Runtime: cr,
		Err: fmt.Errorf("container wait: %w", tb80Outage())})
	if d.runtimeRegistry.GetRuntime("container") != fresh {
		t.Error("a stale execute-path outage from the old runtime removed the recovered one")
	}
}

// newTB80EngineFactory installs a test factory on d whose detection reports
// a Podman engine, whose construction returns a fresh mock container
// runtime, and whose registration ping answers only while the engine is up.
func newTB80EngineFactory(d *Daemon, up bool) *tb80Engine {
	e := &tb80Engine{up: up}
	f := NewContainerRuntimeFactoryForTest(d.cfg, d.logger,
		func(runtime.ContainerBackend) runtime.BackendInfo {
			return runtime.BackendInfo{Backend: runtime.BackendPodman, Engine: "podman", Version: "5.3.1",
				SocketPath: "/var/folders/82/T/podman/podman-machine-default-api.sock"}
		},
		func(runtime.BackendInfo) (runtime.Runtime, error) {
			e.mu.Lock()
			e.builds++
			e.mu.Unlock()
			return &mockRuntime{canHandle: true, name: "container"}, nil
		})
	f.SetEnginePingForTest(func(runtime.Runtime) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.up {
			return nil
		}
		return &runtime.EngineUnreachableError{Backend: runtime.BackendPodman,
			Socket: "/var/folders/82/T/podman/podman-machine-default-api.sock",
			Err:    errors.New("docker ping: Cannot connect to the Docker daemon")}
	})
	d.containerFactory = f
	return e
}

func (e *tb80Engine) buildCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.builds
}
