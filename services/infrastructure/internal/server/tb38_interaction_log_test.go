package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
)

// TB-38: every volunteer↔leaf↔head interaction event must appear at production log
// level as one structured line carrying its inputs and its outcome. These tests pin
// the promoted/added lines; each one was a real head-side blind spot that forced an
// investigation into DB forensics (see docs/triage/TODO/TB-38).

// tb38Lines returns the log records in buf whose msg equals msg exactly.
func tb38Lines(buf *bytes.Buffer, msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if m, _ := rec["msg"].(string); m == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestHandOut_LoggedAtInfoWithTriple: a hand-out is an Info line per unit carrying
// the identifying triple (unit, leaf, volunteer) plus the requesting machine. It
// was Debug-only, so on a production head a unit could be handed out ~20 times with
// no log line at all (TB-35's zombies were proven from the CLIENT's receipts).
func TestHandOut_LoggedAtInfoWithTriple(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol, host := types.NewID(), types.NewID()
	if r, _ := c.HandOut(vol, hostOpts(vol, host, 4), 1); len(r) != 1 {
		t.Fatalf("hand-out = %d units, want 1", len(r))
	}

	lines := tb38Lines(buf, "hand-out")
	if len(lines) != 1 {
		t.Fatalf("hand-out log lines at Info = %d, want 1 — a production head must record every "+
			"unit it hands out; got log:\n%s", len(lines), buf.String())
	}
	rec := lines[0]
	if lvl, _ := rec["level"].(string); lvl != "INFO" {
		t.Errorf("level = %q, want INFO", lvl)
	}
	if got, _ := rec["work_unit_id"].(string); got != unitID.String() {
		t.Errorf("work_unit_id = %q, want %q", got, unitID.String())
	}
	if got, _ := rec["leaf_id"].(string); got != leafID.String() {
		t.Errorf("leaf_id = %q, want %q", got, leafID.String())
	}
	if got, _ := rec["volunteer_id"].(string); got != vol.String() {
		t.Errorf("volunteer_id = %q, want %q", got, vol.String())
	}
	if got, _ := rec["host_id"].(string); got != host.String() {
		t.Errorf("host_id = %q, want %q", got, host.String())
	}
	if _, ok := rec["reserved_until"]; !ok {
		t.Errorf("line must carry reserved_until: %v", rec)
	}
}

// TestStarvationWarn_CarriesRequestInputs: the throttled no-work WARN must carry the
// REQUEST's inputs (the LeafIDs filter and the requested batch size) next to its
// tallies. Without them, "did this client ask narrowly (a client bug) or broadly"
// was unanswerable from the head log — the question that stayed open in the
// 2026-08-01 starved-backfill case.
func TestStarvationWarn_CarriesRequestInputs(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	leafID := types.NewID()
	c.warm(gpuLeafFor(leafID, 8192), leafRepo)
	c.stageUnit(types.NewID(), leafID, 1, 0)

	// A machine that PINNED the leaf but is refused on VRAM: the WARN fires
	// (capability mismatch) and must show what the machine asked for.
	vol, host := types.NewID(), types.NewID()
	opts := hostOpts(vol, host, 4)
	opts.HasGPU = true
	opts.MaxGPUVRAMMB = 4096
	opts.GPUVendors = []string{"NVIDIA"}
	opts.LeafIDs = []types.ID{leafID}

	if r, _ := c.HandOut(vol, opts, 3); len(r) != 0 {
		t.Fatalf("hand-out = %d units, want 0", len(r))
	}

	lines := capabilityStarveLines(buf)
	if len(lines) != 1 {
		t.Fatalf("capability-mismatch WARN lines = %d, want 1; got log:\n%s", len(lines), buf.String())
	}
	rec := lines[0]
	if got, _ := rec["requested"].(float64); int(got) != 3 {
		t.Errorf("requested = %v, want 3 (the request's batch size is an input the WARN must carry)", rec["requested"])
	}
	ids, _ := rec["leaf_ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("leaf_ids = %v, want the request's one-leaf filter — without it the head cannot "+
			"say whether the client asked narrowly or broadly", rec["leaf_ids"])
	}
	if got, _ := ids[0].(string); got != leafID.String() {
		t.Errorf("leaf_ids[0] = %q, want %q", got, leafID.String())
	}
}

// TestFlushConflict_RevokedHandOutWarns: a hand-out whose reservation does not land
// (flush conflict) is silently revoked — the volunteer holds a unit the head has no
// claim row for, exactly the TB-35 zombie-window shape. The void must be WARN, not
// Debug: it is the tripwire if that window ever reopens.
func TestFlushConflict_RevokedHandOutWarns(t *testing.T) {
	wuRepo := &fakeWURepo{}
	// Nothing lands: every flushed reservation is a conflict.
	wuRepo.flushFn = func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
		return nil, nil
	}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(r) != 1 {
		t.Fatalf("hand-out = %d units, want 1", len(r))
	}
	c.flushBatch(context.Background(), false)

	var voidLines []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if m, _ := rec["msg"].(string); strings.Contains(m, "did not land") {
			voidLines = append(voidLines, rec)
		}
	}
	if len(voidLines) != 1 {
		t.Fatalf("non-landed-copy lines at a production level = %d, want 1 — a revoked hand-out "+
			"must be visible without Debug; got log:\n%s", len(voidLines), buf.String())
	}
	rec := voidLines[0]
	if lvl, _ := rec["level"].(string); lvl != "WARN" {
		t.Errorf("level = %q, want WARN", lvl)
	}
	if got, _ := rec["work_unit_id"].(string); got != unitID.String() {
		t.Errorf("work_unit_id = %q, want %q", got, unitID.String())
	}
	if got, _ := rec["volunteer_id"].(string); got != vol.String() {
		t.Errorf("volunteer_id = %q, want %q", got, vol.String())
	}
}

