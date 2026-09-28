//go:build integration

package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestRestartedRun_SecondRunStartAndResultAreAccepted: a volunteer client can
// stop a running task and run the same unit again from the start, so that it
// picks up changed settings (the cores it is given). The head sees that as a
// second run-start of a unit the volunteer already holds as running. It must
// answer it as the same start — the copy stays the volunteer's and keeps its
// first start time, so the deadline still counts from then — and accept the
// result that follows. The client's restart relies on both.
func TestRestartedRun_SecondRunStartAndResultAreAccepted(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "restart")
	pub := genVolunteerKey(t)
	volID := registerBetaVolunteer(t, env, ctx, pub, "Restart Vol", nil)
	proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name: "Restarted Run", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: defaultExecConfig(), ValConfig: defaultHLValConfig(), FTConfig: defaultFTConfig(),
		DataConfig: defaultDataConfig(), CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1},
	})
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs/"+proj.ID.String()+"/work-units/generate",
		workunit.GenerateRequest{ParameterSpace: map[string]interface{}{"x": []interface{}{float64(1)}}})
	requireStatus(t, resp, http.StatusAccepted, "generate")
	resp.Body.Close()

	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pub), &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: pub})
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}
	wu := firstAssignment(t, wuResp)

	// startedAt reads the start time of the volunteer's live copy of the unit.
	startedAt := func() time.Time {
		t.Helper()
		var started time.Time
		if err := env.pool.QueryRow(ctx,
			`SELECT started_at FROM work_unit_assignment_history
			 WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`,
			wu.WorkUnitId, volID).Scan(&started); err != nil {
			t.Fatalf("read the copy's start: %v", err)
		}
		return started
	}
	runStart := func(label string) {
		t.Helper()
		sw, err := env.grpc.StartWork(signFor(t, ctx, pub), &lettucev1.StartWorkRequest{WorkUnitId: wu.WorkUnitId, VolunteerId: volID})
		if err != nil || !sw.GetOk() {
			t.Fatalf("%s: ok=%v err=%v (%s), want ok", label, sw.GetOk(), err, sw.GetMessage())
		}
	}

	runStart("first run-start")
	first := startedAt()
	time.Sleep(1100 * time.Millisecond)
	runStart("run-start after the restart")
	if again := startedAt(); !again.Equal(first) {
		t.Errorf("the second run-start moved the copy's start from %v to %v; the deadline must keep counting from the first", first, again)
	}

	output := []byte(`{"result": 1}`)
	sub, err := env.grpc.SubmitResult(signFor(t, ctx, pub), &lettucev1.SubmitResultRequest{
		WorkUnitId: wu.WorkUnitId, VolunteerId: volID, PublicKey: pub,
		OutputData: output, OutputChecksumSha256: sha256Hex(output),
		Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 4, CpuCoresUsed: 2},
	})
	if err != nil || !sub.GetAccepted() {
		t.Fatalf("result after the restart: accepted=%v err=%v, want accepted", sub.GetAccepted(), err)
	}
}
