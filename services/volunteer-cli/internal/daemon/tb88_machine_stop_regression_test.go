package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-88 regression: a Podman machine the volunteer stops — `podman machine
// stop` in a terminal, or the app's Stop Machine button — was started again
// by the daemon within a minute. The stop took the engine off the air, the
// outage path (TB-80) unregistered the runtime and woke the re-detection
// loop, and the loop's probe ran the machine bring-up exactly as it does for
// a machine that has never been started. The bring-up at daemon start is a
// feature (TQ-47); applied to a machine that was running under this daemon
// and then deliberately stopped, it undid the volunteer's decision.
//
// This file is written against the pre-fix API (plus the platform seam in
// tb88_helpers_test.go), so its first test fails on the pre-fix tree for the
// reason filed: `podman machine start` runs a second time after the stop.

// tb88Podman is a scripted `podman`: the machine's state, and counts of the
// starts and stops the daemon issues. `info` (WaitForReady's probe) answers
// while the machine is running.
type tb88Podman struct {
	mu     sync.Mutex
	state  string // running | stopped
	starts int
	stops  int
	// socketDead makes the engine's ping fail while the machine reads
	// running: the applehv stale-socket state (TQ-58).
	socketDead bool
}

func (p *tb88Podman) set(state string) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

func (p *tb88Podman) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

func (p *tb88Podman) exec(name string, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "machine" && args[1] == "inspect":
		return []byte(fmt.Sprintf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":1366},"ConnectionInfo":{"PodmanSocket":{"Path":"/tmp/podman-machine-default-api.sock"}}}]`, p.state)), nil
	case len(args) >= 2 && args[0] == "machine" && args[1] == "start":
		p.starts++
		p.state = "running"
		return []byte("Machine started\n"), nil
	case len(args) >= 2 && args[0] == "machine" && args[1] == "stop":
		p.stops++
		p.state = "stopped"
		return []byte("Machine stopped\n"), nil
	case len(args) >= 1 && args[0] == "info":
		if p.state == "running" {
			return []byte("true"), nil
		}
		return nil, errors.New("connection refused")
	}
	return nil, fmt.Errorf("unexpected podman command %v", args)
}

// ping is the factory's registration ping: the engine answers while the
// machine is running and its socket is alive.
func (p *tb88Podman) ping(runtime.Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "running" && !p.socketDead {
		return nil
	}
	return &runtime.EngineUnreachableError{Backend: runtime.BackendPodman,
		Socket: "/tmp/podman-machine-default-api.sock", Err: errors.New("dial unix: connection refused")}
}

// tb88Daemon is a daemon with a head trusted for CONTAINER, only WASM
// registered, and a production-shaped factory: detection finds a Podman
// binary (so the factory creates a real machine manager, driven through the
// scripted podman), construction returns a mock container runtime, and the
// ping is the podman's. The machine starts out stopped, as at a boot.
func tb88Daemon(t *testing.T, p *tb88Podman) *Daemon {
	t.Helper()
	tb88OnMachinePlatform(t)
	orig := runtime.CommandExecutor
	runtime.CommandExecutor = p.exec
	t.Cleanup(func() { runtime.CommandExecutor = orig })

	head := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	d.runtimeRegistry.Unregister(cr)
	d.containerRedetectCh = make(chan struct{}, 1)

	f := NewContainerRuntimeFactoryForTest(d.cfg, d.logger,
		func(runtime.ContainerBackend) runtime.BackendInfo {
			return runtime.BackendInfo{Backend: runtime.BackendPodman, Engine: "podman", Version: "5.8.6",
				BinaryPath: "/opt/podman/bin/podman", SocketPath: "/tmp/podman-machine-default-api.sock"}
		},
		func(runtime.BackendInfo) (runtime.Runtime, error) {
			return &mockRuntime{canHandle: true, name: "container"}, nil
		})
	f.SetEnginePingForTest(p.ping)
	d.containerFactory = f
	return d
}

