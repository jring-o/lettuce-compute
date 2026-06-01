package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
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

func newFetcherTestDaemon(servers []*ServerConnection) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false

	mr := &mockRuntime{canHandle: true}
	registry := NewRuntimeRegistry()
	registry.Register(mr)

	multiClient := NewMultiServerClient(servers, logger)
	leafCache := NewLeafCache(5*time.Minute, logger)
	ws := NewWeightedSelector()

	d := &Daemon{
		cfg:              cfg,
		pubKey:           pub,
		privKey:          priv,
		multiClient:      multiClient,
		runtimeRegistry:  registry,
		logger:           logger,
		initialBackoff:   10 * time.Millisecond,
		maxBackoff:       50 * time.Millisecond,
		cachedHW:         nil,
		leafCache:        leafCache,
		weightedSelector: ws,
		userPauseCh:      make(chan bool, 1),
		// Disable the fetcher inter-request throttle by default; individual tests
		// set fetcher.minInterval explicitly where they need a specific value.
		fetcherMinInterval: -1,
	}

	// Set up leaf cache so weighted selection works.
	for _, srv := range servers {
		leafCache.PopulateForTest(srv.Name, &CachedHeadInfo{
			Name: srv.Name,
			Leafs: []CachedLeafInfo{
				{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
			},
			DefaultWeights: map[string]int{"leaf-1": 100},
		})
		ws.SetLeafWeights(srv.Name, map[string]int{"leaf-1": 100})
	}
	headWeights := make(map[string]int)
	for _, srv := range servers {
		headWeights[srv.Name] = 100
	}
	ws.SetHeadWeights(headWeights)

	return d
}

func TestFetcher_FillsQueue(t *testing.T) {
	wuCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			wuCount++
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", wuCount),
				ProjectId:                "proj-1",
				Runtime:                  "native",
				InputData:                []byte("input"),
				HeartbeatIntervalSeconds: 300,
				ExecutionSpec:            &lettucev1.ExecutionSpec{},
			}, nil
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(4, d.logger)

	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.maxBackoff = 50 * time.Millisecond
	fetcher.minInterval = 0 // disable the inter-request throttle for fast tests

	ctx, cancel := context.WithCancel(context.Background())

	go fetcher.Run(ctx)

	// Wait until queue reaches maxDepth.
	deadline := time.After(5 * time.Second)
	for {
		if queue.Len() >= 4 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("queue only reached %d items, wanted 4", queue.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	if queue.Len() < 4 {
		t.Errorf("queue length = %d, want >= 4", queue.Len())
	}
}

func TestFetcher_BacksOffWhenNoWork(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			return nil, status.Error(codes.NotFound, "no work")
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)

	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.maxBackoff = 50 * time.Millisecond
	fetcher.minInterval = 0 // disable the inter-request throttle for fast tests

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	fetcher.Run(ctx)

	// Queue should still be empty.
	if queue.Len() != 0 {
		t.Errorf("queue length = %d, want 0", queue.Len())
	}

	// Should have been called multiple times (backoff, not stuck).
	if callCount < 2 {
		t.Errorf("server called %d times, expected > 1 (should retry with backoff)", callCount)
	}
}

func TestFetcher_StopsWhenContextCancelled(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.minInterval = 0 // disable the inter-request throttle for fast tests

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	done := make(chan struct{})
	go func() {
		fetcher.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// OK — fetcher exited.
	case <-time.After(1 * time.Second):
		t.Fatal("fetcher did not stop within 1 second")
	}
}

// --- THROTTLE + ESCALATION tests (#18 fix 2 / #15 fix 4) ---

// lockedWriter serializes writes to an underlying buffer so the prepare
// heartbeat goroutine and the Run goroutine don't race on the log buffer when a
// test inspects emitted log lines.
type lockedWriter struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// nativeLeafServer builds the single-native-leaf test fixture used by several
// fetcher tests: a mockClient plus a one-native-leaf head info, returned ready
// to drop into newFetcherTestDaemon (which seeds the leaf cache from "leaf-1").
func nativeLeafServer(mc *mockClient) []*ServerConnection {
	if mc.getHeadInfoFn == nil {
		mc.getHeadInfoFn = func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		}
	}
	return []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}
}

// nativeWURespFn returns a RequestWorkUnit handler that always serves a native
// work unit with a fresh canonical UUID, counting how many it served.
func nativeWURespFn(count *int) func(context.Context, *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	return func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		*count++
		return &lettucev1.RequestWorkUnitResponse{
			WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", *count),
			ProjectId:                "proj-1",
			Runtime:                  "native",
			InputData:                []byte("input"),
			HeartbeatIntervalSeconds: 300,
			ExecutionSpec:            &lettucev1.ExecutionSpec{},
		}, nil
	}
}

