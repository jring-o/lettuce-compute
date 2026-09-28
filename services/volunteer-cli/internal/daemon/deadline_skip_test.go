package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Tests for the deadline skip's arithmetic and notice, the CPU setup notices
// and the preview of what would run together.

// deadlineHost is a machine attached to "head-1" serving GREP, which declares
// 2–4 cores and whose last four runs here each took 5 hours on 2 cores
// against a 6-hour deadline, with a CPU limit of limit cores.
func deadlineHost(t *testing.T, limit int) *Daemon {
	t.Helper()
	servers := []*ServerConnection{{Client: &mockClient{}, VolunteerID: "vol-1", Name: "head-1", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.cfg.ResourceLimits.MaxCPUCores = limit
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.cfg.Servers = []config.ServerConfig{{Name: "head-1", GRPCAddress: "head-1:443"}}
	d.notices = NewNoticeLog()
	d.durations = LoadDurationTracker(t.TempDir())
	for i := 0; i < 4; i++ {
		d.durations.RecordRun("leaf-grep", 0, 5*3600, 2, 6*3600)
	}
	d.leafCache.PopulateForTest("head-1", &CachedHeadInfo{Name: "head-1", Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
	}})
	return d
}

// TestDeadline_CoresRaiseBringsTheLeafBack is the acceptance case: GREP's
// median here is 5 h on 2 cores against a 6 h deadline and the 1.25 margin.
// With a CPU limit of 2 it is not fetched and a notice names the leaf, the
// run time, the deadline and the remedy (GREP can use 4 cores, the limit
// gives it 2). Raising the limit to 4 brings the estimate to 2.5 h — under
// the bound — and it is fetched again, the notice resolved.
func TestDeadline_CoresRaiseBringsTheLeafBack(t *testing.T) {
	d := deadlineHost(t, 2)
	grep, _ := d.cachedLeaf("leaf-grep")
	if ok, why := d.leafDeadlineGate(grep); ok || !strings.Contains(why, "6 h 15 min") {
		t.Fatalf("GREP at 2 cores: gate ok=%v (%q); want it skipped, needing 6 h 15 min", ok, why)
	}
	d.refreshCPUSetupNotices()
	n := liveNotice(d, noticeDeadlineSkip)
	if n == nil {
		t.Fatal("no notice for a leaf this machine cannot finish in time")
	}
	for _, want := range []string{"GREP's tasks on head-1 must finish within 6 hours", "about 5 hours at 2 cores", "the middle of its last 4 runs", "raise your CPU limit", "up to 4 cores", "disable GREP"} {
		if !strings.Contains(n.Message, want) {
			t.Errorf("notice lacks %q:\n%s", want, n.Message)
		}
	}
	if n.Level != NoticeWarn || n.Leaf != "leaf-grep" || n.Head != "head-1" {
		t.Errorf("notice level/leaf/head = %s/%s/%s", n.Level, n.Leaf, n.Head)
	}
	// Checked again with nothing changed: the same notice, not a new count.
	d.refreshCPUSetupNotices()
	if n2 := liveNotice(d, noticeDeadlineSkip); n2 == nil || n2.Count != 1 {
		t.Errorf("re-evaluating an unchanged condition changed the notice: %+v", n2)
	}

	d.cfg.ResourceLimits.MaxCPUCores = 4
	if ok, why := d.leafDeadlineGate(grep); !ok {
		t.Errorf("GREP at 4 cores (2.5 h, 3 h 8 min with the margin) is still skipped: %s", why)
	}
	d.refreshCPUSetupNotices()
	if n := liveNotice(d, noticeDeadlineSkip); n != nil {
		t.Errorf("the notice outlived the condition: %s", n.Message)
	}
}

// TestDeadline_CoresSettingIsTheRemedyWhenItHoldsTheLeafBack: with a limit of
// 8 but the volunteer's own cores setting of 2 for GREP, the notice points at
// that setting, and clearing it brings GREP back.
func TestDeadline_CoresSettingIsTheRemedyWhenItHoldsTheLeafBack(t *testing.T) {
	d := deadlineHost(t, 8)
	d.cfg.Servers[0].LeafPreferences = config.LeafPreferences{Cores: map[string]int{"grep": 2}}
	d.refreshCPUSetupNotices()
	n := liveNotice(d, noticeDeadlineSkip)
	if n == nil || !strings.Contains(n.Message, "you set 2") || !strings.Contains(n.Message, "lettuce-volunteer leafs cores grep 4") {
		t.Fatalf("notice should point at the leaf's cores setting: %+v", n)
	}
	d.cfg.Servers[0].LeafPreferences = config.LeafPreferences{}
	grep, _ := d.cachedLeaf("leaf-grep")
	if ok, why := d.leafDeadlineGate(grep); !ok {
		t.Errorf("with the setting cleared GREP gets 4 cores and fits; still skipped: %s", why)
	}
}

// TestDeadline_ScheduleAndCPUTimeCount: 4 hours at the best cores fits a
// 6-hour deadline (5 h with the margin) — until the schedule leaves only part
// of the deadline to run in, or the CPU time limit stretches the run.
func TestDeadline_ScheduleAndCPUTimeCount(t *testing.T) {
	d := deadlineHost(t, 2)
	d.durations = LoadDurationTracker(t.TempDir())
	d.durations.RecordRun("leaf-grep", 0, 4*3600, 2, 6*3600)
	grep, _ := d.cachedLeaf("leaf-grep")
	if c := d.checkDeadline(grep, 6*3600); c.Infeasible {
		t.Fatalf("4 h (+25 %% = 5 h) against 6 h judged infeasible: %+v", c)
	}

	d.cfg.ResourceLimits.MaxCPUTimePct = 80 // 5 h ÷ 0.8 = 6 h 15 min
	if c := d.checkDeadline(grep, 6*3600); !c.Infeasible {
		t.Errorf("at 80 %% CPU time a 5 h need becomes 6 h 15 min; judged feasible: %+v", c)
	}
	d.refreshCPUSetupNotices()
	if n := liveNotice(d, noticeDeadlineSkip); n == nil || !strings.Contains(n.Message, "80 % CPU time limit") || !strings.Contains(n.Message, "raise the CPU time limit") {
		t.Errorf("notice should name the CPU time limit: %+v", n)
	}

	d.cfg.ResourceLimits.MaxCPUTimePct = 100
	now := time.Now()
	start, end := (now.Hour()+2)%24, (now.Hour()+6)%24 // open for 4 of the next 6 hours at most
	d.scheduler = resource.NewScheduler(&config.Scheduling{Mode: "SCHEDULED",
		ScheduleRanges: []config.ScheduleRange{{Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHour: start, EndHour: end}}}, quietLogger())
	c := d.checkDeadline(grep, 6*3600)
	if !c.Infeasible || c.OpenSeconds > 4*3600 {
		t.Errorf("with the schedule open at most 4 of the 6 hours, a 5 h need must not fit: %+v", c)
	}
}

// TestDeadline_NoRunsYetFetchesAsBefore: before a leaf's first completion
// here there is no run time to judge by, and it is fetched as before.
func TestDeadline_NoRunsYetFetchesAsBefore(t *testing.T) {
	d := deadlineHost(t, 2)
	d.durations = LoadDurationTracker(t.TempDir())
	grep, _ := d.cachedLeaf("leaf-grep")
	d.noteArrivalDeadline("leaf-grep", 3600)
	if ok, why := d.leafDeadlineGate(grep); !ok {
		t.Errorf("a leaf with no runs here was skipped: %s", why)
	}
}

// TestDeadline_TheLeafsDeadlineIsLearnedFromArrivalsAndRuns: the deadline a
// leaf is judged by before any of its units is held comes from the last one
// to arrive, else from its recorded runs.
func TestDeadline_TheLeafsDeadlineIsLearnedFromArrivalsAndRuns(t *testing.T) {
	d := deadlineHost(t, 2)
	if got := d.leafDeadline("leaf-grep"); got != 6*3600 {
		t.Errorf("deadline from the recorded runs = %d, want 21600", got)
	}
	d.noteArrivalDeadline("leaf-grep", 12*3600)
	if got := d.leafDeadline("leaf-grep"); got != 12*3600 {
		t.Errorf("deadline after a 12 h unit arrived = %d, want 43200", got)
	}
}

// TestCPUSetup_OneTaskAtATime is the field case: 32 CPUs with a CPU limit of
// 1 core runs one task at a time, while the memory limit would run eight
// one-core, 1024 MB tasks. The notice says so with the exact remedy, and
// goes once the limit is raised.
func TestCPUSetup_OneTaskAtATime(t *testing.T) {
	old := hostCPUCount
	hostCPUCount = func() int { return 32 }
	defer func() { hostCPUCount = old }()
	d := deadlineHost(t, 1)
	d.cfg.ResourceLimits.MaxMemoryMB = 8192
	d.leafCache.PopulateForTest("head-1", &CachedHeadInfo{Name: "head-1", Leafs: []CachedLeafInfo{
		{ID: "leaf-bb", Slug: "beyblade", Name: "Beyblade", State: "ACTIVE",
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1},
			ExecutionSpec:        &CachedExecutionSpec{MaxMemoryMB: 1024}},
	}})
	d.refreshCPUSetupNotices()
	n := liveNotice(d, noticeCPUOneTask)
	if n == nil {
		t.Fatal("no notice for a 1-core limit on a 32-CPU machine")
	}
	for _, want := range []string{"Your CPU limit is 1 core in total", "one task at a time", "not given to each", "32 CPUs", "would run 8 such tasks at once", "set the CPU limit to 8", "resource_limits.max_cpu_cores 8"} {
		if !strings.Contains(n.Message, want) {
			t.Errorf("notice lacks %q:\n%s", want, n.Message)
		}
	}
	d.cfg.ResourceLimits.MaxCPUCores = 8
	d.refreshCPUSetupNotices()
	if n := liveNotice(d, noticeCPUOneTask); n != nil {
		t.Errorf("the notice outlived the setting: %s", n.Message)
	}

	// One task at a time chosen explicitly is not questioned.
	d.cfg.ResourceLimits.MaxCPUCores = 1
	d.cfg.MaxRunningTasks = 1
	d.refreshCPUSetupNotices()
	if n := liveNotice(d, noticeCPUOneTask); n != nil {
		t.Errorf("with max_running_tasks 1 the notice still says: %s", n.Message)
	}
}

