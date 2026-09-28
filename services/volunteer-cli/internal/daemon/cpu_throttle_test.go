package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// throttleTestEngine reports, for each container, the throttle counts the
// test sets next, the way Docker's stats report them.
type throttleTestEngine struct {
	runtime.DockerClient // nil: a call not implemented below panics
	mu                   sync.Mutex
	counts               map[string]runtime.CPUThrottle
}

func (e *throttleTestEngine) set(id string, periods, throttled uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counts[id] = runtime.CPUThrottle{Periods: periods, Throttled: throttled}
}

func (e *throttleTestEngine) ContainerUsage(_ context.Context, id string) (*runtime.ContainerStats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.counts[id]
	if !ok {
		return nil, fmt.Errorf("no such container %s", id)
	}
	return &runtime.ContainerStats{ThrottlePeriods: c.Periods, ThrottledPeriods: c.Throttled}, nil
}

// TestTaskStoppedByItsGrantIsFlagged: a GREP task granted 2 of the 4
// cores its leaf can use, whose quota stops it in 80 % of its periods over a
// minute, raises one notice naming the leaf, its grant, the leaf's range and
// why it got fewer; a Beyblade task stopped in 2 % of its periods raises
// nothing; the first reading of a task, and a minute in which it barely ran,
// judge nothing; and the notice is resolved once the GREP task stops being
// stopped.
func TestTaskStoppedByItsGrantIsFlagged(t *testing.T) {
	d := grantTestDaemon(t, 4)
	d.notices = NewNoticeLog()
	engine := &throttleTestEngine{counts: map[string]runtime.CPUThrottle{}}

	grep := grantTestGrep("grep-1")
	grep.CPUGrant = runtime.CPUGrant{Cores: 2, BudgetCores: 4}
	occupy(d, 0, grep, NewContainerProcessHandle(engine, "c-grep"))
	bb := grantTestBB("bb-1")
	bb.CPUGrant = runtime.CPUGrant{Cores: 1, BudgetCores: 4}
	occupy(d, 1, bb, NewContainerProcessHandle(engine, "c-bb"))
	for _, i := range []int{0, 1} {
		d.slotManager.slots[i].conn = &ServerConnection{Name: "scios"}
	}
	ctx := context.Background()
	live := func() []Notice {
		var out []Notice
		notices, _ := d.notices.Since(0)
		for _, n := range notices {
			if n.Code == cpuThrottleNoticeCode && n.ResolvedAt == nil {
				out = append(out, n)
			}
		}
		return out
	}

	engine.set("c-grep", 1000, 800)
	engine.set("c-bb", 1000, 20)
	d.checkCPUThrottling(ctx)
	if n := live(); len(n) != 0 {
		t.Fatalf("a first reading raised %d notice(s); it has nothing to compare with", len(n))
	}

	engine.set("c-grep", 1600, 1280) // 480 of 600 periods stopped: 80 %
	engine.set("c-bb", 1600, 32)     // 12 of 600: 2 %
	d.checkCPUThrottling(ctx)
	n := live()
	if len(n) != 1 {
		t.Fatalf("live throttle notices = %d, want exactly one (GREP's)", len(n))
	}
	if n[0].Leaf != "leaf-grep" || n[0].Head != "scios" {
		t.Errorf("notice names head %q, leaf %q; want scios / leaf-grep", n[0].Head, n[0].Leaf)
	}
	for _, want := range []string{"80%", "the 2 cores it was given", "2–4 cores", "up to 4", "other tasks needed the rest"} {
		if !strings.Contains(n[0].Message, want) {
			t.Errorf("notice lacks %q: %s", want, n[0].Message)
		}
	}

	engine.set("c-grep", 1650, 1320) // 50 periods: it barely ran
	engine.set("c-bb", 1650, 33)
	d.checkCPUThrottling(ctx)
	if n := live(); len(n) != 1 {
		t.Errorf("a minute with too few periods to judge changed the notice: %d live", len(n))
	}

	engine.set("c-grep", 2250, 1340) // 20 of 600: it now fits its grant
	engine.set("c-bb", 2250, 40)
	d.checkCPUThrottling(ctx)
	if n := live(); len(n) != 0 {
		t.Errorf("the GREP task is no longer stopped but %d throttle notice(s) stay live", len(n))
	}
}

// TestTaskAtItsLeafsMaximumIsTheLeafsToFix: a task stopped although it
// was given all its leaf declares it can use points at the leaf, not the
// volunteer's limit.
func TestTaskAtItsLeafsMaximumIsTheLeafsToFix(t *testing.T) {
	d := grantTestDaemon(t, 8)
	d.notices = NewNoticeLog()
	engine := &throttleTestEngine{counts: map[string]runtime.CPUThrottle{}}
	grep := grantTestGrep("grep-1")
	grep.CPUGrant = runtime.CPUGrant{Cores: 4, BudgetCores: 8}
	occupy(d, 0, grep, NewContainerProcessHandle(engine, "c-grep"))
	d.slotManager.slots[0].conn = &ServerConnection{Name: "scios"}

	engine.set("c-grep", 1000, 900)
	d.checkCPUThrottling(context.Background())
	engine.set("c-grep", 2000, 1800)
	d.checkCPUThrottling(context.Background())
	notices, _ := d.notices.Since(0)
	var msg string
	for _, n := range notices {
		if n.Code == cpuThrottleNoticeCode {
			msg = n.Message
		}
	}
	for _, want := range []string{"the 4 cores it was given", "all its leaf declares it can use (max_cpu_cores 4)", "LETTUCE_CPU_LIMIT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice lacks %q: %s", want, msg)
		}
	}
}
