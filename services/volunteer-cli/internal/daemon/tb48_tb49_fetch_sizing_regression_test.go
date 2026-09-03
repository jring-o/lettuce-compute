package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-48 / TB-49 regression tests. Both bugs sit in the fetcher's per-leaf loop and
// the buffer sizing it consults, and both were traced head-side on the fleet's only
// GPU host (2026-09-02/03) — hence one batch.
//
// TB-48: the hours target and the batch ask were work_buffer_hours × max_concurrent_tasks
// with no per-resource term. GPU-required units occupy at most one slot per physical
// GPU (canAccommodateWU's exclusivity guard), so a one-GPU host with eight slots
// buffered sixteen hours of GPU units of which one ran; the rest sat until the
// 90 %-of-deadline drop and the head's in-flight cap filled with copies only one slot
// could ever drain. The tests here use only the pre-existing entry points
// (bufferAccepts, requestAndBuffer, Run) so they compile — and fail — against the
// pre-fix code.
//
// TB-49: the loop issued RequestWorkUnit for leafs whose runtime this machine never
// advertised to the head (not registered here, or the head untrusted for it). The
// head refused every such request — correctly — and fired its capability-mismatch
// WARN each time, so a healthy container-only host asking for the native leaf every
// round buried the tell for real misconfiguration (231 lines in 30 h on one head).

// tb48GPUHost is the tester's shape: work_buffer_hours 2, eight slots, ONE GPU,
// benchmark 1 (so est seconds == rsc_fpops_est).
func tb48GPUHost(t *testing.T) *Daemon {
	t.Helper()
	d := newBufferTestDaemon(t, 2.0, 8, 1.0)
	d.cachedHW = &lettucev1.HardwareCapabilities{Gpus: []*lettucev1.GpuInfo{{Model: "one-gpu"}}}
	return d
}

func tb48GPUUnit(id string, fpops float64) *runtime.WorkUnit {
	return &runtime.WorkUnit{ID: id, LeafID: "leaf-gpu", RscFpopsEst: fpops,
		ExecutionSpec: runtime.ExecutionSpec{GPURequired: true}}
}

// tb48FetchHost reshapes a fetcher-test daemon into the one-GPU eight-slot host, with
// the fetcher's queue being the daemon's own so the sizing sees what was buffered.
func tb48FetchHost(t *testing.T, mc *mockClient) (*Daemon, *ServerConnection) {
	t.Helper()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "gpu-head", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = 2
	d.cfg.MaxConcurrentTasks = 8
	d.benchmarkFPOPS = 1.0
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.cachedHW = &lettucev1.HardwareCapabilities{Gpus: []*lettucev1.GpuInfo{{Model: "one-gpu"}}}
	return d, servers[0]
}

// TestTB48_BufferAcceptsBoundsGPUUnitsByGPUCount: on the one-GPU, eight-slot host
// the global target is 16 h, but GPU units drain through one slot, so their own
// target is 2 h. A second 3 h GPU unit must be refused as over the GPU target
// (pre-fix: four in a row were accepted with no objection — the filed scratch
// test), while a CPU unit of the same size is still accepted, because the global
// target is what governs it.
func TestTB48_BufferAcceptsBoundsGPUUnitsByGPUCount(t *testing.T) {
	d := tb48GPUHost(t)
	if got := d.bufferTargetSeconds(); got != 57600 {
		t.Fatalf("global target = %g, want 57600 (2 h × 8 slots) — harness drift", got)
	}

	first := tb48GPUUnit("00000000-0000-4000-8000-000000000001", 10800)
	if ok, reason := d.bufferAccepts(first); !ok {
		t.Fatalf("first GPU unit refused (%s): the GPU target must admit at least its first unit", reason)
	}
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: first}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	second := tb48GPUUnit("00000000-0000-4000-8000-000000000002", 10800)
	ok, reason := d.bufferAccepts(second)
	if ok {
		t.Error("second 3 h GPU unit accepted with 3 h of GPU work already held against a 2 h GPU target (one GPU) — TB-48")
	} else if !strings.Contains(reason, "GPU") {
		t.Errorf("refusal reason %q does not name the GPU buffer", reason)
	}

	cpu := &runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000003", LeafID: "leaf-cpu", RscFpopsEst: 10800}
	if ok, reason := d.bufferAccepts(cpu); !ok {
		t.Errorf("CPU unit refused (%s) at 3 h held against a 16 h global target — the GPU bound must not tighten the CPU class", reason)
	}
}

