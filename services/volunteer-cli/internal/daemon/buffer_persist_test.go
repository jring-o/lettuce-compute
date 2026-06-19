package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestBufferState_RoundTrip verifies the prefetch-buffer persistence round-trips its
// buffered-task fields and uses a file separate from the active-tasks state.
func TestBufferState_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{{
		WorkUnitID:        "buf-1",
		LeafID:            "leaf-1",
		ServerGRPCAddress: "localhost:50051",
		ServerName:        "s",
		VolunteerID:       "v",
		RuntimeName:       "container",
		WorkDir:           "/tmp/w",
		DeadlineSeconds:   18000,
		ReservedUntilUnix: 1234567890,
		FetchedAt:         time.Now().UTC().Truncate(time.Second),
	}}

	if err := SaveBufferState(dir, tasks); err != nil {
		t.Fatalf("SaveBufferState: %v", err)
	}
	state, err := LoadBufferState(dir)
	if err != nil {
		t.Fatalf("LoadBufferState: %v", err)
	}
	if state == nil || len(state.Tasks) != 1 {
		t.Fatalf("expected 1 buffered task, got %+v", state)
	}
	got := state.Tasks[0]
	if got.WorkUnitID != "buf-1" || got.ReservedUntilUnix != 1234567890 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// The buffer state is written to its own file and must not touch active-tasks.json.
	if _, err := os.Stat(filepath.Join(dir, "prefetch-buffer.json")); err != nil {
		t.Errorf("prefetch-buffer.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "active-tasks.json")); !os.IsNotExist(err) {
		t.Errorf("buffer save must not write active-tasks.json")
	}

	ClearBufferState(dir)
	if s, _ := LoadBufferState(dir); s != nil {
		t.Errorf("ClearBufferState should remove the file")
	}
}

// TestHeldWorkUnitIDs_UnionOfBufferAndSlots verifies the reported held set is the
// union of the prefetch buffer and the active slots.
func TestHeldWorkUnitIDs_UnionOfBufferAndSlots(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		logger:        logger,
		prefetchQueue: NewPreFetchQueue(10, logger),
		slotManager:   NewSlotManager(2, logger),
	}

	if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{ID: "buffered-1"}}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{ID: "buffered-2"}}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Mark a slot active by hand (avoids launching the real slot goroutine/StartWork).
	slot := d.slotManager.slots[0]
	slot.mu.Lock()
	slot.active = true
	slot.wu = &runtime.WorkUnit{ID: "running-1"}
	slot.mu.Unlock()

	ids := d.heldWorkUnitIDs()
	want := map[string]bool{"buffered-1": true, "buffered-2": true, "running-1": true}
	if len(ids) != len(want) {
		t.Fatalf("expected %d held ids, got %d: %v", len(want), len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected held id %q", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("missing held ids: %v", want)
	}
}
