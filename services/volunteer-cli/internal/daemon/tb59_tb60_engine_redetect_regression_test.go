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

// TB-59 and TB-60 regression tests.
//
// TB-60: the "connected but getting no work after repeated polls" streak
// counted fetch rounds in which every leaf was skipped BEFORE any request, so
// on a host whose every leaf is runtime-blocked the notice fired 5 s after
// start — claiming polls that never happened, in the reproduction's log the
// head's whole trace was the one registration line — and where the skips were
// transient it fell through to the generic head-side wording on top of the
// accurate prepare_failed / leaf_failing notice. Only rounds that asked a head
// count now; the runtime-blocked verdict has its own notice, raised at once.
//
// TB-59: the container runtime was built once at start and every head told
// once; an engine that came up later was invisible until a restart. The daemon
// now keeps the detector, re-probes while it has no container runtime, and on
// success registers the runtime, re-advertises to every head on its next
// contact, and resolves the runtime-blocked notice.

func countNoticesByCode(l *NoticeLog, code string) (n int, last Notice) {
	notices, _ := l.Since(0)
	for _, x := range notices {
		if x.Code == code {
			n++
			last = x
		}
	}
	return n, last
}

// tb60BlockedHost is the reproduction's shape: a head with a container leaf and
// a native-only leaf, a machine with only WASM registered, the head trusted for
// CONTAINER (so the engine is simply missing) but not NATIVE. Every leaf is
// skipped before the request; nothing is ever asked.
func tb60BlockedHost(t *testing.T, mc WorkClient) (*Daemon, *ServerConnection) {
	t.Helper()
	head := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.notices = NewNoticeLog()
	d.cfg.Servers = []config.ServerConfig{head.Config}
	tb49TwoLeafHead(d, "server-a")
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})
	return d, head
}

func runFetcherFor(d *Daemon, dur time.Duration) (*Fetcher, *bytes.Buffer) {
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	f.Run(ctx)
	return f, &buf
}

// TestTB60_RuntimeBlockedRoundsAreNotPolls: every leaf skipped pre-request, so
// zero RequestWorkUnit calls in 300 ms of Run — and therefore no "after
// repeated polls" notice. The configuration fact gets its own notice, raised
// on the FIRST round (count 1, not a streak of five), naming the trust gate and
// its remedy. Pre-fix: no_work raised after five 1 ms rounds with the TB-49
// wording, no runtime_blocked notice.
func TestTB60_RuntimeBlockedRoundsAreNotPolls(t *testing.T) {
	mc, requests := tb49CountingHead()
	d, _ := tb60BlockedHost(t, mc)

	_, buf := runFetcherFor(d, 300*time.Millisecond)

	if reqs := requests(); len(reqs) != 0 {
		t.Errorf("RequestWorkUnit was issued for %v; every leaf is runtime-blocked and must be skipped before the request", reqs)
	}
	if n, nw := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("no_work notice raised %d time(s) without a single poll: %+v", n, nw)
	}
	n, rb := countNoticesByCode(d.notices, "runtime_blocked")
	if n != 1 {
		t.Fatalf("runtime_blocked notices = %d, want exactly 1", n)
	}
	if rb.Count != 1 {
		t.Errorf("runtime_blocked count = %d, want 1: the verdict is raised once, not per round", rb.Count)
	}
	if rb.ResolvedAt != nil {
		t.Errorf("runtime_blocked resolved while still blocked: %+v", rb)
	}
	if !strings.Contains(rb.Message, "No attached leaf can run on this machine") ||
		!strings.Contains(rb.Message, "heads trust") {
		t.Errorf("runtime_blocked message does not state the fact and the remedy: %q", rb.Message)
	}
	s := buf.String()
	if c := strings.Count(s, "connected but getting no work"); c != 0 {
		t.Errorf("'connected but getting no work' logged %d time(s) with zero requests", c)
	}
	if c := strings.Count(s, "no runnable leafs"); c != 1 {
		t.Errorf("'no runnable leafs' logged %d time(s), want exactly 1; log:\n%s", c, s)
	}
	if !strings.Contains(s, "has not trusted") {
		t.Errorf("the WARN does not name the trust gate; log:\n%s", s)
	}
}