// TestCPUSetup_LeafThatCanNeverStart: GREP needs 2 cores per task; with a CPU
// limit of 1 heads never send it, and a notice names the leaf and the limit
// that would let it run. Raising the limit to 2 resolves it.
func TestCPUSetup_LeafThatCanNeverStart(t *testing.T) {
	d := deadlineHost(t, 1)
	d.durations = LoadDurationTracker(t.TempDir())
	d.refreshCPUSetupNotices()
	n := liveNotice(d, noticeLeafCannotStart)
	if n == nil || !strings.Contains(n.Message, "GREP needs at least 2 cores") || !strings.Contains(n.Message, "raise the CPU limit to 2") || n.Leaf != "leaf-grep" {
		t.Fatalf("notice for a leaf that can never start: %+v", n)
	}
	d.cfg.ResourceLimits.MaxCPUCores = 2
	d.refreshCPUSetupNotices()
	if n := liveNotice(d, noticeLeafCannotStart); n != nil {
		t.Errorf("the notice outlived the limit: %s", n.Message)
	}
}

// previewHost is a 4-core machine with GREP (2–4 cores) and Beyblade (1 core)
// enabled on "head-1".
func previewHost(t *testing.T) *Daemon {
	t.Helper()
	d := deadlineHost(t, 4)
	d.durations = LoadDurationTracker(t.TempDir())
	d.slotManager = NewSlotManager(8, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.leafCache.PopulateForTest("head-1", &CachedHeadInfo{Name: "head-1", Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 2, MaxCPUCores: 4}},
		{ID: "leaf-bb", Slug: "beyblade", Name: "Beyblade", State: "ACTIVE",
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}},
	}})
	return d
}

