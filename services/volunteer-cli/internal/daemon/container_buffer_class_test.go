package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Container work gets its own buffer class where the container engine runs
// inside a VM that admits fewer container units at once than the machine has
// slots. The hours target used to count every slot for every unit, so on a
// two-slot Mac whose VM holds one 768 MB container unit the buffer filled with
// container units that then drained through one slot: the last of them waited
// about twice the buffer hours, up to the 90 %-of-deadline drop, and some
// were returned unrun. These tests are written against entry points that
// existed before the class did (bufferAccepts, requestBatchSize, Run), so
// they compile against the earlier code and fail there.

// containerClassMini is a two-slot Mac: max_memory_mb memMB and max_cpu_cores
// cores configured, a container engine whose VM reports vmMB of memory and
// vmCPUs CPUs, work_buffer_hours 2, and a benchmark of 1 so a unit's FP-ops
// estimate is its seconds.
func containerClassMini(t *testing.T, memMB, cores, vmMB, vmCPUs int) *Daemon {
	t.Helper()
	d, _, _, _ := tb85Mini(t, memMB, cores, vmMB, vmCPUs)
	d.cfg.WorkBufferHours = 2
	d.benchmarkFPOPS = 1
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	return d
}

// miniContainerUnit is a 768 MB container unit of the leaf the mini runs,
// estimated at sec seconds.
func miniContainerUnit(i int, sec float64) *runtime.WorkUnit {
	wu := headContainerUnit(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "leaf-bb-c", "ghcr.io/example/beyblade:1", 768)
	wu.RscFpopsEst = sec
	return wu
}

// fillWithContainerUnits offers 41-minute container units to bufferAccepts
// one at a time, buffering each accepted one, until one is refused, and
// returns how many were accepted and the refusal reason.
func fillWithContainerUnits(t *testing.T, d *Daemon) (int, string) {
	t.Helper()
	for i := 1; i <= 20; i++ {
		wu := miniContainerUnit(i, 2460)
		ok, reason := d.bufferAccepts(wu)
		if !ok {
			return i - 1, reason
		}
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: wu, FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	t.Fatal("twenty 41-minute container units accepted against a 4 h target — no bound held")
	return 0, ""
}

// TestContainerBufferClass_AcceptsOnlyWhatTheVMCanDrain: the VM runs one
// 768 MB unit at a time (768 MB budget, one vCPU), so container work gets a
// target of 2 h, not the 4 h two slots would give. Three 41-minute units fill
// it; the fourth is refused, and the reason names the container buffer.
// Before the class existed six were accepted, and drained one at a time the
// sixth started about 205 minutes after it arrived. A native unit is still
// accepted afterwards: it runs in the other slot and the global target governs
// it.
func TestContainerBufferClass_AcceptsOnlyWhatTheVMCanDrain(t *testing.T) {
	d := containerClassMini(t, 1024, 2, 1280, 1)
	if got := d.bufferTargetSeconds(); got != 14400 {
		t.Fatalf("global target = %g, want 14400 (2 h × 2 slots) — harness drift", got)
	}

	accepted, reason := fillWithContainerUnits(t, d)
	if accepted != 3 {
		t.Errorf("container units accepted = %d, want 3 (a 2 h target for the one unit the VM runs at a time, 41 min each)", accepted)
	}
	if !strings.Contains(reason, "container") {
		t.Errorf("refusal reason %q does not name the container buffer", reason)
	}

	native := headNativeUnit("00000000-0000-4000-8000-000000000099", "leaf-bb-n", 128)
	native.RscFpopsEst = 2460
	if ok, why := d.bufferAccepts(native); !ok {
		t.Errorf("native unit refused (%s) with %d s held against the 14,400 s global target — the container bound must not tighten other work", why, int(d.bufferedSeconds()))
	}
}

// TestContainerBufferClass_UnchangedWhereTheEngineDoesNotBindIt: with no VM
// clip (Linux, where the engine shares the host) or a VM that runs a unit per
// slot, container work is buffered against the global target exactly as
// before: six 41-minute units against 4 h.
func TestContainerBufferClass_UnchangedWhereTheEngineDoesNotBindIt(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		memMB, cores, vmMB, n int
	}{
		{"no VM", 1024, 2, 0, 0},
		{"VM runs two at a time", 4096, 4, 2048 + runtime.ContainerVMHeadroomMB, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := containerClassMini(t, tc.memMB, tc.cores, tc.vmMB, tc.n)
			if accepted, _ := fillWithContainerUnits(t, d); accepted != 6 {
				t.Errorf("container units accepted = %d, want 6 (the global 4 h target alone)", accepted)
			}
		})
	}
}

