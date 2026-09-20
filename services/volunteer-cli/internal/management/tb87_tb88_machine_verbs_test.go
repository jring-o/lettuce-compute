package management

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-87, management half: POST /api/v1/container-runtime/{start,stop,setup}
// ran `podman machine start` / `stop` / `init` to completion inside the
// request — 30-120 s on an Intel Mac against the app's 15 s client, which
// gave up with "DAEMON_UNREACHABLE … error sending request" while the
// machine was in fact starting. The verbs now answer 202 as soon as the
// operation is accepted, GET /api/v1/container-runtime reports starting /
// stopping meanwhile and carries a failure in `error` afterwards.
//
// TB-88, management half: a stop through the API is the volunteer's decision.
// The status route names it (machine_held_stopped, machine_stop_source
// "app") and the daemon leaves the machine stopped; the start verb releases
// the hold.

// tb87Podman is a scripted `podman` whose machine start and stop block on
// gate while it is non-nil, so a test can observe a request in flight.
type tb87Podman struct {
	mu       sync.Mutex
	state    string
	gate     chan struct{}
	startErr error
	initErr  error
	starts   int
	stops    int
}

// setGate installs (or removes) the gate start, stop and init block on.
func (p *tb87Podman) setGate(gate chan struct{}) {
	p.mu.Lock()
	p.gate = gate
	p.mu.Unlock()
}

// fail sets the error the next starts and inits return (nil clears it).
func (p *tb87Podman) fail(startErr, initErr error) {
	p.mu.Lock()
	p.startErr = startErr
	p.initErr = initErr
	p.mu.Unlock()
}

func (p *tb87Podman) set(state string) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

func (p *tb87Podman) count() (starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts, p.stops
}

func (p *tb87Podman) exec(name string, args ...string) ([]byte, error) {
	switch {
	case len(args) >= 2 && args[0] == "machine" && args[1] == "inspect":
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.state == "not_initialized" {
			return []byte("Error: no VM found"), fmt.Errorf("exit status 125")
		}
		return []byte(fmt.Sprintf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":1366},"ConnectionInfo":{"PodmanSocket":{"Path":"/tmp/podman-machine-default-api.sock"}}}]`, p.state)), nil
	case len(args) >= 2 && args[0] == "machine" && (args[1] == "start" || args[1] == "stop" || args[1] == "init"):
		p.mu.Lock()
		gate := p.gate
		p.mu.Unlock()
		if gate != nil {
			<-gate
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		switch args[1] {
		case "start":
			p.starts++
			if p.startErr != nil {
				return []byte("hypervisor error\n"), p.startErr
			}
			p.state = "running"
		case "stop":
			p.stops++
			p.state = "stopped"
		case "init":
			if p.initErr != nil {
				return []byte("cannot init\n"), p.initErr
			}
			p.state = "stopped"
		}
		return []byte("OK\n"), nil
	case len(args) >= 1 && args[0] == "info":
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.state == "running" {
			return []byte("true"), nil
		}
		return nil, errors.New("connection refused")
	case len(args) >= 1 && args[0] == "--version":
		return []byte("podman version 5.8.6\n"), nil
	}
	return nil, fmt.Errorf("unexpected podman command %v", args)
}

func (p *tb87Podman) ping(runtime.Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "running" {
		return nil
	}
	return &runtime.EngineUnreachableError{Backend: runtime.BackendPodman, Socket: "/tmp/podman-machine-default-api.sock", Err: errors.New("connection refused")}
}

// tb87Env is a management env whose daemon has a production-shaped
// container factory (detection finds a Podman binary, so the factory owns a
// real machine manager driven through the scripted podman), a head trusted
// for CONTAINER, and the test put on the machine platform. It runs the
// start-up bring-up (a forced Build, as buildRuntimeRegistry does) so the
// machine manager exists, as it does on every production daemon that found
// a Podman binary: with the podman's state "running" the runtime is
// registered; a failing start leaves the machine stopped, unregistered and
// not held (the daemon's own bring-up failed, so it retries later).
func tb87Env(t *testing.T, p *tb87Podman) *testEnv {
	t.Helper()
	env := tb87EnvUnbooted(t, p)
	env.daemon.RedetectContainerRuntime(context.Background(), true)
	return env
}

func tb87EnvUnbooted(t *testing.T, p *tb87Podman) *testEnv {
	t.Helper()
	t.Cleanup(runtime.SetNeedsMachineForTest(true))
	orig := runtime.CommandExecutor
	runtime.CommandExecutor = p.exec
	t.Cleanup(func() { runtime.CommandExecutor = orig })

	registry := daemon.NewRuntimeRegistry()
	factory := func(cfg *config.Config, logger *slog.Logger) *daemon.ContainerRuntimeFactory {
		f := daemon.NewContainerRuntimeFactoryForTest(cfg, logger,
			func(runtime.ContainerBackend) runtime.BackendInfo {
				return runtime.BackendInfo{Backend: runtime.BackendPodman, Engine: "podman", Version: "5.8.6",
					BinaryPath: "/opt/podman/bin/podman", SocketPath: "/tmp/podman-machine-default-api.sock"}
			},
			func(runtime.BackendInfo) (runtime.Runtime, error) {
				cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), logger, nil)
				cr.SetBackend(runtime.BackendPodman)
				return cr, nil
			})
		f.SetEnginePingForTest(p.ping)
		return f
	}
	return tb59Env(t, registry, factory, []string{"CONTAINER"})
}