// TestTB60_TransientSkipsStaySilent: a leaf paused after repeated local
// failures (TB-10) is skipped before the request; that condition has its own
// leaf_failing notice, so the request-less rounds it produces raise nothing.
// Pre-fix: five such rounds raised the generic "the head has no matching
// units" notice on top of it.
func TestTB60_TransientSkipsStaySilent(t *testing.T) {
	mc, requests := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.notices = NewNoticeLog()
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{{ID: "leaf-wasm", Slug: "leaf-wasm", Name: "Wasm", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"wasm": "https://example.org/leaf.wasm"}}}},
		DefaultWeights: map[string]int{"leaf-wasm": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-wasm": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	f.leafFailurePausedFn = func(string) bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	if reqs := requests(); len(reqs) != 0 {
		t.Errorf("RequestWorkUnit issued for a paused leaf: %v", reqs)
	}
	if n, nw := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("no_work raised %d time(s) for rounds that asked nothing: %+v", n, nw)
	}
	if n, rb := countNoticesByCode(d.notices, "runtime_blocked"); n != 0 {
		t.Errorf("runtime_blocked raised for a leaf that is merely paused: %+v", rb)
	}
	if c := strings.Count(buf.String(), "connected but getting no work"); c != 0 {
		t.Errorf("'connected but getting no work' logged %d time(s) with zero requests", c)
	}
}

// TestTB60_EmptyAnswersStillRaiseNoWork: rounds that DID ask and were answered
// empty still count, so a genuinely empty head raises the head-side wording
// once — and on a partly blocked host the notice says how many leafs were
// never asked for, since a leaf the machine cannot run explains part of the
// silence.
func TestTB60_EmptyAnswersStillRaiseNoWork(t *testing.T) {
	mc, requests := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.notices = NewNoticeLog()
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-container", Slug: "leaf-container", Name: "Container", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/img:tag"}},
			{ID: "leaf-wasm", Slug: "leaf-wasm", Name: "Wasm", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"wasm": "https://example.org/leaf.wasm"}}},
		},
		DefaultWeights: map[string]int{"leaf-container": 100, "leaf-wasm": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-container": 100, "leaf-wasm": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})

	_, buf := runFetcherFor(d, 400*time.Millisecond)

	reqs := requests()
	if reqs["leaf-container"] != 0 {
		t.Errorf("container leaf requested %d times with no container runtime", reqs["leaf-container"])
	}
	if reqs["leaf-wasm"] < noWorkWarnThreshold {
		t.Fatalf("wasm leaf requested only %d times; the streak needs %d answered polls", reqs["leaf-wasm"], noWorkWarnThreshold)
	}
	n, nw := countNoticesByCode(d.notices, "no_work")
	if n != 1 {
		t.Fatalf("no_work notices = %d, want exactly 1", n)
	}
	if !strings.Contains(nw.Message, "after repeated polls") {
		t.Errorf("no_work notice lacks the head-side wording: %q", nw.Message)
	}
	if !strings.Contains(nw.Message, "1 of the 2 attached leaf(s)") {
		t.Errorf("no_work notice does not say one leaf is never requested: %q", nw.Message)
	}
	if n, rb := countNoticesByCode(d.notices, "runtime_blocked"); n != 0 {
		t.Errorf("runtime_blocked raised on a host with a requestable leaf: %+v", rb)
	}
	if c := strings.Count(buf.String(), "connected but getting no work"); c != 1 {
		t.Errorf("'connected but getting no work' logged %d time(s), want exactly 1", c)
	}
}

// tb59Engine is a container engine a test can switch on: the factory's
// detection seam answers "none" until up is set, then a Docker engine; the
// construction seam returns a mock runtime named container.
type tb59Engine struct {
	mu sync.Mutex
	up bool
}

func (e *tb59Engine) set(up bool) { e.mu.Lock(); e.up = up; e.mu.Unlock() }

