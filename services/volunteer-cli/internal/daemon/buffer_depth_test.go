package daemon

import (
	"context"
	"testing"
)

// The work buffer's queue depth scales with the machine. On a large machine it holds the
// whole hours target, so it is not the limit that binds there; where it does bind (units so
// short that two hours of them outnumber sixteen per slot) the ask is the room left, never
// the hours deficit.

func TestWorkBufferQueueDepth_ScalesWithTheSlots(t *testing.T) {
	for _, c := range []struct{ slots, want int }{
		{1, 256}, {8, 256}, {16, 256}, {17, 272}, {64, 1024}, {256, 4096},
	} {
		if got := workBufferQueueDepthFor(c.slots); got != c.want {
			t.Errorf("depth for %d slots = %d, want %d", c.slots, got, c.want)
		}
	}
}

// askAt fills a fresh 256-slot host's queue (built at the production depth) with fill units
// of unitSeconds each and returns what one fetch round asked for and the room it had.
func askAt(t *testing.T, unitSeconds float64, fill int) (asked int32, room int, d *Daemon) {
	t.Helper()
	head := newRoomTestHead()
	d, f, _ := roomTestHost(t, head, 256, workBufferQueueDepthFor(256), unitSeconds)
	fillQueue(t, d.prefetchQueue, fill)
	room = d.prefetchQueue.Room()
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	asks := head.recordedAsks()
	if len(asks) != 1 {
		t.Fatalf("fill %d: sent %d requests, want 1", fill, len(asks))
	}
	return asks[0].MaxAssignments, room, d
}

// The machine the fetch loop was found on: 256 slots, a two-hour buffer, 13-minute units.
// The hours target is about 2,360 units, and the queue holds 4,096, so the queue never
// refuses a unit the hours asked for; at every fill the ask is the hours ask and within
// the room left.
func TestWorkBufferQueueDepth_HoldsTheHoursTargetOnA256SlotMachine(t *testing.T) {
	depth := workBufferQueueDepthFor(256)
	_, _, d := askAt(t, 780, 0)
	target := int(d.bufferTargetSeconds() / 780)
	if target < 2000 || target > depth {
		t.Fatalf("hours target = %d units against a depth of %d; want the target (about 2,360) within the depth", target, depth)
	}
	for _, fill := range []int{0, 1000, 2000, target - 10} {
		asked, room, _ := askAt(t, 780, fill)
		if int(asked) > room {
			t.Errorf("fill %d: asked for %d with room for %d", fill, asked, room)
		}
		wantHours := int32(target - fill)
		if wantHours > maxBatchPerRequest {
			wantHours = maxBatchPerRequest
		}
		if asked != wantHours {
			t.Errorf("fill %d: asked for %d, want the hours ask %d: the depth must not be the limit here", fill, asked, wantHours)
		}
	}
}

// One-minute units: two hours of them are 30,720 units on 256 slots, far past the depth.
// There the depth binds, and ten short of it the ask is ten.
func TestWorkBufferQueueDepth_BindsOnlyOnShortUnitsAndTheAskIsTheRoom(t *testing.T) {
	depth := workBufferQueueDepthFor(256)
	asked, room, d := askAt(t, 60, depth-10)
	if d.workBufferHoursFull() {
		t.Fatal("the hours target is reached: harness drift (the depth must bind first here)")
	}
	if room != 10 || asked != 10 {
		t.Errorf("asked for %d with room for %d; want 10 and 10", asked, room)
	}
}