// requestWithin sends the request with a client timeout, so a handler that
// blocks for the command's whole duration fails the test instead of hanging
// it (the pre-fix shape).
func (e *testEnv) requestWithin(t *testing.T, timeout time.Duration, method, path string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, e.baseURL+path, strings.NewReader(""))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

// waitRouteStatus polls GET /api/v1/container-runtime until status reads want.
func (e *testEnv) waitRouteStatus(t *testing.T, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = decodeJSON(t, e.doRequest(t, "GET", "/api/v1/container-runtime", ""))
		if last["status"] == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the status route never reported %q; last %v", want, last)
	return last
}

// TestTB87_StartVerbAnswersAtOnceWhileTheMachineStarts: `podman machine
// start` is blocked; the verb answers 202 "starting" within the client's
// timeout; the status route reads starting; once the command returns it
// reads running. Pre-fix: the request blocked until the command returned and
// the client timed out.
func TestTB87_StartVerbAnswersAtOnceWhileTheMachineStarts(t *testing.T) {
	p := &tb87Podman{state: "stopped", startErr: errors.New("boot: hypervisor not ready")}
	env := tb87Env(t, p)
	p.fail(nil, nil)
	gate := make(chan struct{})
	p.setGate(gate)
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)

	resp, err := env.requestWithin(t, 2*time.Second, "POST", "/api/v1/container-runtime/start")
	if err != nil {
		t.Fatalf("the start verb did not answer while podman machine start was running: %v (pre-fix: the handler blocked for the command's whole duration and the app's 15 s client reported the daemon unreachable)", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: status %d, want 202 Accepted", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["status"] != "starting" {
		t.Errorf("start body = %v, want status starting", body)
	}
	during := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", ""))
	if during["status"] != "starting" {
		t.Errorf("status route during the start = %v, want starting", during["status"])
	}
	// A second click while it runs is not a second start.
	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("a second start during the first: status %d, want 202 (already starting)", resp.StatusCode)
	}
	resp.Body.Close()

	release()
	after := env.waitRouteStatus(t, "running")
	if after["error"] != nil {
		t.Errorf("error after a successful start = %v, want none", after["error"])
	}
	if starts, _ := p.count(); starts != 2 {
		t.Errorf("podman machine start ran %d times, want 2 (the failed boot start + the verb)", starts)
	}
}