func (e *tb59Engine) factory(cfg *config.Config, logger *slog.Logger) *ContainerRuntimeFactory {
	return NewContainerRuntimeFactoryForTest(cfg, logger,
		func(runtime.ContainerBackend) runtime.BackendInfo {
			e.mu.Lock()
			defer e.mu.Unlock()
			if !e.up {
				return runtime.BackendInfo{Backend: runtime.BackendNone}
			}
			return runtime.BackendInfo{Backend: runtime.BackendDocker, Engine: "docker", Version: "27.1"}
		},
		func(runtime.BackendInfo) (runtime.Runtime, error) {
			return &mockRuntime{canHandle: true, name: "container"}, nil
		})
}

// TestTB59_LateEngineRegistersReadvertisesAndResolves is the reproduction with
// its outcome inverted. A daemon started with no engine registers WASM-only and
// raises the runtime-blocked notice (its only leaf needs a container). The
// engine then "appears": one detection attempt registers the container runtime,
// the notice resolves, the daemon reports the detected backend, and the next
// fetch round re-registers with the head — echoing the head's own host id and
// advertising CONTAINER — before it asks for the container leaf. Pre-fix none
// of this existed: the registry, the head's row and the notice were fixed for
// the daemon's lifetime.
func TestTB59_LateEngineRegistersReadvertisesAndResolves(t *testing.T) {
	mc, requests := tb49CountingHead()
	rc := &reRegMockClient{mockClient: mc,
		resp: &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}}
	head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.notices = NewNoticeLog()
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
	engine := &tb59Engine{}
	d.containerFactory = engine.factory(d.cfg, d.logger)

	// Phase 1: no engine. Nothing asked, the verdict notice live, a probe finds nothing.
	runFetcherFor(d, 150*time.Millisecond)
	if reqs := requests(); len(reqs) != 0 {
		t.Fatalf("requests issued with no container runtime: %v", reqs)
	}
	if n, rb := countNoticesByCode(d.notices, "runtime_blocked"); n != 1 || rb.ResolvedAt != nil {
		t.Fatalf("runtime_blocked before the engine: n=%d %+v", n, rb)
	}
	if !d.ContainerRedetectActive() {
		t.Fatal("ContainerRedetectActive = false; a head is trusted for CONTAINER and none is registered")
	}
	if d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = true with no engine")
	}
	if d.runtimeRegistry.GetRuntime("container") != nil {
		t.Fatal("container runtime registered with no engine")
	}
	if _, ok := d.ContainerBackend(); ok {
		t.Fatal("ContainerBackend reports a registered runtime with no engine")
	}

	// Phase 2: the engine appears. One attempt registers it and resolves the notice.
	engine.set(true)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false after the engine came up")
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Fatal("container runtime not registered after detection")
	}
	if b, ok := d.ContainerBackend(); !ok || b.Backend != runtime.BackendDocker || b.Version != "27.1" {
		t.Errorf("ContainerBackend = %+v, %v; want the detected Docker engine", b, ok)
	}
	if d.ContainerRedetectActive() {
		t.Error("ContainerRedetectActive still true after registration")
	}
	if _, rb := countNoticesByCode(d.notices, "runtime_blocked"); rb.ResolvedAt == nil {
		t.Errorf("runtime_blocked notice not resolved by the late registration: %+v", rb)
	}
	if got := d.advertisedRuntimesFor(head.Config); strings.Join(got, ",") != "CONTAINER,WASM" {
		t.Errorf("advertisedRuntimesFor = %v, want [CONTAINER WASM]", got)
	}

	// Phase 3: the next fetch round re-registers with the head, then asks for
	// the leaf it can now run.
	runFetcherFor(d, 150*time.Millisecond)
	if rc.lastReq == nil {
		t.Fatal("the head was never re-registered after the runtime change")
	}
	if rc.lastReq.HostId != "host-1" {
		t.Errorf("re-registration HostId = %q, want the head's own id host-1 (an update, not a new host)", rc.lastReq.HostId)
	}
	if got := strings.Join(rc.lastReq.AvailableRuntimes, ","); got != "CONTAINER,WASM" {
		t.Errorf("re-registration AvailableRuntimes = %q, want CONTAINER,WASM", got)
	}
	if reqs := requests(); reqs["leaf-container"] == 0 {
		t.Errorf("container leaf never requested after the runtime registered: %v", reqs)
	}
	d.readvertiseMu.Lock()
	pending := len(d.readvertisePending)
	d.readvertiseMu.Unlock()
	if pending != 0 {
		t.Errorf("%d head(s) still flagged for re-advertisement after success", pending)
	}
}

