package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"gopkg.in/yaml.v3"
)

// Regression tests for the volunteer's per-leaf CPU override: the cores each
// of a leaf's tasks is given, and how many of its tasks run at once. The
// overrides are written the way a volunteer writes them — the config file's
// leaf_preferences YAML — and read through entry points that existed before
// the override did (bufferAccepts, requestBatchSize, leafClassBufferFull,
// canAccommodateWU, fillSlots, the fetcher's Run), so these tests compile
// against the earlier client and fail there on what it does.

// leafPrefsYAML reads a head's leaf_preferences block as the config loader
// would.
func leafPrefsYAML(t *testing.T, text string) config.LeafPreferences {
	t.Helper()
	var lp config.LeafPreferences
	if err := yaml.Unmarshal([]byte(text), &lp); err != nil {
		t.Fatalf("leaf_preferences YAML: %v", err)
	}
	return lp
}

// overrideHost is an eight-core fetch-test machine attached to one head,
// "head-1", serving a GREP leaf and a Beyblade leaf of one core each, with
// work_buffer_hours 2 and a benchmark of 1 so a unit's FP-ops estimate is its
// seconds. prefs is the head's leaf_preferences YAML.
func overrideHost(t *testing.T, mc *mockClient, prefs string) (*Daemon, CachedLeafInfo) {
	t.Helper()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "head-1", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = 2
	d.cfg.ResourceLimits.MaxCPUCores = 8
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.cfg.Servers = []config.ServerConfig{{Name: "head-1", GRPCAddress: "head-1:443", LeafPreferences: leafPrefsYAML(t, prefs)}}
	d.benchmarkFPOPS = 1.0
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	grep := CachedLeafInfo{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE", EstimatedDurationSeconds: 600,
		ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}}
	bb := CachedLeafInfo{ID: "leaf-bb", Slug: "beyblade", Name: "Beyblade", State: "ACTIVE", EstimatedDurationSeconds: 600,
		ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}}
	d.leafCache.PopulateForTest("head-1", &CachedHeadInfo{Name: "head-1", Leafs: []CachedLeafInfo{grep, bb},
		DefaultWeights: map[string]int{"grep": 100, "beyblade": 100}})
	d.weightedSelector.SetLeafWeights("head-1", map[string]int{"grep": 100, "beyblade": 100})
	return d, grep
}

// cappedGrepUnit is a 10-minute GREP unit.
func cappedGrepUnit(i int) *runtime.WorkUnit {
	return &runtime.WorkUnit{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), LeafID: "leaf-grep", RscFpopsEst: 600, SourceHead: "head-1"}
}