// TestTB48_GPULeafAskSizedByGPUDeficit reads the ask the fetcher actually puts on the
// wire (MaxAssignments) for leafs of each class on the one-GPU host with an empty
// buffer. Pre-fix every ask divided the 16 h global deficit: 5 three-hour GPU units
// for one GPU, the 64-unit ceiling for ten-minute GPU units, and a full 64-unit batch
// for a GPU leaf with no estimate at all — the "requested": 64 the head logged every
// five minutes while refusing him for his in-flight cap.
func TestTB48_GPULeafAskSizedByGPUDeficit(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int32{}
	mc := &mockClient{}
	mc.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		if len(req.LeafIds) == 1 {
			asked[req.LeafIds[0]] = req.MaxAssignments
		}
		mu.Unlock()
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	d, head := tb48FetchHost(t, mc)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)

	gpuSpec := &CachedExecutionSpec{GPURequired: true}
	for _, tc := range []struct {
		leaf CachedLeafInfo
		want int32
		why  string
	}{
		{CachedLeafInfo{ID: "gpu-3h", Slug: "gpu-3h", ExecutionSpec: gpuSpec, EstimatedDurationSeconds: 10800},
			1, "2 h GPU target ÷ 3 h per unit (pre-fix 16 h ÷ 3 h = 5)"},
		{CachedLeafInfo{ID: "gpu-10m", Slug: "gpu-10m", ExecutionSpec: gpuSpec, EstimatedDurationSeconds: 600},
			12, "2 h GPU target ÷ 10 min per unit (pre-fix the 64 ceiling)"},
		{CachedLeafInfo{ID: "gpu-unknown", Slug: "gpu-unknown", ExecutionSpec: gpuSpec},
			2, "no estimate: the GPU unit-count fallback, 2 per GPU slot (pre-fix a full 64-unit batch)"},
		{CachedLeafInfo{ID: "cpu-3h", Slug: "cpu-3h", EstimatedDurationSeconds: 10800},
			5, "16 h global target ÷ 3 h per unit — CPU leafs keep the slot-count target"},
	} {
		f.requestAndBuffer(context.Background(), head, tc.leaf, []string{tc.leaf.ID}, nil)
		mu.Lock()
		got, seen := asked[tc.leaf.ID]
		mu.Unlock()
		if !seen {
			t.Errorf("%s: no RequestWorkUnit reached the head", tc.leaf.Slug)
			continue
		}
		if got != tc.want {
			t.Errorf("ask for %s = %d, want %d (%s)", tc.leaf.Slug, got, tc.want, tc.why)
		}
	}
}

// TestTB48_FetcherStopsAskingWhenGPUClassIsFull: with 2 h of GPU units already held
// (twelve 10-minute units) on the one-GPU host, the global buffer is far from full
// (2 h of 16 h), so pre-fix the fetcher asked the head for the GPU leaf every round.
// Against a head with nothing to give (his, once his in-flight cap was full of
// exactly these units) that was one refused RPC per round, and after five of them
// the "connected but getting no work" WARN fired for a machine whose only condition
// was a full GPU buffer. Post-fix the leaf is skipped before the request: zero RPCs,
// no WARN, the twelve units untouched.
func TestTB48_FetcherStopsAskingWhenGPUClassIsFull(t *testing.T) {
	mc := &mockClient{}
	mc.requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	d, _ := tb48FetchHost(t, mc)
	d.leafCache.PopulateForTest("gpu-head", &CachedHeadInfo{
		Name: "gpu-head",
		Leafs: []CachedLeafInfo{{ID: "leaf-gpu", Slug: "leaf-gpu", Name: "GPU", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{GPURequired: true}, EstimatedDurationSeconds: 600}},
		DefaultWeights: map[string]int{"leaf-gpu": 100},
	})
	d.weightedSelector.SetLeafWeights("gpu-head", map[string]int{"leaf-gpu": 100})
	for i := 1; i <= 12; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: tb48GPUUnit(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), 600)}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if d.workBufferFull() {
		t.Fatal("global buffer reports full at 2 h of 16 h — state does not reproduce the defect")
	}

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	if calls := mc.getRequestCalls(); calls != 0 {
		t.Errorf("RequestWorkUnit calls = %d, want 0: 2 h of GPU units are held for one GPU, so asking for more only produces refusals (TB-48)", calls)
	}
	if n := strings.Count(buf.String(), "connected but getting no work"); n != 0 {
		t.Errorf("no-work WARN fired %d time(s) for a full GPU buffer — a full buffer is not an empty head", n)
	}
	if got := d.prefetchQueue.Len(); got != 12 {
		t.Errorf("buffer holds %d units, want the 12 preloaded (nothing accepted, nothing dropped)", got)
	}
}