// TestTB87_StopVerbAnswersAtOnceAndReportsStopping: the same for stop, and
// the route says stopping — not starting — while the machine goes down.
func TestTB87_StopVerbAnswersAtOnceAndReportsStopping(t *testing.T) {
	p := &tb87Podman{state: "running"}
	env := tb87Env(t, p)
	gate := make(chan struct{})
	p.setGate(gate)
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	if before := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", "")); before["status"] != "running" {
		t.Fatalf("premise: status %v, want running", before["status"])
	}

	resp, err := env.requestWithin(t, 2*time.Second, "POST", "/api/v1/container-runtime/stop")
	if err != nil {
		t.Fatalf("the stop verb did not answer while podman machine stop was running: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202 Accepted", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["status"] != "stopping" {
		t.Errorf("stop body = %v, want status stopping", body)
	}
	during := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", ""))
	if during["status"] != "stopping" {
		t.Errorf("status route during the stop = %v, want stopping (pre-fix: starting, for any operation in progress)", during["status"])
	}
	// The volunteer's stop is on record from the accept on (TB-88).
	if during["machine_held_stopped"] != true || during["machine_stop_source"] != "app" {
		t.Errorf("during the stop: machine_held_stopped=%v machine_stop_source=%v, want true/app", during["machine_held_stopped"], during["machine_stop_source"])
	}

	release()
	after := env.waitRouteStatus(t, "stopped")
	if after["machine_held_stopped"] != true {
		t.Errorf("after the stop: machine_held_stopped=%v, want true", after["machine_held_stopped"])
	}
}

// TestTB87_FailedStartSurfacesInTheStatusRoute: a start that fails after the
// 202 has no request to fail; the route reports the machine's state with the
// failure in `error`. Pre-fix: 500 START_FAILED on the request itself.
func TestTB87_FailedStartSurfacesInTheStatusRoute(t *testing.T) {
	p := &tb87Podman{state: "stopped", startErr: errors.New("boot: hypervisor not ready")}
	env := tb87Env(t, p)
	p.fail(errors.New("exit status 1: hypervisor error"), nil)

	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", ""))
		if last["status"] == "stopped" && last["error"] != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last["status"] != "stopped" {
		t.Errorf("status after the failed start = %v, want stopped (the machine's own state)", last["status"])
	}
	errText, _ := last["error"].(string)
	if !strings.Contains(errText, "podman machine start failed") || !strings.Contains(errText, "hypervisor error") {
		t.Errorf("error after the failed start = %q, want the verb's start failure", errText)
	}
}

// TestTB87_SetupVerbAnswersAtOnceThenRunning: setup (init + start, minutes
// on a real machine) is asynchronous too; the route reads starting, then
// running, and the daemon registers the runtime.
func TestTB87_SetupVerbAnswersAtOnceThenRunning(t *testing.T) {
	p := &tb87Podman{state: "not_initialized", initErr: errors.New("boot: image download failed")}
	env := tb87Env(t, p)
	if before := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", "")); before["status"] != "not_initialized" {
		t.Fatalf("premise: status %v, want not_initialized", before["status"])
	}
	p.fail(nil, nil)
	gate := make(chan struct{})
	p.setGate(gate)
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)

	resp, err := env.requestWithin(t, 2*time.Second, "POST", "/api/v1/container-runtime/setup")
	if err != nil {
		t.Fatalf("the setup verb did not answer while podman machine init was running: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("setup: status %d, want 202", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["status"] != "starting" {
		t.Errorf("setup body = %v, want status starting", body)
	}
	if during := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", "")); during["status"] != "starting" {
		t.Errorf("status route during setup = %v, want starting", during["status"])
	}
	release()
	env.waitRouteStatus(t, "running")
	// The done callback requested a probe; the loop is not running in this
	// env, so drive it: the runtime registers against the new machine.
	if !env.daemon.RedetectContainerRuntime(context.Background(), true) {
		t.Error("the runtime was not registered once the machine was set up")
	}
}