// TestContainerBufferClass_AskSizedByWhatTheVMCanDrain: with an empty buffer
// the ask for the container leaf divides the container target, 2 h ÷ 41 min
// = 2, where it used to divide the global 4 h (5, of which the buffer could
// run one at a time). The native leaf's ask is unchanged.
func TestContainerBufferClass_AskSizedByWhatTheVMCanDrain(t *testing.T) {
	d := containerClassMini(t, 1024, 2, 1280, 1)
	leafs := tb85Leafs(1)
	container, native := leafs[0], leafs[1]
	container.EstimatedDurationSeconds = 2460
	native.EstimatedDurationSeconds = 1620

	if got := d.requestBatchSize(container, d.leafEstSeconds(container)); got != 2 {
		t.Errorf("container leaf ask = %d, want 2 (2 h container target ÷ 41 min)", got)
	}
	if got := d.requestBatchSize(native, d.leafEstSeconds(native)); got != 8 {
		t.Errorf("native leaf ask = %d, want 8 (4 h global target ÷ 27 min)", got)
	}

	// No estimate at all: the unit-count fallback, two per unit the VM runs
	// at a time, rather than the four the slots would allow.
	container.EstimatedDurationSeconds = 0
	if got := d.requestBatchSize(container, 0); got != 2 {
		t.Errorf("container leaf ask with no estimate = %d, want 2 (two per container unit the VM runs at a time)", got)
	}
}

// TestContainerBufferClass_FetcherStopsAskingWhenTheClassIsFull: with three
// 41-minute container units held, the container class is at its target while
// the global buffer is not (7,380 s of 14,400), so the fetcher used to ask the
// head for the container leaf every round; each answer was either refused on
// arrival or queued to the deadline drop. The leaf is now skipped before the
// request: no RPC, and no "connected but getting no work" notice either.
func TestContainerBufferClass_FetcherStopsAskingWhenTheClassIsFull(t *testing.T) {
	d := containerClassMini(t, 1024, 2, 1280, 1)
	for i := 1; i <= 3; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: miniContainerUnit(i, 2460), FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if d.workBufferFull() {
		t.Fatal("global buffer reports full at 7,380 s of 14,400 — state does not reproduce the defect")
	}
	head := d.multiClient.Servers()[0]
	rc, ok := head.Client.(*reRegMockClient)
	if !ok {
		t.Fatalf("head client is %T, want the harness's *reRegMockClient", head.Client)
	}

	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	// The disk gate is not under test, and it would ask the harness's
	// client-less container runtime whether the leaf's image is cached.
	f.shouldFetchFunc = nil
	f.leafDiskGateFn = nil
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	if calls := rc.mockClient.getRequestCalls(); calls != 0 {
		t.Errorf("RequestWorkUnit calls = %d, want 0: the container engine's VM already has 2 h of container work to drain one unit at a time", calls)
	}
	if n, _ := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("no_work notice raised %d time(s) for a full container buffer", n)
	}
	if got := d.prefetchQueue.Len(); got != 3 {
		t.Errorf("buffer holds %d units, want the 3 preloaded", got)
	}
}