// TestLeafOverride_CappedLeafIsBufferedOnlyForWhatItRuns: a leaf the
// volunteer caps at one running task, with 10-minute units and a 2-hour
// buffer, is asked for at most 2 h × 1 task ÷ 10 min = 12 units, holds no
// more, and is not asked again while those are held — even though the
// eight-core machine's global buffer (2 h × 8 tasks) is nowhere near full. A
// running-task cap the fetcher does not plan for is how a volunteer client
// comes to request far more work than it can run before the deadlines (the
// incumbent platform's client once asked for up to its thousand-job limit
// this way). Pre-fix the cap did not exist and the leaf was buffered for all
// eight slots: the ask was 64 (the per-request ceiling) and 96 units were
// accepted.
func TestLeafOverride_CappedLeafIsBufferedOnlyForWhatItRuns(t *testing.T) {
	d, grep := overrideHost(t, &mockClient{}, "max_running:\n  grep: 1\n")

	if got := d.requestBatchSize(grep, 600); got != 12 {
		t.Errorf("first ask for GREP = %d units, want 12 (2 h for the one GREP task that runs at a time, 10 min each)", got)
	}

	accepted, reason := 0, ""
	for i := 1; i <= 100; i++ {
		wu := cappedGrepUnit(i)
		ok, why := d.bufferAccepts(wu)
		if !ok {
			reason = why
			break
		}
		accepted++
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: wu, FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if accepted != 12 {
		t.Errorf("GREP units accepted = %d, want 12: the machine runs one at a time, so a 2 h buffer holds 12 of them", accepted)
	}
	if !strings.Contains(reason, "leaf") {
		t.Errorf("refusal reason %q does not say the leaf's own buffer is full", reason)
	}
	if full, _ := d.leafClassBufferFull(grep); !full {
		t.Error("with 12 GREP units held the leaf is not reported full, so the fetcher would ask for it again")
	}

	bb := CachedLeafInfo{ID: "leaf-bb", Slug: "beyblade"}
	if full, why := d.leafClassBufferFull(bb); full {
		t.Errorf("the uncapped Beyblade leaf is reported full (%s): the cap on GREP must not hold other leafs back", why)
	}
}

// TestLeafOverride_FetcherAsksNothingForAFullCappedLeaf runs the fetcher:
// with the capped leaf's 12 units held and the Beyblade leaf disabled, a fetch
// loop asks the head for nothing and raises no "connected but getting no
// work" warning — a leaf full for what it can run is not a head without work.
// Pre-fix it asked every round (the global buffer is at 2 h of 16) and, the
// head answering empty, the warning fired.
func TestLeafOverride_FetcherAsksNothingForAFullCappedLeaf(t *testing.T) {
	mc := &mockClient{}
	mc.requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	d, _ := overrideHost(t, mc, "mode: SPECIFIC\nenabled: [grep]\nmax_running:\n  grep: 1\n")
	d.weightedSelector.SetLeafWeights("head-1", map[string]int{"grep": 100})
	for i := 1; i <= 12; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: cappedGrepUnit(i), FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
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
		t.Errorf("RequestWorkUnit calls = %d, want 0: GREP runs one at a time and 2 h of it are held", calls)
	}
	if n := strings.Count(buf.String(), "connected but getting no work"); n != 0 {
		t.Errorf("no-work warning fired %d time(s) for a leaf that is full for what it can run", n)
	}
}

// grantHost is a four-core machine with the slot filler and a real container
// runtime on a recording engine, attached to "head-1" with a GREP leaf that
// declares 2–4 cores. prefs is the head's leaf_preferences YAML.
func grantHost(t *testing.T, prefs string) (*Daemon, *grantRecordingEngine, *runtime.ContainerRuntime) {
	t.Helper()
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{},
		resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger()))
	d.cfg.ResourceLimits.MaxCPUCores = 4
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	head := d.multiClient.Servers()[0].Name
	d.cfg.Servers = []config.ServerConfig{{Name: head, LeafPreferences: leafPrefsYAML(t, prefs)}}
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	d.leafCache.PopulateForTest(head, &CachedHeadInfo{Name: head, Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
		{ID: "leaf-bb", Slug: "beyblade", ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}},
	}})
	engine := newGrantRecordingEngine()
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), engine)
	d.runtimeRegistry.Register(cr)
	d.wireRuntimeLimits(nil, &testLimiter{})
	return d, engine, cr
}

