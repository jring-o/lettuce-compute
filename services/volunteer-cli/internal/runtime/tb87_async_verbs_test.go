package runtime

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TB-87 regression, manager half: `podman machine start` / `stop` take
// 30-120 s on an Intel Mac, and the management API ran them to completion
// inside one request. StartAsync / StopAsync / SetupAsync return as soon as
// the operation is accepted, Status reports starting / stopping from that
// moment (not from the moment the command begins), a failure is carried in
// Status until the next operation succeeds, and a second operation while one
// is in flight is refused rather than queued behind it.

// tb87Podman is a scripted `podman` whose machine start and stop block until
// the test releases them, so the test can observe the manager mid-operation.
type tb87Podman struct {
	mu       sync.Mutex
	state    string // running | stopped
	startErr error
	gate     chan struct{} // start/stop wait on it when non-nil
	starts   int
	stops    int
}

func (p *tb87Podman) exec(name string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
		p.mu.Lock()
		defer p.mu.Unlock()
		return []byte(fmt.Sprintf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":1366},"ConnectionInfo":{}}]`, p.state)), nil
	}
	if len(args) >= 2 && args[0] == "machine" && (args[1] == "start" || args[1] == "stop") {
		p.mu.Lock()
		gate := p.gate
		p.mu.Unlock()
		if gate != nil {
			<-gate
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if args[1] == "start" {
			p.starts++
			if p.startErr != nil {
				return []byte("cannot start\n"), p.startErr
			}
			p.state = "running"
		} else {
			p.stops++
			p.state = "stopped"
		}
		return []byte("OK\n"), nil
	}
	return nil, exec.ErrNotFound
}

func tb87Manager(t *testing.T, p *tb87Podman) *PodmanMachineManager {
	t.Helper()
	t.Cleanup(SetNeedsMachineForTest(true))
	withMockExecutor(t, p.exec)
	return newTestManager(t)
}

// waitStatus polls Status until it reports want or the deadline passes.
func waitStatus(t *testing.T, m *PodmanMachineManager, want MachineStatus) MachineInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var info MachineInfo
	for time.Now().Before(deadline) {
		info = m.Status()
		if info.Status == want {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Status never reported %s; last %+v", want, info)
	return info
}

// TestTB87_StartAsyncReturnsAtOnceAndReportsStarting: the accept is
// immediate while `podman machine start` is still running; Status says
// starting from the accept on; a second start is refused as busy; once the
// command returns, done sees success and Status reads running.
func TestTB87_StartAsyncReturnsAtOnceAndReportsStarting(t *testing.T) {
	p := &tb87Podman{state: "stopped", gate: make(chan struct{})}
	m := tb87Manager(t, p)

	done := make(chan error, 1)
	accepted := make(chan struct{})
	go func() {
		err := m.StartAsync(func(err error) { done <- err })
		if err != nil {
			t.Errorf("StartAsync: %v", err)
		}
		close(accepted)
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("StartAsync did not return while podman machine start was running (pre-fix: Start blocked for the command's whole duration)")
	}

	// Reported as starting at once — before Status has been asked once, so
	// this is the accept, not the command's own flag.
	if st := m.Status(); st.Status != MachineStarting {
		t.Errorf("Status during the start = %s, want starting", st.Status)
	}
	if err := m.StartAsync(nil); !errors.Is(err, ErrMachineBusy) {
		t.Errorf("a second StartAsync while one is in flight = %v, want ErrMachineBusy", err)
	}

	close(p.gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("done reported %v, want success", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("done was never called")
	}
	info := waitStatus(t, m, MachineRunning)
	if info.Error != "" {
		t.Errorf("Status after a successful start carries an error: %q", info.Error)
	}
	if !m.StartedByThisProcess() {
		t.Error("a machine started through StartAsync must be owned by this process (PB-27)")
	}
	if p.starts != 1 {
		t.Errorf("podman machine start ran %d times, want 1", p.starts)
	}
}

// TestTB87_StopAsyncReportsStoppingNotStarting: the manager used to report
// any operation in progress — a stop included — as starting, which the app
// rendered as "Starting..." while the machine went down.
func TestTB87_StopAsyncReportsStoppingNotStarting(t *testing.T) {
	p := &tb87Podman{state: "running", gate: make(chan struct{})}
	m := tb87Manager(t, p)
	if st := m.Status(); st.Status != MachineRunning {
		t.Fatalf("premise: machine reads %s, want running", st.Status)
	}

	done := make(chan error, 1)
	if err := m.StopAsync(func(err error) { done <- err }); err != nil {
		t.Fatalf("StopAsync: %v", err)
	}
	if st := m.Status(); st.Status != MachineStopping {
		t.Errorf("Status during the stop = %s, want stopping (pre-fix: starting, for any operation in progress)", st.Status)
	}
	if err := m.StartAsync(nil); !errors.Is(err, ErrMachineBusy) {
		t.Errorf("StartAsync during a stop = %v, want ErrMachineBusy", err)
	}
	close(p.gate)
	if err := <-done; err != nil {
		t.Fatalf("done reported %v, want success", err)
	}
	waitStatus(t, m, MachineStopped)
	if m.StartedByThisProcess() {
		t.Error("ownership must clear once the stop completed")
	}
}

// TestTB87_FailedAsyncStartSurfacesInStatus: a start that fails after the
// accept has no request to fail — the failure rides on Status.Error until
// the next operation succeeds, so a caller that only polls learns of it.
func TestTB87_FailedAsyncStartSurfacesInStatus(t *testing.T) {
	p := &tb87Podman{state: "stopped", startErr: errors.New("exit status 125")}
	m := tb87Manager(t, p)

	done := make(chan error, 1)
	if err := m.StartAsync(func(err error) { done <- err }); err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "podman machine start failed") {
		t.Fatalf("done = %v, want the start failure", err)
	}
	info := waitStatus(t, m, MachineStopped)
	if !strings.Contains(info.Error, "podman machine start failed") {
		t.Errorf("Status.Error after the failed start = %q, want the failure (the machine itself is fine, so this is the only place it can be reported)", info.Error)
	}
	if m.StartedByThisProcess() {
		t.Error("a failed start must not claim ownership")
	}

	// The next successful operation clears it.
	p.mu.Lock()
	p.startErr = nil
	p.mu.Unlock()
	if err := m.StartAsync(func(err error) { done <- err }); err != nil {
		t.Fatalf("second StartAsync: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("second start failed: %v", err)
	}
	if info := waitStatus(t, m, MachineRunning); info.Error != "" {
		t.Errorf("Status.Error after a later successful start = %q, want cleared", info.Error)
	}
}

// TestTB87_SetupAsyncReportsStartingAndItsOwnRefusal: Setup's own refusal
// (an inspect that errors) reaches Status too, not only the command failures
// recorded by init/start/stop.
func TestTB87_SetupAsyncReportsStartingAndItsOwnRefusal(t *testing.T) {
	t.Cleanup(SetNeedsMachineForTest(true))
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte("hypervisor unavailable"), fmt.Errorf("exit status 1")
		}
		return nil, exec.ErrNotFound
	})
	m := newTestManager(t)
	done := make(chan error, 1)
	if err := m.SetupAsync(2, 4096, 20, func(err error) { done <- err }); err != nil {
		t.Fatalf("SetupAsync: %v", err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "podman machine error") {
		t.Fatalf("done = %v, want Setup's machine-error refusal", err)
	}
	info := waitStatus(t, m, MachineError)
	if !strings.Contains(info.Error, "podman machine inspect failed") {
		t.Errorf("Status.Error = %q, want the inspect failure", info.Error)
	}
}
