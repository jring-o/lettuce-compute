package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// bootingPodman is a scripted podman whose machine, once the daemon has a
// runtime on it, is found booting: `inspect` reads "starting" until the
// socket is first probed, and the machine is up from then on. No start is
// expected of the daemon.
type bootingPodman struct {
	mu     sync.Mutex
	state  string // running | stopped | starting
	starts int
}

func (p *bootingPodman) exec(name string, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "machine" && args[1] == "inspect":
		return []byte(fmt.Sprintf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":1366},"ConnectionInfo":{"PodmanSocket":{"Path":"/tmp/podman-machine-default-api.sock"}}}]`, p.state)), nil
	case len(args) >= 2 && args[0] == "machine" && args[1] == "start":
		p.starts++
		p.state = "running"
		return []byte("Machine started\n"), nil
	case len(args) >= 1 && args[0] == "info":
		if p.state == "starting" {
			p.state = "running"
		}
		if p.state == "running" {
			return []byte("true"), nil
		}
		return nil, errors.New("connection refused")
	}
	return nil, fmt.Errorf("unexpected podman command %v", args)
}

func (p *bootingPodman) ping(runtime.Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "running" {
		return nil
	}
	return &runtime.EngineUnreachableError{Backend: runtime.BackendPodman,
		Socket: "/tmp/podman-machine-default-api.sock", Err: errors.New("dial unix: connection refused")}
}

// TestMachineHold_BootingMachineIsNotTakenForStopped: the daemon holds a
// machine it had a runtime on and then finds stopped — the volunteer stopped
// it, and starting it again would undo that. A machine found still booting
// (the engine dropped out while it restarted) was read as stopped, so the
// hold took it: the daemon told the volunteer they had stopped the machine
// and left it to be started again, instead of waiting for it to finish
// booting and connecting to it.
func TestMachineHold_BootingMachineIsNotTakenForStopped(t *testing.T) {
	p := &bootingPodman{state: "stopped"}
	// The machine-stop tests' daemon, driven by this podman instead of its
	// own (tb88Daemon restores the executor when the test ends).
	d := tb88Daemon(t, &tb88Podman{state: "stopped"})
	runtime.CommandExecutor = p.exec
	d.containerFactory.SetEnginePingForTest(p.ping)

	if !d.RedetectContainerRuntime(context.Background(), true) {
		t.Fatal("premise: start-up did not register the container runtime")
	}
	cr := d.runtimeRegistry.GetRuntime("container")

	p.mu.Lock()
	p.state = "starting"
	p.mu.Unlock()
	d.NoteContainerEngineUnreachable(cr, tb80Outage())

	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Error("the probe did not connect to the machine once it had booted")
	}
	if held, source := d.PodmanMachineHeldStopped(); held {
		t.Errorf("a booting machine was held as stopped (%s)", source)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.starts != 1 {
		t.Errorf("podman machine start ran %d times, want 1 (start-up only)", p.starts)
	}
}
