package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// A unit handed out again with a checkpoint another run saved is restored from
// that checkpoint before it starts. The restore goes through the production
// client against a server with the head's message limits, so gRPC's own
// receive limit is in the path: with its 4 MiB default every checkpoint over
// 4 MiB failed to download, the unit ran from the beginning, and the task list
// still reported it "resumed from checkpoint".

// checkpointHead serves one checkpoint blob (or refuses with err) and accepts
// run-start and checkpoint saves.
type checkpointHead struct {
	lettucev1.UnimplementedVolunteerServiceServer
	blob []byte
	err  error
}

func (h *checkpointHead) GetCheckpoint(context.Context, *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	if h.err != nil {
		return nil, h.err
	}
	return &lettucev1.GetCheckpointResponse{HasCheckpoint: true, CheckpointData: h.blob, CheckpointSequence: 3}, nil
}

func (h *checkpointHead) StartWork(context.Context, *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
	return &lettucev1.StartWorkResponse{Ok: true}, nil
}

func (h *checkpointHead) SaveCheckpoint(context.Context, *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
}

// checkpointHeadConn starts h with the head's 128 MiB limits and returns a
// connection to it through client.New, as the daemon makes one.
func checkpointHeadConn(t *testing.T, h *checkpointHead) *ServerConnection {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const headMaxMsgSize = 128 * 1024 * 1024
	s := grpc.NewServer(grpc.MaxRecvMsgSize(headMaxMsgSize), grpc.MaxSendMsgSize(headMaxMsgSize))
	lettucev1.RegisterVolunteerServiceServer(s, h)
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop(); lis.Close() })

	c, err := client.New(client.ClientConfig{ServerURL: lis.Addr().String(), Insecure: true}, newTestLogger())
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return &ServerConnection{Name: "head-a", VolunteerID: "vol-1", Client: c,
		Config: config.ServerConfig{GRPCAddress: lis.Addr().String()}}
}

// checkpointBlob is a checkpoint archive holding one file of size bytes.
func checkpointBlob(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "state.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(size)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte{7}, size)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runReassignedUnit starts a slot on a unit the head says has a checkpoint and
// reports, from inside the run, the size of the restored file (-1 if absent)
// and whether the task list calls the task resumed.
func runReassignedUnit(t *testing.T, conn *ServerConnection) (restoredBytes int64, resumed bool) {
	t.Helper()
	sm := NewSlotManager(1, newTestLogger())
	d := newSlotTestDaemon()

	running := make(chan int64, 1)
	release := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{ID: "wu-reassigned", LeafID: "leaf-1",
			HasCheckpoint: true, CheckpointSequence: 3, CheckpointIntervalSeconds: 3600},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: t.TempDir()},
		Runtime: &mockRuntime{canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				size := int64(-1)
				if fi, err := os.Stat(filepath.Join(prep.WorkDir, "checkpoint", "state.bin")); err == nil {
					size = fi.Size()
				}
				running <- size
				select {
				case <-release:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok")}, nil
			}},
		Conn:      conn,
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-sm.available
	if err := sm.StartSlot(ctx, slotID, item, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	select {
	case restoredBytes = <-running:
	case <-time.After(30 * time.Second):
		t.Fatal("the unit never started")
	}
	tasks := sm.GetCurrentTasks(nil)
	if len(tasks) != 1 {
		t.Fatalf("%d active tasks, want 1", len(tasks))
	}
	resumed = tasks[0].ResumedFromCheckpoint
	close(release)
	sm.WaitForCompletion(ctx)
	return restoredBytes, resumed
}

// TestCheckpointRestore_OverFourMiBIsRestoredBeforeTheRunStarts: a 5 MiB
// checkpoint is in the work dir's checkpoint/ when the unit starts, and the
// task is reported resumed. Before, the download failed at 4 MiB, the unit
// started with an empty checkpoint/, and the task was reported resumed anyway.
func TestCheckpointRestore_OverFourMiBIsRestoredBeforeTheRunStarts(t *testing.T) {
	const size = 5 << 20
	conn := checkpointHeadConn(t, &checkpointHead{blob: checkpointBlob(t, size)})
	got, resumed := runReassignedUnit(t, conn)
	if got != size {
		t.Errorf("restored checkpoint file is %d bytes when the unit starts, want %d (-1: not restored)", got, size)
	}
	if !resumed {
		t.Error("task not reported resumed from checkpoint after a successful restore")
	}
}

// TestCheckpointRestore_FailedRestoreIsNotReportedResumed: when the checkpoint
// cannot be had (here the head refuses it), the unit starts fresh and the task
// list says so. Before, it said "resumed from checkpoint" whatever happened.
func TestCheckpointRestore_FailedRestoreIsNotReportedResumed(t *testing.T) {
	conn := checkpointHeadConn(t, &checkpointHead{err: status.Error(codes.PermissionDenied, "checkpoint belongs to another volunteer")})
	got, resumed := runReassignedUnit(t, conn)
	if got != -1 {
		t.Errorf("a checkpoint file of %d bytes is present after a refused restore", got)
	}
	if resumed {
		t.Error("task reported resumed from checkpoint although no checkpoint was restored")
	}
}