// TestRunPreview_GREPAndBeybladeOnFourCores: on its own GREP runs two tasks
// of 2 cores and Beyblade four of 1; together, with the buffer holding both
// in turn, a GREP task of 2 cores and a Beyblade of 1 start, and the next
// GREP task waits for the core left, which is held for it.
func TestRunPreview_GREPAndBeybladeOnFourCores(t *testing.T) {
	d := previewHost(t)
	p := d.RunPreview()
	alone := map[string][]int{}
	for _, a := range p.Alone {
		alone[a.LeafName] = a.Tasks
	}
	if got := alone["GREP"]; len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Errorf("GREP alone = %v, want two tasks of 2 cores", got)
	}
	if got := alone["Beyblade"]; len(got) != 4 {
		t.Errorf("Beyblade alone = %v, want four tasks of 1 core", got)
	}
	together, aloneText := DescribeRunPreview(p)
	if together != "GREP 2 cores, Beyblade 1 core — 3 of 4 cores; the next GREP task, which needs 2 cores, would wait for them" {
		t.Errorf("together = %q", together)
	}
	if aloneText != "GREP 2 at once, 2 cores each; Beyblade 4 at once, 1 core each" {
		t.Errorf("alone = %q", aloneText)
	}
}

// TestRunPreview_IsWhatTheSlotFillerStarts: the preview is the scheduler's own
// arithmetic, not a copy of it. Buffered in the preview's order on an idle
// 4-core machine, the units the real slot filler starts — their leafs and the
// cores they are given — are the preview's "together" list.
func TestRunPreview_IsWhatTheSlotFillerStarts(t *testing.T) {
	d := previewHost(t)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	defer func() { freeSystemMemoryMB = orig }()
	p := d.RunPreview()

	block := make(chan struct{})
	defer close(block)
	for _, wu := range d.previewUnits(d.enabledLeafsByHead()) {
		dir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dir, "output"), 0o755)
		d.prefetchQueue.Push(&PreFetchItem{
			WU: wu, Prep: &runtime.PrepareResult{WorkDir: dir},
			Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, _ *runtime.WorkUnit, _ *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-block:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{}, nil
			}},
			Conn: &ServerConnection{Name: "head-1", VolunteerID: "vol-1", Client: &mockClient{}}, WUResp: &lettucev1.WorkUnitAssignment{}, FetchedAt: time.Now(),
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.fillSlots(ctx)
	time.Sleep(100 * time.Millisecond)

	var started, previewed []string
	for _, wu := range d.slotManager.ActiveWorkUnits() {
		started = append(started, wu.LeafID+":"+strconv.Itoa(wu.CPUGrant.Cores))
	}
	for _, task := range p.Together {
		previewed = append(previewed, task.LeafID+":"+strconv.Itoa(task.Cores))
	}
	sort.Strings(started)
	sort.Strings(previewed)
	if strings.Join(started, ",") != strings.Join(previewed, ",") {
		t.Errorf("the slot filler started %v; the preview said %v", started, previewed)
	}
}
