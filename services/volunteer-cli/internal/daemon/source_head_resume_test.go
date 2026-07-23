package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// PB-31 regression coverage: WorkUnit.SourceHead scopes the artifact netguard
// opt-in (runtime/artifact_exemption.go) per head, but it is not part of the
// persisted task state. A unit resumed from a previous session must get it
// re-stamped from the server connection it is re-attached to — before the fix
// the resume paths left it empty, so a resumed unit under an explicitly
// opted-in head silently lost its opt-in (failed closed) while runtime.go's
// doc comment claimed the field was persisted.

// TestResumePrefetchBuffer_RestampsSourceHead: a buffered (not-yet-started)
// unit restored from prefetch-buffer.json carries the head's display name.
func TestResumePrefetchBuffer_RestampsSourceHead(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.cfg.DataDir = t.TempDir()
	d.prefetchQueue = NewPreFetchQueue(8, d.logger)

	srv := d.multiClient.Servers()[0]
	pt := PersistedTask{
		WorkUnitID:        "9c1e2d3f-4a5b-4c6d-8e7f-102938475601",
		LeafID:            "leaf-1",
		ServerGRPCAddress: srv.Config.GRPCAddress,
		ServerName:        srv.Name,
		VolunteerID:       srv.VolunteerID,
		RuntimeName:       "native",
		WorkDir:           t.TempDir(),
		FetchedAt:         time.Now(),
	}
	if err := SaveBufferState(d.cfg.DataDir, []PersistedTask{pt}); err != nil {
		t.Fatalf("SaveBufferState: %v", err)
	}

	d.resumePrefetchBuffer(context.Background())

	item := d.prefetchQueue.Pop()
	if item == nil {
		t.Fatal("restored buffered task not found in the prefetch queue")
	}
	if item.WU.SourceHead != srv.Name {
		t.Errorf("restored buffered unit SourceHead = %q, want the head's display name %q (PB-31)",
			item.WU.SourceHead, srv.Name)
	}
}

// TestResumePersistedTasks_RestampsSourceHead: an active task resumed via the
// re-execution path (work dir preserved, no live orphan PID) carries the head's
// display name when it reaches the runtime.
func TestResumePersistedTasks_RestampsSourceHead(t *testing.T) {
	gotHead := make(chan string, 1)
	rt := &mockRuntime{
		canHandle: true,
		name:      "native",
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			gotHead <- wu.SourceHead
			return &runtime.ExecutionResult{
				OutputData:     []byte("ok"),
				OutputChecksum: "c",
				ExitCode:       0,
			}, nil
		},
	}
	mc := &mockClient{} // StartWork defaults to {Ok: true}
	d := newTestDaemon(mc, rt)
	d.cfg.DataDir = t.TempDir()
	d.slotManager = NewSlotManager(1, d.logger)
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(rt)

	srv := d.multiClient.Servers()[0]
	pt := PersistedTask{
		WorkUnitID:        "7b8c9dae-1f20-4132-8455-66778899aabb",
		LeafID:            "leaf-1",
		ServerGRPCAddress: srv.Config.GRPCAddress,
		ServerName:        srv.Name,
		VolunteerID:       srv.VolunteerID,
		RuntimeName:       "native",
		WorkDir:           t.TempDir(),
		PID:               0, // re-exec branch, not orphan reattach
		StartedAt:         time.Now().Add(-time.Minute),
	}
	if err := SaveActiveState(d.cfg.DataDir, []PersistedTask{pt}); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.resumePersistedTasks(ctx)

	select {
	case head := <-gotHead:
		if head != srv.Name {
			t.Errorf("resumed re-exec unit SourceHead = %q, want the head's display name %q (PB-31)", head, srv.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed task never reached Execute")
	}
	d.slotManager.StopAll()
}