// TestFetcher_ThrottlesRequestRate verifies the inter-request gate caps how
// often RequestWorkUnit is issued. With a 50ms floor over a ~250ms window the
// fetcher must issue far fewer requests than an ungated busy loop would.
func TestFetcher_ThrottlesRequestRate(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	// Make the per-empty backoff negligible so the inter-request floor is the
	// only thing pacing the loop, then assert the floor actually paces it.
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 1 * time.Millisecond
	fetcher.minInterval = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	calls := mc.getRequestCalls()
	if calls == 0 {
		t.Fatalf("expected some requests, got 0")
	}
	// 250ms / 50ms = 5 cycles; allow generous scheduling slack but it must be
	// well below the hundreds a 1ms-backoff loop would produce without the gate.
	if calls > 12 {
		t.Errorf("throttle not honored: %d requests in ~250ms with a 50ms floor (want <= 12)", calls)
	}
}

// TestFetcher_BacksOffOnResourceExhausted verifies a ResourceExhausted reply is
// treated as a rate-limit backoff (dedicated floor, head parked) rather than a
// NotFound (no backoff) or a tight loop.
func TestFetcher_BacksOffOnResourceExhausted(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 200 * time.Millisecond
	fetcher.minInterval = 0 // isolate the per-head rate-limit backoff
	fetcher.rateLimitBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	// The head is parked with the 100ms rate-limit floor each time it answers,
	// so over ~250ms we expect only a couple of requests, not a tight loop.
	calls := mc.getRequestCalls()
	if calls == 0 {
		t.Fatalf("expected at least one request, got 0")
	}
	if calls > 6 {
		t.Errorf("ResourceExhausted did not back off: %d requests in ~250ms with a 100ms floor (want <= 6)", calls)
	}

	// And the head must be parked with at least the rate-limit floor (proving it
	// was not mistaken for NotFound, which zeroes Backoff).
	srv := servers[0]
	if srv.Available {
		t.Errorf("head should be marked unavailable after ResourceExhausted")
	}
	if srv.Backoff < fetcher.rateLimitBackoff {
		t.Errorf("head backoff = %v, want >= rateLimitBackoff %v", srv.Backoff, fetcher.rateLimitBackoff)
	}
}

// TestFetcher_EscalatesAfterRepeatedPrepareFailures (Test A): when Prepare keeps
// failing for a runtime, the breaker trips at exactly the threshold — the
// volunteer grabs and abandons exactly `threshold` units, then stops requesting
// that runtime's leafs and emits the loud WARN exactly once.
func TestFetcher_EscalatesAfterRepeatedPrepareFailures(t *testing.T) {
	lw := &lockedWriter{buf: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(lw, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reqCount := 0
	mc := &mockClient{requestWorkUnitFn: nativeWURespFn(&reqCount)}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	d.logger = logger
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			return nil, fmt.Errorf("prepare boom")
		},
	})

	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.logger = logger
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	fetcher.minInterval = 0

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	// Exactly `threshold` units were grabbed (each requested + prepared +
	// abandoned) before the runtime got paused and further requests were skipped.
	if got := mc.getRequestCalls(); got != runtimeAbandonPauseThreshold {
		t.Errorf("request calls = %d, want %d (should stop after pausing the runtime)", got, runtimeAbandonPauseThreshold)
	}
	if got := mc.getAbandonCalls(); got != runtimeAbandonPauseThreshold {
		t.Errorf("abandon calls = %d, want %d", got, runtimeAbandonPauseThreshold)
	}

	// The runtime must now be paused.
	if !fetcher.runtimePaused("native") {
		t.Errorf("native runtime should be paused after %d abandons", runtimeAbandonPauseThreshold)
	}

	// The loud WARN must have fired exactly once.
	logged := lw.String()
	if n := strings.Count(logged, "pausing leaf requests that need it"); n != 1 {
		t.Errorf("loud pause WARN fired %d times, want exactly 1\nlogs:\n%s", n, logged)
	}
}

// TestFetcher_EscalationResetsOnSuccess (Test B): a couple of prepare failures
// followed by a success never trips the breaker, and the counter resets.
func TestFetcher_EscalationResetsOnSuccess(t *testing.T) {
	reqCount := 0
	mc := &mockClient{requestWorkUnitFn: nativeWURespFn(&reqCount)}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	prepareAttempt := 0
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			prepareAttempt++
			if prepareAttempt <= 2 {
				return nil, fmt.Errorf("transient prepare blip %d", prepareAttempt)
			}
			return &runtime.PrepareResult{WorkDir: "/tmp/work"}, nil
		},
	})

	queue := NewPreFetchQueue(8, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	fetcher.minInterval = 0

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	if fetcher.runtimePaused("native") {
		t.Errorf("native runtime should NOT be paused: 2 failures then a success must reset")
	}
	// After the third (successful) prepare, the counter is cleared.
	if c := fetcher.runtimeAbandons["native"]; c != 0 {
		t.Errorf("abandon counter = %d, want 0 after a successful prepare reset", c)
	}
}