// TestTB88_StopVerbLeavesTheMachineStoppedUntilStart: after the Stop button
// the daemon's probes do not start the machine; the route says why; the
// Start button releases the hold and the machine comes back.
func TestTB88_StopVerbLeavesTheMachineStoppedUntilStart(t *testing.T) {
	p := &tb87Podman{state: "stopped"}
	env := tb87Env(t, p) // the boot starts the stopped machine and registers the runtime
	if env.daemon.ContainerRedetectActive() {
		t.Fatal("start-up bring-up did not register the runtime")
	}
	if starts, _ := p.count(); starts != 1 {
		t.Fatalf("boot ran podman machine start %d times, want 1", starts)
	}
	if before := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", "")); before["status"] != "running" || before["machine_held_stopped"] != false {
		t.Fatalf("premise: %v", before)
	}

	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/stop", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	stopped := env.waitRouteStatus(t, "stopped")
	if stopped["machine_held_stopped"] != true || stopped["machine_stop_source"] != "app" {
		t.Errorf("after the stop: machine_held_stopped=%v machine_stop_source=%v, want true/app", stopped["machine_held_stopped"], stopped["machine_stop_source"])
	}
	if stopped["redetecting"] != true {
		t.Errorf("redetecting=%v after the stop, want true: the daemon keeps looking for a hand-started machine", stopped["redetecting"])
	}
	if stopped["backend"] != "podman" {
		t.Errorf("backend after the stop = %v, want podman", stopped["backend"])
	}

	// The loop's probes (the stop's wake and two ticks) leave it stopped.
	for i := 0; i < 3; i++ {
		if env.daemon.RedetectContainerRuntime(context.Background(), false) {
			t.Fatalf("probe %d registered a runtime on the stopped machine", i+1)
		}
	}
	if starts, _ := p.count(); starts != 1 {
		t.Errorf("podman machine start ran %d times after the Stop button, want 1: the daemon restarted the machine the volunteer stopped", starts)
	}
	// The redetect verb is still offered (the runtime is out of service).
	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/redetect", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("redetect while held: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// The Start button: 202, the machine starts, and the start's completion
	// requests a probe. The loop is not running in this env, so wait for the
	// start to finish, then drive the probe as the loop would; the route then
	// reads running with the hold gone.
	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if starts, _ := p.count(); starts == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if starts, _ := p.count(); starts != 2 {
		t.Fatalf("podman machine start ran %d times, want 2 (boot + the Start button)", starts)
	}
	for time.Now().Before(deadline) && env.daemon.GetMachineManager().Status().Status == runtime.MachineStarting {
		time.Sleep(10 * time.Millisecond)
	}
	if !env.daemon.RedetectContainerRuntime(context.Background(), env.daemon.TakeContainerRedetectForceForTest()) {
		t.Fatal("the probe after the Start button did not register the runtime")
	}
	running := env.waitRouteStatus(t, "running")
	if running["machine_held_stopped"] != false {
		t.Errorf("machine_held_stopped=%v after the Start button, want false", running["machine_held_stopped"])
	}
	if running["error"] != nil {
		t.Errorf("error after the restart = %v, want none", running["error"])
	}
}

// TestTB88_HandStartedHeldMachineReportsRunningAndWakesTheProbe: a held
// machine the volunteer starts with `podman machine start` reads running on
// the route at once — not "unreachable" until the loop's next tick — and the
// route brings the probe forward.
func TestTB88_HandStartedHeldMachineReportsRunningAndWakesTheProbe(t *testing.T) {
	p := &tb87Podman{state: "stopped"}
	env := tb87Env(t, p)
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/stop", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	env.waitRouteStatus(t, "stopped")
	env.daemon.RedetectContainerRuntime(context.Background(), false)
	if held, _ := env.daemon.PodmanMachineHeldStopped(); !held {
		t.Fatal("premise: the machine is not held after the stop")
	}

	// `podman machine start` by hand.
	p.set("running")
	env.daemon.GetMachineManager().InvalidateStatus()
	now := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", ""))
	if now["status"] != "running" || now["machine_held_stopped"] != false {
		t.Errorf("route over a hand-started held machine: status=%v machine_held_stopped=%v, want running/false (pre-fix shape: unreachable until the next tick)", now["status"], now["machine_held_stopped"])
	}
	if !env.daemon.ContainerRedetectWakePendingForTest() {
		t.Error("the route did not bring the probe forward")
	}
	if !env.daemon.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("the probe did not register the hand-started machine")
	}
	if starts, _ := p.count(); starts != 1 {
		t.Errorf("podman machine start ran %d times, want 1 (the boot): a hand-started machine is connected to, not started", starts)
	}
}

// TestTB88_StartVerbRefusedWhileStopping: a start while the stop is still
// running is refused as busy rather than queued behind it (the tester's
// "press again" after the false error).
func TestTB88_StartVerbRefusedWhileStopping(t *testing.T) {
	p := &tb87Podman{state: "running"}
	env := tb87Env(t, p)
	gate := make(chan struct{})
	p.setGate(gate)
	t.Cleanup(func() { close(gate) })
	resp := env.doRequest(t, "POST", "/api/v1/container-runtime/stop", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/start", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("start during the stop: status %d, want 409", resp.StatusCode)
	}
	if code := errorCode(decodeJSON(t, resp)); code != "MACHINE_BUSY" {
		t.Errorf("error code = %v, want MACHINE_BUSY", code)
	}
}
