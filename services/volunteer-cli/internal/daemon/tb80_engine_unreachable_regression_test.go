package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-80 regression tests, behavioural half.
//
// When a registered container engine's socket died under a running daemon,
// every container unit was abandoned as a BILLED failure: Prepare's ping
// failure went through the runtime breaker (three billed abandons, then
// "prepare failed 3 times", a ten-minute pause, then one more billed unit
// as the probe — forever: 188 copies in 30 hours on one Linux host), and a
// create-time connection refusal in Execute went through the LEAF breaker
// ("leaf keeps failing on this machine"). Both breakers classified by where
// the error happened, not what it was. An engine outage is now its own
// error (runtime.EngineUnreachableError): un-run units go back
// budget-neutral, the leaf breaker ignores it, and the runtime is taken out
// of service and re-probed with a ping.
//
// These tests compile against the pre-fix tree once the error type exists,
// so their red halves are behavioural: the give-backs unflagged, the
// prepare breaker tripped, the leaf breaker counting.

// tb80Outage is the reproduction's error: the engine's socket refuses.
func tb80Outage() error {
	return &runtime.EngineUnreachableError{Backend: runtime.BackendPodman,
		Socket: "/var/folders/82/T/podman/podman-machine-default-api.sock",
		Err:    errors.New("docker ping: Cannot connect to the Docker daemon at unix:///var/folders/82/T/podman/podman-machine-default-api.sock. Is the docker daemon running?")}
}

// tb80RecordingHead is a head that records every AbandonWorkUnit it receives.
type tb80RecordingHead struct {
	*mockClient
	mu       sync.Mutex
	abandons []*lettucev1.AbandonWorkUnitRequest
}

func newTB80RecordingHead() *tb80RecordingHead {
	h := &tb80RecordingHead{mockClient: &mockClient{}}
	h.mockClient.abandonFn = func(_ context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
		h.mu.Lock()
		h.abandons = append(h.abandons, req)
		h.mu.Unlock()
		return &lettucev1.AbandonWorkUnitResponse{Requeued: true}, nil
	}
	return h
}

func (h *tb80RecordingHead) recorded() []*lettucev1.AbandonWorkUnitRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*lettucev1.AbandonWorkUnitRequest(nil), h.abandons...)
}

// tb80ContainerBatch is one batch of n container units, as the head hands
// them out.
func tb80ContainerBatch(n int) []*lettucev1.WorkUnitAssignment {
	out := make([]*lettucev1.WorkUnitAssignment, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &lettucev1.WorkUnitAssignment{
			WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
			LeafId:        "leaf-container",
			Runtime:       "container",
			ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:tag"},
		})
	}
	return out
}