// TestFetcher_EscalationCooldownReprobe (Test C): once the cooldown elapses, the
// paused runtime is re-probed. Uses the fetcher.now clock seam to advance time
// without sleeping.
func TestFetcher_EscalationCooldownReprobe(t *testing.T) {
	reqCount := 0
	mc := &mockClient{requestWorkUnitFn: nativeWURespFn(&reqCount)}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			return nil, fmt.Errorf("prepare boom")
		},
	})

	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	fetcher.minInterval = 0

	base := time.Now()
	fetcher.now = func() time.Time { return base }

	// First run: trip the breaker (threshold abandons), then it pauses and stops.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	fetcher.Run(ctx1)
	cancel1()

	if !fetcher.runtimePaused("native") {
		t.Fatalf("native should be paused after first run")
	}
	firstReqs := mc.getRequestCalls()
	if firstReqs != runtimeAbandonPauseThreshold {
		t.Fatalf("first-run requests = %d, want %d", firstReqs, runtimeAbandonPauseThreshold)
	}

	// Advance the clock past the cooldown so the runtime is re-probed.
	fetcher.now = func() time.Time { return base.Add(runtimeAbandonCooldown + time.Minute) }

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	fetcher.Run(ctx2)
	cancel2()

	// It must have issued more requests in the second window (re-probed), and
	// re-tripped (threshold more abandons before pausing again).
	if mc.getRequestCalls() <= firstReqs {
		t.Errorf("expected re-probe requests after cooldown: before=%d after=%d", firstReqs, mc.getRequestCalls())
	}
	if got := mc.getRequestCalls(); got != 2*runtimeAbandonPauseThreshold {
		t.Errorf("total requests after re-probe = %d, want %d (re-tripped)", got, 2*runtimeAbandonPauseThreshold)
	}
}

// TestFetcher_SkipsPausedRuntimeLeafOnly (Test D): with a native leaf and a
// container leaf, and only the native runtime registered, the container leaf is
// skipped after its runtime trips while the native leaf keeps getting work.
func TestFetcher_SkipsPausedRuntimeLeafOnly(t *testing.T) {
	var nativeReqs, containerReqs int
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			// The fetcher requests a single leaf per call via LeafIds.
			leaf := ""
			if len(req.LeafIds) > 0 {
				leaf = req.LeafIds[0]
			}
			if leaf == "leaf-native" {
				nativeReqs++
				return &lettucev1.RequestWorkUnitResponse{
					WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", nativeReqs),
					ProjectId:                "proj-1",
					Runtime:                  "native",
					HeartbeatIntervalSeconds: 300,
					ExecutionSpec:            &lettucev1.ExecutionSpec{},
				}, nil
			}
			// container leaf: serve a container WU so SelectRuntime fails (no
			// container runtime registered) -> abandon -> escalate.
			containerReqs++
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId:               fmt.Sprintf("00000000-0000-4000-9000-%012d", containerReqs),
				ProjectId:                "proj-1",
				Runtime:                  "container",
				HeartbeatIntervalSeconds: 300,
				ExecutionSpec:            &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:tag"},
			}, nil
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-native", Slug: "leaf-native", Name: "Native", State: "ACTIVE"},
					{Id: "leaf-container", Slug: "leaf-container", Name: "Container", State: "ACTIVE",
						ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:tag"}},
				},
			}, nil
		},
	}
	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	// Replace the single-leaf cache with the two-leaf head info.
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-native", Slug: "leaf-native", Name: "Native", State: "ACTIVE"},
			{ID: "leaf-container", Slug: "leaf-container", Name: "Container", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/img:tag"}},
		},
		DefaultWeights: map[string]int{"leaf-native": 100, "leaf-container": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-native": 100, "leaf-container": 100})

	// Only the native runtime is registered, so container WUs can't be handled.
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "native"})

	queue := NewPreFetchQueue(64, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	fetcher.minInterval = 0

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	// Container runtime should have tripped and been paused.
	if !fetcher.runtimePaused("container") {
		t.Errorf("container runtime should be paused")
	}
	// Container leaf must have been requested only up to the threshold, then
	// skipped (pre-request) forever after.
	if containerReqs != runtimeAbandonPauseThreshold {
		t.Errorf("container requests = %d, want %d (then skipped)", containerReqs, runtimeAbandonPauseThreshold)
	}
	// Native must NOT be paused and must keep getting work well past the
	// container's threshold.
	if fetcher.runtimePaused("native") {
		t.Errorf("native runtime should not be paused")
	}
	if nativeReqs <= runtimeAbandonPauseThreshold {
		t.Errorf("native requests = %d, want > %d (native keeps fetching while container is paused)", nativeReqs, runtimeAbandonPauseThreshold)
	}
}
