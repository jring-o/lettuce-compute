package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TB-83 regression tests, daemon side: the yield monitor is a third
// automatic pause source beside the resource and thermal monitors. Each
// source keeps its own flag, the reported reason ranks them, and "busy"
// carries the measured share for status. Before this change one flag was
// overwritten by whichever monitor signalled last, so a source resuming
// could unfreeze work another still held paused.

// TestTB83_PauseSourcesAreIndependent: thermal and busy both hold; thermal
// releasing leaves the daemon paused by busy, and the reason says so.
func TestTB83_PauseSourcesAreIndependent(t *testing.T) {
	d := &Daemon{logger: newTestLogger()}

	d.setAutoPause(pauseSourceBusy, true)
	if !d.IsPaused() || d.PauseReason() != "busy" {
		t.Fatalf("after busy pause: paused=%v reason=%q, want paused/busy", d.IsPaused(), d.PauseReason())
	}
	d.setAutoPause(pauseSourceThermal, true)
	if got := d.PauseReason(); got != "thermal" {
		t.Errorf("thermal + busy reason = %q, want thermal (the more serious source ranks first)", got)
	}
	d.setAutoPause(pauseSourceThermal, false)
	if !d.IsPaused() {
		t.Fatal("thermal released while busy still holds: daemon must stay paused")
	}
	if got := d.PauseReason(); got != "busy" {
		t.Errorf("reason after thermal release = %q, want busy", got)
	}
	d.setAutoPause(pauseSourceBusy, false)
	if d.IsPaused() || d.PauseReason() != "" {
		t.Errorf("all sources released: paused=%v reason=%q, want neither", d.IsPaused(), d.PauseReason())
	}
}

// The schedule signal ranks last among the automatic sources; a user pause
// outranks everything (it is the one `resume` undoes).
func TestTB83_PauseReasonPrecedence(t *testing.T) {
	d := &Daemon{logger: newTestLogger()}
	d.setAutoPause(pauseSourceResource, true)
	d.setAutoPause(pauseSourceBusy, true)
	if got := d.PauseReason(); got != "busy" {
		t.Errorf("scheduled + busy reason = %q, want busy", got)
	}
	d.mu.Lock()
	d.userPaused = true
	d.mu.Unlock()
	if got := d.PauseReason(); got != "user" {
		t.Errorf("with a user pause reason = %q, want user", got)
	}
}

// PauseDetail carries the measured share and thresholds for "busy" only.
func TestTB83_PauseDetailNamesTheShare(t *testing.T) {
	ym := runtime.NewYieldMonitor(runtime.YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15}, make(chan bool, 1), newTestLogger())
	d := &Daemon{logger: newTestLogger(), yieldMonitor: ym}

	if got := d.PauseDetail(); got != "" {
		t.Errorf("unpaused detail = %q, want empty", got)
	}
	d.setAutoPause(pauseSourceBusy, true)
	got := d.PauseDetail()
	for _, want := range []string{"other programs are using", "pause above 25%", "resume below 15%"} {
		if !strings.Contains(got, want) {
			t.Errorf("busy detail %q lacks %q", got, want)
		}
	}
	d.setAutoPause(pauseSourceThermal, true)
	if got := d.PauseDetail(); got != "" {
		t.Errorf("thermal-ranked pause detail = %q, want empty (the detail is busy's)", got)
	}
}

// ownCPUMeter never goes backwards: a source that disappears keeps its
// seconds, and a source whose counter fell keeps its high mark.
func TestTB83_OwnCPUMeterIsMonotonic(t *testing.T) {
	var m ownCPUMeter
	if got := m.total(map[string]float64{"a": 10, "b": 5}); got != 15 {
		t.Fatalf("total = %v, want 15", got)
	}
	// "b" finished: its 5 s are retired, not lost.
	if got := m.total(map[string]float64{"a": 12}); got != 17 {
		t.Errorf("after b exits total = %v, want 17 (12 live + 5 retired)", got)
	}
	// "a" reports lower than before (a counter restart): high mark kept.
	if got := m.total(map[string]float64{"a": 3}); got != 17 {
		t.Errorf("after a's counter fell total = %v, want 17", got)
	}
	// Nothing live: everything retired, still 17.
	if got := m.total(map[string]float64{}); got != 17 {
		t.Errorf("with nothing live total = %v, want 17", got)
	}
}

// fakeProcessGroup stands in for the platform process group.
type fakeProcessGroup struct {
	ProcessGroup
	groups map[string]float64
	err    error
}

func (f *fakeProcessGroup) CPUSeconds() (map[string]float64, error) { return f.groups, f.err }

// fakeStatsClient is an engine client whose only live method is the stats read.
type fakeStatsClient struct {
	runtime.DockerClient
	nanos map[string]uint64
	err   error
}

func (f *fakeStatsClient) ContainerCPUNanos(ctx context.Context, id string) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.nanos[id], nil
}