// TestTB59_ReadvertiseRetriesWhileHeadUnreachable: a head that refuses the
// re-registration stays flagged and is re-tried on its next contact, so a head
// that was down when the engine appeared still learns about it — and the
// host id is left untouched by the failure.
func TestTB59_ReadvertiseRetriesWhileHeadUnreachable(t *testing.T) {
	mc, _ := tb49CountingHead()
	rc := &reRegMockClient{mockClient: mc, err: context.DeadlineExceeded}
	head := &ServerConnection{Client: rc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "host-1",
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.notices = NewNoticeLog()
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})

	d.markRuntimesChanged()
	d.readvertiseIfPending(context.Background(), head)
	if rc.lastReq == nil {
		t.Fatal("no re-registration attempted")
	}
	d.readvertiseMu.Lock()
	stillPending := d.readvertisePending["server-a"]
	d.readvertiseMu.Unlock()
	if !stillPending {
		t.Error("a failed re-registration cleared the pending flag; the head would never learn the new runtimes")
	}
	if head.HostID != "host-1" {
		t.Errorf("HostID = %q after a failed re-registration, want host-1 unchanged", head.HostID)
	}

	// The head comes back: the retry succeeds and clears the flag.
	rc.err = nil
	rc.resp = &lettucev1.RegisterVolunteerResponse{VolunteerId: "vol-1", HostId: "host-1"}
	d.readvertiseIfPending(context.Background(), head)
	d.readvertiseMu.Lock()
	stillPending = d.readvertisePending["server-a"]
	d.readvertiseMu.Unlock()
	if stillPending {
		t.Error("a successful re-registration left the head flagged")
	}
}

// TestTB59_OnDemandProbeThroughRunLoop drives the daemon's own loop: Run starts
// the re-detection goroutine (no container runtime, a head trusted for
// CONTAINER), an on-demand request wakes it, and the engine that is up by then
// is registered — without waiting for the periodic tick and without a restart.
func TestTB59_OnDemandProbeThroughRunLoop(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(context.Context, *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{}, nil
		},
	}
	d := newTestDaemon(mc, &mockRuntime{canHandle: true, name: "wasm"})
	d.cfg.Servers = []config.ServerConfig{{Name: "default", GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	engine := &tb59Engine{}
	d.containerFactory = engine.factory(d.cfg, d.logger)

	if err := d.RequestContainerRedetect(); err != nil {
		t.Fatalf("RequestContainerRedetect before Run: %v (the request must queue for the loop)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// The queued request fires as soon as the loop starts; with no engine it
	// finds nothing. Then the engine comes up and a second request registers it.
	deadline := time.Now().Add(2 * time.Second)
	for !d.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	engine.set(true)
	if err := d.RequestContainerRedetect(); err != nil {
		t.Fatalf("RequestContainerRedetect while running: %v", err)
	}
	for d.runtimeRegistry.GetRuntime("container") == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.runtimeRegistry.GetRuntime("container") == nil {
		t.Fatal("the loop never registered the container runtime after the on-demand probe")
	}
	if err := d.RequestContainerRedetect(); err != ErrContainerRuntimeRegistered {
		t.Errorf("RequestContainerRedetect after registration = %v, want ErrContainerRuntimeRegistered", err)
	}
	cancel()
	<-done
}

// TestRuntimeRegistry_ConcurrentRegisterAndRead: the registry is now written
// after start (a late container runtime) while the fetcher, slots and the
// management API read it; every method must take the lock.
func TestRuntimeRegistry_ConcurrentRegisterAndRead(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&mockRuntime{canHandle: true, name: "wasm"})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = r.GetRuntime("container")
				_ = r.AvailableRuntimes()
				_, _ = r.SelectRuntime(&runtime.WorkUnit{Runtime: "wasm"})
			}
		}()
	}
	for i := 0; i < 200; i++ {
		r.Register(&mockRuntime{canHandle: true, name: "container"})
	}
	close(stop)
	wg.Wait()
	if r.GetRuntime("container") == nil {
		t.Fatal("container runtime missing after concurrent registration")
	}
}