// pushContainerUnit buffers a container unit with a prepared work dir.
func pushContainerUnit(t *testing.T, d *Daemon, cr *runtime.ContainerRuntime, id, leaf string, client *mockClient) {
	t.Helper()
	workDir := t.TempDir()
	for _, sub := range []string{"input", "output", "checkpoint"} {
		if err := os.MkdirAll(filepath.Join(workDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.prefetchQueue.Push(&PreFetchItem{
		WU: headContainerUnit(id, leaf, "ghcr.io/example/"+leaf+":1", 256), Prep: &runtime.PrepareResult{WorkDir: workDir}, Runtime: cr,
		Conn:   &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: client},
		WUResp: &lettucev1.WorkUnitAssignment{}, FetchedAt: time.Now(),
	})
}

// waitCreated waits until the engine has created n containers.
func waitCreated(t *testing.T, e *grantRecordingEngine, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		e.mu.Lock()
		created := len(e.quotas)
		e.mu.Unlock()
		if created >= n {
			return
		}
	}
	t.Fatalf("the engine created fewer than %d containers", n)
}

// TestLeafOverride_CoresOverrideIsTheGrant: a volunteer who sets GREP (2–4
// cores) to 2 has each GREP task created with 2 cores and told
// LETTUCE_CPU_LIMIT=2, and the task is booked at 2. Pre-fix there was no
// override: beside one waiting Beyblade, GREP was given the 3 cores left once
// the Beyblade's core was set aside.
func TestLeafOverride_CoresOverrideIsTheGrant(t *testing.T) {
	d, engine, cr := grantHost(t, "cores:\n  grep: 2\n")
	pushContainerUnit(t, d, cr, "grep-1", "leaf-grep", &mockClient{})
	pushContainerUnit(t, d, cr, "bb-1", "leaf-bb", &mockClient{})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); close(engine.release) }()
	d.fillSlots(ctx)
	waitCreated(t, engine, 2)

	engine.mu.Lock()
	quota, limit, bbQuota := engine.quotas["grep-1"], engine.limits["grep-1"], engine.quotas["bb-1"]
	engine.mu.Unlock()
	if quota != 200000 || limit != "2" {
		t.Errorf("GREP task created with quota %d, told LETTUCE_CPU_LIMIT=%q; want 200000 and \"2\" (the volunteer's 2 cores)", quota, limit)
	}
	if bbQuota != 100000 {
		t.Errorf("Beyblade quota %d, want 100000", bbQuota)
	}
	if booked := d.bookedCPUCores(&runtime.WorkUnit{ID: "grep-2", LeafID: "leaf-grep", Runtime: "container"}); booked != 2 {
		t.Errorf("a GREP unit is booked at %d cores, want the volunteer's 2", booked)
	}
}

// TestLeafOverride_CoresAreKeptInsideTheLeafsRange: an override is the range
// a task is granted from and booked at — 3 for a 2–4 leaf is 3 — and one
// outside the range the leaf declares is brought inside it — 6 runs at 4, 1
// at 2 — because the leaf's minimum is what its units need and its maximum is
// all they can use.
func TestLeafOverride_CoresAreKeptInsideTheLeafsRange(t *testing.T) {
	for _, tc := range []struct {
		cores string
		want  int
	}{{"3", 3}, {"6", 4}, {"1", 2}} {
		t.Run(tc.cores, func(t *testing.T) {
			d, _, _ := grantHost(t, "cores:\n  grep: "+tc.cores+"\n")
			minCores, maxCores := d.unitCoreRange(&runtime.WorkUnit{ID: "g", LeafID: "leaf-grep", Runtime: "container"})
			if minCores != tc.want || maxCores != tc.want {
				t.Errorf("override %s on a 2–4 leaf: task range %d–%d, want %d–%d", tc.cores, minCores, maxCores, tc.want, tc.want)
			}
		})
	}
}

// TestLeafOverride_MaxRunningCapsTheLeafsTasks: with GREP capped at one
// running task, a second GREP unit is refused admission — the reason names
// the leaf's limit — while a Beyblade unit still starts. Pre-fix both GREP
// units ran.
func TestLeafOverride_MaxRunningCapsTheLeafsTasks(t *testing.T) {
	d, engine, cr := grantHost(t, "max_running:\n  grep: 1\n")
	pushContainerUnit(t, d, cr, "grep-1", "leaf-grep", &mockClient{})
	pushContainerUnit(t, d, cr, "grep-2", "leaf-grep", &mockClient{})
	pushContainerUnit(t, d, cr, "bb-1", "leaf-bb", &mockClient{})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); close(engine.release) }()
	d.fillSlots(ctx)
	waitCreated(t, engine, 2)
	time.Sleep(100 * time.Millisecond)

	running := map[string]bool{}
	for _, wu := range d.slotManager.ActiveWorkUnits() {
		running[wu.ID] = true
	}
	if running["grep-2"] {
		t.Errorf("a second GREP task started with GREP capped at one: running %v", running)
	}
	if !running["grep-1"] || !running["bb-1"] {
		t.Errorf("want grep-1 and bb-1 running, got %v", running)
	}
	ok, why := d.canAccommodateWU(&runtime.WorkUnit{ID: "grep-2", LeafID: "leaf-grep", Runtime: "container"})
	if ok || !strings.Contains(why, "at most 1") {
		t.Errorf("admission of a second GREP unit = %v (%q), want a refusal naming the leaf's limit of 1", ok, why)
	}
}