// tb49TwoLeafHead registers a native-only leaf (native binaries, no wasm) and a
// container leaf on the named head — the lbry shape: beyblade-arena-native beside
// its container sibling.
func tb49TwoLeafHead(d *Daemon, head string) {
	d.leafCache.PopulateForTest(head, &CachedHeadInfo{
		Name: head,
		Leafs: []CachedLeafInfo{
			{ID: "leaf-native", Slug: "leaf-native", Name: "Native", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/native"}}},
			{ID: "leaf-container", Slug: "leaf-container", Name: "Container", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Image: "ghcr.io/example/img:tag"}},
		},
		DefaultWeights: map[string]int{"leaf-native": 100, "leaf-container": 100},
	})
	d.weightedSelector.SetLeafWeights(head, map[string]int{"leaf-native": 100, "leaf-container": 100})
}

// tb49CountingHead is a head that answers every request EMPTY — what a v0.10.4+ head
// does on a capability mismatch: no error, no units, one WARN in its own log — and
// counts requests per leaf id.
func tb49CountingHead() (*mockClient, func() map[string]int) {
	var mu sync.Mutex
	reqs := map[string]int{}
	mc := &mockClient{}
	mc.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		for _, id := range req.LeafIds {
			reqs[id]++
		}
		mu.Unlock()
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	return mc, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(reqs))
		for k, v := range reqs {
			out[k] = v
		}
		return out
	}
}

// TestTB49_FetcherNeverRequestsLeafForUnregisteredRuntime is the filed scratch test
// with its assertion inverted: two cached leafs, only the container runtime
// registered (a container-only host), a head that answers empty. Pre-fix both leafs
// were requested every round (200 each in 300 ms of Run); the runtime breaker never
// saw an abandon — the head served nothing to abandon — so nothing ever stopped it.
func TestTB49_FetcherNeverRequestsLeafForUnregisteredRuntime(t *testing.T) {
	mc, requests := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	tb49TwoLeafHead(d, "server-a")
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "container"})

	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	reqs := requests()
	if reqs["leaf-native"] != 0 {
		t.Errorf("native leaf requested %d times from a host with no native runtime, want 0 — the head refuses each one and logs a capability WARN (TB-49)", reqs["leaf-native"])
	}
	if reqs["leaf-container"] == 0 {
		t.Error("container leaf never requested — the skip must be per leaf, not per host")
	}
}

// TestTB49_FetcherNeverRequestsLeafForUntrustedRuntime: the machine HAS a native
// runtime, but this head was attached without NATIVE trust (the TB-7 shape: WASM
// only), so the volunteer advertised no NATIVE to it and the head refuses the native
// leaf. A wasm-capable leaf on the same head is still requested — WASM is always
// trusted.
func TestTB49_FetcherNeverRequestsLeafForUntrustedRuntime(t *testing.T) {
	mc, requests := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	servers[0].Config.TrustedRuntimes = []string{} // WASM only: opts out of the harness's CONTAINER+NATIVE backfill
	d := newFetcherTestDaemon(servers)
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-native", Slug: "leaf-native", Name: "Native", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/native"}}},
			{ID: "leaf-wasm", Slug: "leaf-wasm", Name: "Wasm", State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"wasm": "https://example.org/leaf.wasm"}}},
		},
		DefaultWeights: map[string]int{"leaf-native": 100, "leaf-wasm": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-native": 100, "leaf-wasm": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "native"})
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "wasm"})

	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	reqs := requests()
	if reqs["leaf-native"] != 0 {
		t.Errorf("native leaf requested %d times from a head the volunteer has not trusted for NATIVE, want 0 (TB-49)", reqs["leaf-native"])
	}
	if reqs["leaf-wasm"] == 0 {
		t.Error("wasm leaf never requested — WASM is always trusted and must keep flowing")
	}
}

// TestTB49_NoWorkWarnNamesTheTrustGate: when every attached leaf is skipped because
// the volunteer has not trusted its head for the runtime, the one-time no-work
// diagnostic must say so (with the opt-in command), not shrug "the head has no
// matching units" — pre-fix the leaf was requested and refused, and the generic
// message fired.
func TestTB49_NoWorkWarnNamesTheTrustGate(t *testing.T) {
	mc, _ := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	servers[0].Config.TrustedRuntimes = []string{}
	d := newFetcherTestDaemon(servers)
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{{ID: "leaf-native", Slug: "leaf-native", Name: "Native", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/native"}}}},
		DefaultWeights: map[string]int{"leaf-native": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-native": 100})
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "native"})

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := NewFetcher(d, NewPreFetchQueue(64, d.logger), d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	s := buf.String()
	if n := strings.Count(s, "connected but getting no work"); n != 1 {
		t.Fatalf("no-work WARN count = %d, want exactly 1; log:\n%s", n, s)
	}
	if !strings.Contains(s, "has not trusted") || !strings.Contains(s, "heads trust") {
		t.Errorf("no-work WARN does not name the trust gate and its remedy; log:\n%s", s)
	}
}
