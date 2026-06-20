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
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", wuCount),
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
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
	// Drive "full" off the queue's hard depth so this test exercises queue
	// filling rather than the hours-based buffer target.
	fetcher.workBufferFullFn = queue.IsFull

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

// TestBufferBatch_SkipsAlreadyHeldUnits verifies the client never re-buffers (and so
// never re-runs) a work unit it already holds in its prefetch buffer or active slots,
// nor a duplicate appearing twice within one batch. Running a duplicate is pure waste:
// its result is rejected by the head as a duplicate from the same volunteer.
func TestBufferBatch_SkipsAlreadyHeldUnits(t *testing.T) {
	servers := []*ServerConnection{
		{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}
	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(8, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)

	const heldID = "00000000-0000-4000-8000-000000000001"
	const freshID = "00000000-0000-4000-8000-000000000002"
	fetcher.heldWorkUnitIDsFn = func() []string { return []string{heldID} }

	leaf := CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}
	mkAsg := func(id string) *lettucev1.WorkUnitAssignment {
		return &lettucev1.WorkUnitAssignment{
			WorkUnitId:    id,
			LeafId:        "leaf-1",
			Runtime:       "native",
			InputData:     []byte("input"),
			ExecutionSpec: &lettucev1.ExecutionSpec{},
		}
	}
	// heldID is already held; freshID appears twice (an intra-batch duplicate).
	pushed := fetcher.bufferBatch(context.Background(), servers[0], leaf,
		[]*lettucev1.WorkUnitAssignment{mkAsg(heldID), mkAsg(freshID), mkAsg(freshID)})

	if pushed != 1 {
		t.Fatalf("bufferBatch pushed %d units, want 1 (held unit + intra-batch duplicate skipped)", pushed)
	}
	if queue.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", queue.Len())
	}
}

func TestFetcher_BacksOffWhenNoWork(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			// No-work is an OK reply with empty assignments and a zero retry delay,
			// so the fetcher keeps polling (paced by the small no-work floor).
			return &lettucev1.RequestWorkUnitResponse{Assignments: nil, RetryAfterSeconds: 0}, nil
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	done := make(chan struct{})
	go func() {
		fetcher.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// OK â€” fetcher exited.
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
			Assignments: []*lettucev1.WorkUnitAssignment{
				{
					WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", *count),
					LeafId:                   "proj-1",
					Runtime:                  "native",
					InputData:                []byte("input"),
					ExecutionSpec:            &lettucev1.ExecutionSpec{},
				},
			},
		}, nil
	}
}

// TestFetcher_ObeysServerRetryDelay (DoD #1): when a head returns a large
// retry_after_seconds, the fetcher issues exactly one RequestWorkUnit and then
// makes no further calls until the clock advances past the delay. The fetcher's
// `now` clock seam drives time so the test does not sleep for the real delay.
func TestFetcher_ObeysServerRetryDelay(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			// No work, but a 600s server-directed delay on the (OK) reply.
			return &lettucev1.RequestWorkUnitResponse{
				Assignments:       nil,
				RetryAfterSeconds: 600,
			}, nil
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(4, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 1 * time.Millisecond

	base := time.Now()
	fetcher.now = func() time.Time { return base }

	// Run a short window; the head answers once then must be parked for 600s.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	fetcher.Run(ctx)
	cancel()

	if calls := mc.getRequestCalls(); calls != 1 {
		t.Fatalf("with a 600s server-directed delay the fetcher made %d requests, want exactly 1", calls)
	}
	srv := servers[0]
	if !srv.NextContactAt.Equal(base.Add(600 * time.Second)) {
		t.Errorf("NextContactAt = %v, want base+600s", srv.NextContactAt)
	}

	// Advancing past the delay re-enables contact: another window yields more calls.
	fetcher.now = func() time.Time { return base.Add(601 * time.Second) }
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	fetcher.Run(ctx2)
	cancel2()
	if calls := mc.getRequestCalls(); calls <= 1 {
		t.Errorf("after the delay elapsed the fetcher made %d total requests, want > 1", calls)
	}
}

// TestFetcher_BacksOffOnResourceExhausted verifies a ResourceExhausted reply is
// treated as a fixed jittered LOCAL backoff (parking the head via NextContactAt),
// not a tight loop and not a server-directed value.
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
	fetcher.rateLimitBackoff = 100 * time.Millisecond

	base := time.Now()
	fetcher.now = func() time.Time { return base }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	// The head is parked via NextContactAt for ~the rate-limit floor (jittered),
	// so over a fixed clock it is contacted exactly once.
	calls := mc.getRequestCalls()
	if calls == 0 {
		t.Fatalf("expected at least one request, got 0")
	}
	if calls != 1 {
		t.Errorf("ResourceExhausted did not park the head: %d requests with a fixed clock (want 1)", calls)
	}

	// NextContactAt must be pushed out by roughly the rate-limit floor (±20%
	// jitter), proving a fixed LOCAL backoff was applied rather than nothing.
	srv := servers[0]
	gotDelay := srv.NextContactAt.Sub(base)
	lo := time.Duration(float64(fetcher.rateLimitBackoff) * 0.8)
	hi := time.Duration(float64(fetcher.rateLimitBackoff) * 1.2)
	if gotDelay < lo || gotDelay > hi {
		t.Errorf("ResourceExhausted local backoff = %v, want within ±20%% of %v", gotDelay, fetcher.rateLimitBackoff)
	}
}