// staleAbandonWURepo answers every copy close with the 409 the repo emits when the
// volunteer holds no live copy of the unit.
type staleAbandonWURepo struct {
	bvMockWURepo
}

func (m *staleAbandonWURepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID, string) (workunit.ClosedCopy, error) {
	return workunit.ClosedCopy{}, apierror.Conflict("no live copy", nil)
}

// TestAbandonWorkUnit_StaleRefusalIsLogged: the FailedPrecondition "no live copy"
// answer ran ~50–97 times/host/day with zero head-side record — every grep for
// those exchanges came back empty. The refusal must log its identifying triple.
func TestAbandonWorkUnit_StaleRefusalIsLogged(t *testing.T) {
	svc, _, pub, volID, _ := shedTestService(t)
	logger, buf := capturingLogger()
	svc.logger = logger

	leafID := types.NewID()
	wuID := types.NewID()
	repo := &staleAbandonWURepo{}
	repo.wus = append(repo.wus, &workunit.WorkUnit{ID: wuID, LeafID: leafID})
	svc.wuRepo = repo

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	_, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volID.String(),
		Reason:      "prepare failed: artifact fetch",
	})
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	lines := tb38Lines(buf, "AbandonWorkUnit refused: no live copy for this volunteer")
	if len(lines) != 1 {
		t.Fatalf("refusal log lines = %d, want 1 — a refused RPC must leave a head-side record; got log:\n%s",
			len(lines), buf.String())
	}
	rec := lines[0]
	if got, _ := rec["work_unit_id"].(string); got != wuID.String() {
		t.Errorf("work_unit_id = %q, want %q", got, wuID.String())
	}
	if got, _ := rec["volunteer_id"].(string); got != volID.String() {
		t.Errorf("volunteer_id = %q, want %q", got, volID.String())
	}
	if got, _ := rec["leaf_id"].(string); got != leafID.String() {
		t.Errorf("leaf_id = %q, want %q", got, leafID.String())
	}
	if got, _ := rec["reason"].(string); got != "prepare failed: artifact fetch" {
		t.Errorf("reason = %q, want the client's reason text", got)
	}
}

// noReservationWURepo makes Assign fail the way it does when the volunteer's
// reservation lapsed or was voided (no live copy row to flip).
type noReservationWURepo struct {
	bvMockWURepo
}

func (m *noReservationWURepo) Assign(context.Context, types.ID, types.ID) (*workunit.WorkUnit, error) {
	return nil, apierror.NotFound("assignment", "none")
}

// TestStartWork_RefusalsAreLogged covers both StartWork denials: a terminal unit
// ("no longer dispatchable") and a lapsed reservation ("no longer reserved"). Both
// used to answer Ok:false with no log line at any level.
func TestStartWork_RefusalsAreLogged(t *testing.T) {
	svc, _, pub, volID, _ := shedTestService(t)
	logger, buf := capturingLogger()
	svc.logger = logger
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// Terminal unit → "no longer dispatchable".
	leafID := types.NewID()
	doneID := types.NewID()
	repo := &bvMockWURepo{wus: []*workunit.WorkUnit{
		{ID: doneID, LeafID: leafID, State: workunit.WorkUnitStateCompleted},
	}}
	svc.wuRepo = repo

	resp, err := svc.StartWork(ctx, &lettucev1.StartWorkRequest{
		WorkUnitId: doneID.String(), VolunteerId: volID.String(),
	})
	if err != nil || resp.Ok {
		t.Fatalf("StartWork on a COMPLETED unit = (%v, %v), want Ok:false", resp, err)
	}
	lines := tb38Lines(buf, "StartWork refused: work unit no longer dispatchable")
	if len(lines) != 1 {
		t.Fatalf("terminal-unit refusal log lines = %d, want 1; got log:\n%s", len(lines), buf.String())
	}
	if got, _ := lines[0]["leaf_id"].(string); got != leafID.String() {
		t.Errorf("leaf_id = %q, want %q", got, leafID.String())
	}

	// Lapsed reservation → "no longer reserved".
	buf.Reset()
	queuedID := types.NewID()
	noRes := &noReservationWURepo{}
	noRes.wus = append(noRes.wus, &workunit.WorkUnit{ID: queuedID, LeafID: leafID, State: workunit.WorkUnitStateQueued})
	svc.wuRepo = noRes

	resp, err = svc.StartWork(ctx, &lettucev1.StartWorkRequest{
		WorkUnitId: queuedID.String(), VolunteerId: volID.String(),
	})
	if err != nil || resp.Ok {
		t.Fatalf("StartWork with a lapsed reservation = (%v, %v), want Ok:false", resp, err)
	}
	lines = tb38Lines(buf, "StartWork refused: work unit no longer reserved for this volunteer")
	if len(lines) != 1 {
		t.Fatalf("lapsed-reservation refusal log lines = %d, want 1; got log:\n%s", len(lines), buf.String())
	}
	if got, _ := lines[0]["work_unit_id"].(string); got != queuedID.String() {
		t.Errorf("work_unit_id = %q, want %q", got, queuedID.String())
	}
}
