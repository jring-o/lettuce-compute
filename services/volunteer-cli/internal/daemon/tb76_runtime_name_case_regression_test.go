package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-76 regression tests.
//
// The exit-137 memory explanation shipped for TB-63 compared the unit's
// runtime with the lower-case "container", while every head writes the
// leaf's execution_config.runtime on the assignment verbatim — the enum name,
// "CONTAINER". The note therefore never fired in production: the head's
// abandon reasons stayed "non-zero exit code 137; output: …" and the
// leaf-failing notice never said "killed for memory". The regression test
// passed because its fixture used the client's spelling, which the head never
// sends. Now the runtime name is normalised once, where the assignment is
// converted (runtime.WorkUnitFromProto) and where a persisted task is loaded
// (a file written by an older build carries the head's spelling), so every
// later comparison sees the one canonical form.

// headContainerAssignment is a work unit exactly as a head sends it: the
// runtime is the enum name, upper-case.
func headContainerAssignment(id, leafID, image string, memMB int32) *lettucev1.WorkUnitAssignment {
	return &lettucev1.WorkUnitAssignment{
		WorkUnitId: id,
		LeafId:     leafID,
		Runtime:    "CONTAINER",
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Image:       image,
			MaxMemoryMb: memMB,
		},
	}
}

// headContainerUnit is headContainerAssignment converted the way the fetcher
// converts every assignment it receives. Daemon tests that need a container
// unit build it through here, so a test cannot pass on a spelling the head
// never sends.
func headContainerUnit(id, leafID, image string, memMB int32) *runtime.WorkUnit {
	return runtime.WorkUnitFromProto(headContainerAssignment(id, leafID, image, memMB))
}

// TestTB76_WorkUnitFromProtoNormalisesTheRuntimeName: the head's "CONTAINER"
// (and any other spelling) becomes the canonical lower-case name at the
// boundary, so the daemon compares one form everywhere.
func TestTB76_WorkUnitFromProtoNormalisesTheRuntimeName(t *testing.T) {
	for _, tc := range []struct{ sent, want string }{
		{"CONTAINER", runtime.RuntimeContainer},
		{"container", runtime.RuntimeContainer},
		{"Native", runtime.RuntimeNative},
		{" WASM ", runtime.RuntimeWasm},
		{"", ""},
	} {
		wu := runtime.WorkUnitFromProto(&lettucev1.WorkUnitAssignment{WorkUnitId: "wu", Runtime: tc.sent})
		if wu.Runtime != tc.want {
			t.Errorf("WorkUnitFromProto(Runtime %q).Runtime = %q, want %q", tc.sent, wu.Runtime, tc.want)
		}
	}
}

// TestTB76_Exit137NoteFiresOnTheHeadsSpelling is the reproduction: the
// TB-63 scenario (a 7000 MB unit on a machine whose engine VM holds 1536 MB)
// with the unit built exactly as the head sends it. Pre-fix the abandon
// reason was "non-zero exit code 137; output: …" — no note — and the
// leaf-failing notice told the volunteer to report the leaf to the head's
// operator.
func TestTB76_Exit137NoteFiresOnTheHeadsSpelling(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.containerFactory = tb63Factory(t, d, 2048)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	mc := &mockClient{}
	wu := headContainerUnit("wu-137", "leaf-grep", "ghcr.io/example/grep:1.2", 7000)
	for i := 0; i < leafFailurePauseThreshold; i++ {
		d.handleSlotResult(context.Background(), SlotResult{
			WU: wu, Conn: handleSlotResultTestConn(mc),
			Result:         &runtime.ExecutionResult{ExitCode: 137},
			FailureLogTail: "[lettuce-student] providers=['CPUExecutionProvider']",
		})
	}
	if mc.lastAbandonReq == nil {
		t.Fatal("no abandon request recorded")
	}
	reason := mc.lastAbandonReq.Reason
	for _, want := range []string{"non-zero exit code 137 (killed for memory", "7000 MB", "1536 MB", "2048 MB"} {
		if !strings.Contains(reason, want) {
			t.Errorf("abandon reason lacks %q: %s", want, reason)
		}
	}
	if strings.Contains(reason, "137; output:") {
		t.Errorf("abandon reason carries the bare exit code with no note: %s", reason)
	}
	_, notice := countNoticesByCode(d.notices, "leaf_failing")
	if !strings.Contains(notice.Message, "killed for memory") {
		t.Errorf("leaf_failing notice does not carry the memory diagnosis: %+v", notice)
	}

	// The in-budget kill is explained too, at the unit's own limit.
	mc = &mockClient{}
	small := headContainerUnit("wu-small", "leaf-grep", "ghcr.io/example/grep:1.2", 1024)
	d.handleSlotResult(context.Background(), SlotResult{WU: small, Conn: handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 137}})
	if got := mc.lastAbandonReq.Reason; !strings.Contains(got, "usually out of memory at its 1024 MB limit") {
		t.Errorf("in-budget 137 reason = %q, want the out-of-memory hint at the unit's own limit", got)
	}
}

// TestTB76_PersistedTaskRuntimeNameIsNormalised: a task file written by an
// older build carries the head's spelling; it loads as the canonical name, so
// a unit adopted on relaunch (TB-74) gets the same 137 diagnosis as a fresh
// one, and the runtime lookup on resume does not depend on case.
func TestTB76_PersistedTaskRuntimeNameIsNormalised(t *testing.T) {
	dir := t.TempDir()
	state := PersistedState{Tasks: []PersistedTask{
		{WorkUnitID: "wu-a", RuntimeName: "CONTAINER"},
		{WorkUnitID: "wu-b", RuntimeName: "Native"},
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{activeTasksPath(dir), bufferedTasksPath(dir)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, load := range []struct {
		name string
		fn   func(string) (*PersistedState, error)
	}{{"LoadActiveState", LoadActiveState}, {"LoadBufferState", LoadBufferState}} {
		got, err := load.fn(dir)
		if err != nil {
			t.Fatalf("%s: %v", load.name, err)
		}
		if got == nil || len(got.Tasks) != 2 {
			t.Fatalf("%s: tasks = %+v, want 2", load.name, got)
		}
		if got.Tasks[0].RuntimeName != runtime.RuntimeContainer || got.Tasks[1].RuntimeName != runtime.RuntimeNative {
			t.Errorf("%s: runtime names = %q / %q, want %q / %q", load.name,
				got.Tasks[0].RuntimeName, got.Tasks[1].RuntimeName, runtime.RuntimeContainer, runtime.RuntimeNative)
		}
	}
}