// tb80Daemon is a daemon with a container leaf on one head and a container
// runtime whose Prepare reports the outage.
func tb80Daemon(t *testing.T, head *ServerConnection, cr runtime.Runtime) *Daemon {
	t.Helper()
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.notices = NewNoticeLog()
	d.leafFailures = newLeafFailureTracker(nil)
	d.cfg.Servers = []config.ServerConfig{head.Config}
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{{ID: "leaf-container", Slug: "leaf-container", Name: "Container", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/img:tag"}}},
		DefaultWeights: map[string]int{"leaf-container": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-container": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})
	d.runtimeRegistry.Register(cr)
	return d
}

// TestTB80_PrepareEngineUnreachable_ReturnsBatchUnrunAndTripsNoBreaker is
// the reproduction's 22:20:40Z batch: six container units arrive, the
// engine's socket refuses the ping. Every unit goes back flagged un-run
// (RETURNED head-side: no copy billed, no bench) after ONE Prepare — the
// rest of the batch is not pinged again — the prepare breaker records
// nothing (no "prepare failed 3 times", no ten-minute pause), and the
// runtime leaves the registry so the next round never asks for the leaf.
// Pre-fix: six billed abandons, six pings, prepare_failed after the third.
func TestTB80_PrepareEngineUnreachable_ReturnsBatchUnrunAndTripsNoBreaker(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container",
		prepareFn: func(context.Context, *runtime.WorkUnit) (*runtime.PrepareResult, error) { return nil, tb80Outage() }}
	d := tb80Daemon(t, head, cr)
	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)

	leaf := d.leafCache.GetLeafs("server-a")[0]
	pushed, _ := f.bufferBatch(context.Background(), head, leaf, tb80ContainerBatch(6))
	if pushed != 0 {
		t.Fatalf("buffered %d units with a dead engine", pushed)
	}

	abandons := hc.recorded()
	if len(abandons) != 6 {
		t.Fatalf("head received %d AbandonWorkUnit calls, want 6 (one per unit of the batch)", len(abandons))
	}
	for _, a := range abandons {
		if !a.UnrunGiveback {
			t.Errorf("unit %s abandoned WITHOUT the un-run flag: the head bills the copy and benches this volunteer (reason %q)", a.WorkUnitId, a.Reason)
		}
		if !strings.Contains(a.Reason, "container engine unreachable") {
			t.Errorf("unit %s reason %q does not name the engine outage", a.WorkUnitId, a.Reason)
		}
	}
	cr.mu.Lock()
	prepares := cr.prepareCalls
	cr.mu.Unlock()
	if prepares != 1 {
		t.Errorf("Prepare called %d times for one batch, want 1: after the first refusal the rest of the batch is returned without another ping", prepares)
	}
	if n, pf := countNoticesByCode(d.notices, "prepare_failed"); n != 0 {
		t.Errorf("prepare_failed raised for an engine outage: %+v", pf)
	}
	if len(f.pausedRuntimes) != 0 || len(f.runtimeAbandons) != 0 {
		t.Errorf("the prepare breaker recorded the outage: paused=%v abandons=%v", f.pausedRuntimes, f.runtimeAbandons)
	}
	if n, lf := countNoticesByCode(d.notices, "leaf_failing"); n != 0 {
		t.Errorf("leaf_failing raised for an engine outage: %+v", lf)
	}
	if d.runtimeRegistry.GetRuntime("container") != nil {
		t.Error("container runtime still registered after its engine stopped answering; the next round would fetch and return another batch")
	}
	n, notice := countNoticesByCode(d.notices, "container_engine_unreachable")
	if n != 1 || notice.Count != 1 {
		t.Fatalf("container_engine_unreachable notices = %d (count %d), want exactly one", n, notice.Count)
	}
	if !strings.Contains(notice.Message, "podman-machine-default-api.sock") || !strings.Contains(notice.Message, "podman machine") {
		t.Errorf("notice does not name the socket and the remedy: %q", notice.Message)
	}
}

// TestTB80_ExecuteEngineUnreachable_IsNotALeafFailure is the reproduction's
// 22:20:39Z unit: a unit already in a slot fails at create because the socket
// refuses. It is abandoned with the engine named as the reason (it was
// run-started, so the head bills it as any started abandon), but the leaf
// breaker does not count it: three such failures raise no "leaf keeps
// failing on this machine". Pre-fix: leaf_failing after the third, and a
// ten-minute leaf pause blaming the artifact.
func TestTB80_ExecuteEngineUnreachable_IsNotALeafFailure(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	d.slotManager = NewSlotManager(2, d.logger)

	for i := 1; i <= leafFailurePauseThreshold; i++ {
		wu := &runtime.WorkUnit{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), LeafID: "leaf-container", Runtime: "container",
			ExecutionSpec: runtime.ExecutionSpec{Image: "ghcr.io/example/img:tag"}}
		d.handleSlotResult(context.Background(), SlotResult{SlotID: 1, WU: wu, Conn: head,
			Err: fmt.Errorf("create container: %w", tb80Outage())})
	}

	abandons := hc.recorded()
	if len(abandons) != leafFailurePauseThreshold {
		t.Fatalf("head received %d abandons, want %d", len(abandons), leafFailurePauseThreshold)
	}
	for _, a := range abandons {
		if !strings.Contains(a.Reason, "container engine unreachable") {
			t.Errorf("abandon reason %q does not name the engine outage", a.Reason)
		}
	}
	if d.leafFailurePaused("leaf-container") {
		t.Error("the leaf breaker paused the leaf for an engine outage: the volunteer is told the LEAF keeps failing")
	}
	if snap := d.LeafFailureSnapshot(); len(snap) != 0 {
		t.Errorf("the leaf breaker counted engine outages as leaf failures: %+v", snap)
	}
	if n, lf := countNoticesByCode(d.notices, "leaf_failing"); n != 0 {
		t.Errorf("leaf_failing raised for an engine outage: %+v", lf)
	}
}

// TestTB80_OrdinaryPrepareFailureStillTripsTheBreaker is the boundary: a
// Prepare that fails for a reason the engine ANSWERED with (an image the
// registry refuses) is still a billed abandon and still trips the prepare
// breaker after three — the TB-80 path is for the engine not answering,
// nothing else.
func TestTB80_OrdinaryPrepareFailureStillTripsTheBreaker(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container",
		prepareFn: func(context.Context, *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			return nil, errors.New("pull access denied for ghcr.io/example/img, repository does not exist")
		}}
	d := tb80Daemon(t, head, cr)
	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)

	leaf := d.leafCache.GetLeafs("server-a")[0]
	f.bufferBatch(context.Background(), head, leaf, tb80ContainerBatch(3))

	for _, a := range hc.recorded() {
		if a.UnrunGiveback {
			t.Errorf("a broken-image prepare failure was returned un-run (%q): it must stay billed so a broken artifact still exhausts its budget", a.Reason)
		}
	}
	if n, _ := countNoticesByCode(d.notices, "prepare_failed"); n != 1 {
		t.Errorf("prepare_failed notices = %d, want 1 after three ordinary prepare failures", n)
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Error("an ordinary prepare failure took the container runtime out of service")
	}
	if n, _ := countNoticesByCode(d.notices, "container_engine_unreachable"); n != 0 {
		t.Error("container_engine_unreachable raised for a pull the engine refused")
	}
}