// tb88BootAndRegister runs the start-up bring-up (a forced Build, as
// buildRuntimeRegistry does): the stopped machine is started by the daemon
// and the runtime registered.
func tb88BootAndRegister(t *testing.T, d *Daemon, p *tb88Podman) runtime.Runtime {
	t.Helper()
	if !d.RedetectContainerRuntime(context.Background(), true) {
		t.Fatal("start-up bring-up did not register the container runtime")
	}
	if got := p.startCount(); got != 1 {
		t.Fatalf("start-up ran podman machine start %d times, want 1", got)
	}
	cr := d.runtimeRegistry.GetRuntime("container")
	if cr == nil {
		t.Fatal("no container runtime registered after start-up")
	}
	return cr
}

// TestTB88_LoopLeavesAMachineTheVolunteerStoppedByHand is the tester's
// terminal sequence: Lettuce started the machine at boot; the volunteer runs
// `podman machine stop`; the next engine call fails and the outage wakes the
// loop. The loop's probes must not start the machine again. Pre-fix: the
// first probe after the outage ran the bring-up and `podman machine start`
// ran a second time ("but just restarts it in background").
func TestTB88_LoopLeavesAMachineTheVolunteerStoppedByHand(t *testing.T) {
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	cr := tb88BootAndRegister(t, d, p)

	// `podman machine stop` by hand: the machine is down, the engine's next
	// call fails.
	p.set("stopped")
	if !d.NoteContainerEngineUnreachable(cr, tb80Outage()) {
		t.Fatal("the outage did not take the runtime out of service")
	}

	// The loop's probe after the outage (not forced), and two more ticks.
	for i := 0; i < 3; i++ {
		if d.RedetectContainerRuntime(context.Background(), false) {
			t.Fatalf("probe %d registered a container runtime while the machine is stopped", i+1)
		}
	}
	if got := p.startCount(); got != 1 {
		t.Errorf("podman machine start ran %d times, want 1 (the boot only): the loop restarted a machine the volunteer had stopped", got)
	}
	if p.state != "stopped" {
		t.Errorf("machine state after the probes = %s, want stopped", p.state)
	}
}

// TestTB88_HandStartedMachineIsPickedUpWithoutABringUp: a held machine the
// volunteer starts again by hand is connected to on the next probe — the
// hold is released by the registration, and no bring-up ran.
func TestTB88_HandStartedMachineIsPickedUpWithoutABringUp(t *testing.T) {
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	cr := tb88BootAndRegister(t, d, p)
	p.set("stopped")
	d.NoteContainerEngineUnreachable(cr, tb80Outage())
	if d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("probe registered a runtime on a stopped machine")
	}

	// `podman machine start` by hand.
	p.set("running")
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("the probe did not register the runtime on the hand-started machine")
	}
	if got := p.startCount(); got != 1 {
		t.Errorf("podman machine start ran %d times, want 1: the daemon must connect to a hand-started machine, not start it", got)
	}
	if d.containerEngineDown() {
		t.Error("the outage is still recorded after the machine came back")
	}
}

// TestTB88_StaleSocketMachineStillGetsTheBringUp is the boundary: a machine
// that reads running while its API socket is dead (the applehv family,
// TQ-58) was not stopped by anyone, so the probe still runs the bring-up as
// before and the machine is not held.
func TestTB88_StaleSocketMachineStillGetsTheBringUp(t *testing.T) {
	p := &tb88Podman{state: "stopped"}
	d := tb88Daemon(t, p)
	cr := tb88BootAndRegister(t, d, p)

	p.mu.Lock()
	p.socketDead = true
	p.mu.Unlock()
	d.NoteContainerEngineUnreachable(cr, tb80Outage())
	if d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("probe registered a runtime behind a dead socket")
	}
	if le := d.ContainerDetectError(); !strings.Contains(le, "connection refused") {
		t.Errorf("ContainerDetectError = %q, want the ping failure: the bring-up ran (the machine reads running, so it was a no-op) and the socket was probed", le)
	}
	if p.state != "running" {
		t.Errorf("machine state = %s, want running (nothing stopped it)", p.state)
	}
	// Once the socket answers, the next probe registers.
	p.mu.Lock()
	p.socketDead = false
	p.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for d.runtimeRegistry.GetRuntime("container") == nil && time.Now().Before(deadline) {
		d.RedetectContainerRuntime(context.Background(), false)
		time.Sleep(10 * time.Millisecond)
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Error("the runtime was not registered once the socket answered")
	}
}