// ownCPUSeconds adds the daemon, its native process groups and its
// containers; a container whose stats cannot be read makes the figure an
// error, so the monitor never blames the machine's load on other programs.
func TestTB83_OwnCPUSecondsAddsEverySource(t *testing.T) {
	sm := NewSlotManager(2, newTestLogger())
	client := &fakeStatsClient{nanos: map[string]uint64{"c1": 3_000_000_000}}
	sm.slots[0].active = true
	sm.slots[0].processHandle = NewContainerProcessHandle(client, "c1")
	sm.slots[1].active = true
	sm.slots[1].processHandle = NewNativeProcessHandle(4242, nil)

	d := &Daemon{logger: newTestLogger(), slotManager: sm, processGroup: &fakeProcessGroup{groups: map[string]float64{"4242": 7}}}
	self, err := runtime.SelfCPUSeconds()
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ownCPUSeconds()
	if err != nil {
		t.Fatal(err)
	}
	// self (≥ what was read a moment ago) + 7 s native + 3 s container.
	if got < self+10 || got > self+10.5 {
		t.Errorf("ownCPUSeconds = %v, want about %v (self) + 10", got, self)
	}

	client.err = errors.New("socket closed")
	if _, err := d.ownCPUSeconds(); err == nil {
		t.Error("a container whose stats cannot be read must make own CPU an error, not a smaller number")
	}

	// A container that has just been removed is not an error: its seconds
	// are retired.
	client.err = nil
	sm.slots[0].processHandle = NewContainerProcessHandle(&notFoundClient{}, "gone")
	got2, err := d.ownCPUSeconds()
	if err != nil {
		t.Fatalf("a removed container must not be an error: %v", err)
	}
	if got2 < got-0.5 {
		t.Errorf("after the container's removal own = %v, must not drop below the earlier %v", got2, got)
	}

	// Native tasks with no process group cannot be attributed: an error.
	d.processGroup = nil
	if _, err := d.ownCPUSeconds(); err == nil {
		t.Error("native tasks without a process group must be an error")
	}
}

type notFoundClient struct{ runtime.DockerClient }

func (notFoundClient) ContainerCPUNanos(context.Context, string) (uint64, error) {
	return 0, notFoundErr{}
}

// notFoundErr satisfies the engine SDK's not-found check.
type notFoundErr struct{}

func (notFoundErr) Error() string { return "no such container" }
func (notFoundErr) NotFound()     {}

// OwnProcesses lists container handles with their client and counts native
// ones; inactive slots and slots without a handle are ignored.
func TestTB83_OwnProcessesListsActiveHandles(t *testing.T) {
	sm := NewSlotManager(3, newTestLogger())
	client := &fakeStatsClient{}
	sm.slots[0].active = true
	sm.slots[0].processHandle = NewContainerProcessHandle(client, "abc")
	sm.slots[1].active = true
	sm.slots[1].processHandle = NewNativeProcessHandle(1, nil)
	sm.slots[2].active = false
	sm.slots[2].processHandle = NewContainerProcessHandle(client, "inactive")

	containers, native := sm.OwnProcesses()
	if len(containers) != 1 || containers[0].ContainerID != "abc" || containers[0].Client != client {
		t.Errorf("containers = %+v, want the one active container with its client", containers)
	}
	if native != 1 {
		t.Errorf("native = %d, want 1", native)
	}
}

// The run loop treats the yield channel exactly as the thermal one: a pause
// suspends everything and stops the fetcher, a resume restarts it.
func TestTB83_RunLoopHonoursYieldPauseChannel(t *testing.T) {
	var mu sync.Mutex
	workRequests := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			mu.Lock()
			workRequests++
			mu.Unlock()
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := NewRuntimeRegistry()
	registry.Register(&mockRuntime{canHandle: true, name: "native"})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := config.Defaults()
	cfg.DataDir = testDataDir()
	cfg.Thermal.Enabled = false
	cfg.Yield.Enabled = false // the monitor goroutine stays off; the channel is driven directly

	d := NewDaemon(DaemonConfig{
		Config:          cfg,
		PubKey:          pub,
		PrivKey:         priv,
		Client:          mc,
		VolunteerID:     "test-volunteer-id",
		RuntimeRegistry: registry,
		Logger:          logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond
	d.multiClient.SetBackoff(1*time.Millisecond, 16*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawBusy, sawDetail bool
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.yieldPauseCh <- true
		time.Sleep(100 * time.Millisecond)
		sawBusy = d.PauseReason() == "busy"
		sawDetail = strings.Contains(d.PauseDetail(), "other programs are using")
		d.yieldPauseCh <- false
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	if !sawBusy {
		t.Error("PauseReason while paused by the yield channel was not \"busy\"")
	}
	if !sawDetail {
		t.Error("PauseDetail while paused by the yield channel did not describe the share")
	}
	mu.Lock()
	n := workRequests
	mu.Unlock()
	if n == 0 {
		t.Error("expected some work requests around the pause")
	}
	d.mu.Lock()
	paused := d.paused
	d.mu.Unlock()
	if paused {
		t.Error("daemon should not be paused after the resume signal")
	}
}