// TestFetcher_EscalatesAfterRepeatedPrepareFailures (Test A): when Prepare keeps
// failing for a runtime, the breaker trips at exactly the threshold â€” the
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
					Assignments: []*lettucev1.WorkUnitAssignment{
						{
							WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", nativeReqs),
							LeafId:                   "proj-1",
							Runtime:                  "native",
							ExecutionSpec:            &lettucev1.ExecutionSpec{},
						},
					},
				}, nil
			}
			// container leaf: serve a container WU so SelectRuntime fails (no
			// container runtime registered) -> abandon -> escalate.
			containerReqs++
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:               fmt.Sprintf("00000000-0000-4000-9000-%012d", containerReqs),
						LeafId:                   "proj-1",
						Runtime:                  "container",
						ExecutionSpec:            &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:tag"},
					},
				},
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

// --- CLIENT WORK BUFFER tests (Layer 1) ---

// TestFetcher_ZeroRequestsWhenBufferFull (DoD #2): when the client work buffer
// reports full, the fetcher issues ZERO RequestWorkUnit calls for the whole
// window.
func TestFetcher_ZeroRequestsWhenBufferFull(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			t.Error("RequestWorkUnit must not be called while the buffer is full")
			return &lettucev1.RequestWorkUnitResponse{}, nil
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(8, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	// Force the buffer to report full for the whole window.
	fetcher.workBufferFullFn = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	if calls := mc.getRequestCalls(); calls != 0 {
		t.Errorf("RequestWorkUnit calls while buffer full = %d, want 0", calls)
	}
}

// TestFetcher_BatchPushesAllAndRecordsEach: a single RequestWorkUnit that returns
// N assignments must push N descriptors into the buffer and record N assignments
// with the selector (RecordAssignment once per unit).
func TestFetcher_BatchPushesAllAndRecordsEach(t *testing.T) {
	const batch = 3
	served := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if served {
				return &lettucev1.RequestWorkUnitResponse{Assignments: nil}, nil
			}
			served = true
			asgs := make([]*lettucev1.WorkUnitAssignment, batch)
			for i := 0; i < batch; i++ {
				asgs[i] = &lettucev1.WorkUnitAssignment{
					WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
					LeafId:                   "proj-1",
					Runtime:                  "native",
					ExecutionSpec:            &lettucev1.ExecutionSpec{},
				}
			}
			return &lettucev1.RequestWorkUnitResponse{Assignments: asgs, RetryAfterSeconds: 600}, nil
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(16, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond
	// Never "full" so the single batch is fully buffered.
	fetcher.workBufferFullFn = func() bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	if got := queue.Len(); got != batch {
		t.Errorf("queue length = %d, want %d (every assignment in the batch buffered)", got, batch)
	}
	// RecordAssignment increments the selector's assigned count per unit.
	if got := d.weightedSelector.AssignedCount("server-a", "leaf-1"); got != batch {
		t.Errorf("RecordAssignment count = %d, want %d (once per buffered unit)", got, batch)
	}
}

// TestFetcher_RequestsMaxAssignments: the fetcher asks for more than one
// assignment when the hours buffer is below target and a per-unit estimate is
// available, and obeys the maxBatchPerRequest cap.
func TestFetcher_RequestsMaxAssignments(t *testing.T) {
	var gotMax int32 = -1
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if gotMax < 0 {
				gotMax = req.MaxAssignments
			}
			return &lettucev1.RequestWorkUnitResponse{Assignments: nil, RetryAfterSeconds: 600}, nil
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	// No benchmark and no buffered work → no per-unit estimate → batch sizer
	// requests a full batch to refill the empty buffer quickly.
	queue := NewPreFetchQueue(16, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.maxBackoff = 2 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	if gotMax != maxBatchPerRequest {
		t.Errorf("MaxAssignments requested = %d, want %d (full batch when buffer empty and no estimate)", gotMax, maxBatchPerRequest)
	}
}

// TestFetcher_NextContactAtSurvivesRecreate: NextContactAt lives on the
// ServerConnection, so a fetcher recreated (pause/resume) still honors a
// previously stamped server-directed retry delay.
func TestFetcher_NextContactAtSurvivesRecreate(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{Assignments: nil, RetryAfterSeconds: 600}, nil
		},
	}
	servers := nativeLeafServer(mc)

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(4, d.logger)

	base := time.Now()
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 1 * time.Millisecond
	fetcher.now = func() time.Time { return base }

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	fetcher.Run(ctx)
	cancel()

	if mc.getRequestCalls() != 1 {
		t.Fatalf("first fetcher requests = %d, want 1", mc.getRequestCalls())
	}

	// Recreate the fetcher (as on resume). The per-head NextContactAt persists on
	// the ServerConnection, so the recreated fetcher must still wait it out.
	fetcher2 := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher2.backoff = 1 * time.Millisecond
	fetcher2.now = func() time.Time { return base.Add(1 * time.Second) } // still < 600s

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	fetcher2.Run(ctx2)
	cancel2()

	if mc.getRequestCalls() != 1 {
		t.Errorf("after recreate, total requests = %d, want 1 (recreated fetcher still obeys the prior delay)", mc.getRequestCalls())
	}
}
