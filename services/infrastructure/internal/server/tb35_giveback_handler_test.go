package server

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TB-35: the AbandonWorkUnit handler routes a wire-flagged un-run give-back
// (unrun_giveback) to the RETURNED close outcome — budget-neutral — and a plain
// abandon to ABANDONED, exactly as before. The started-copy verification lives in
// the repo write (CloseCopyByVolunteer) and is pinned by its own integration test;
// this pins the handler's outcome selection.

// givebackRecordingWURepo wraps the bv mock to record the outcome the handler
// closes the copy with.
type givebackRecordingWURepo struct {
	bvMockWURepo
	lastCloseOutcome string
}

func (m *givebackRecordingWURepo) CloseCopyByVolunteer(_ context.Context, _ types.ID, _ types.ID, outcome string, _ *types.ID, _ string) (workunit.ClosedCopy, error) {
	m.lastCloseOutcome = outcome
	return workunit.ClosedCopy{Outcome: outcome}, nil
}

func TestAbandonWorkUnit_GivebackFlagSelectsReturnedOutcome(t *testing.T) {
	svc, _, pub, volID, wuID := shedTestService(t)
	repo := &givebackRecordingWURepo{}
	svc.wuRepo = repo

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	// Flagged give-back → RETURNED.
	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:    wuID.String(),
		VolunteerId:   volID.String(),
		Reason:        "work buffer full (over the hours target)",
		UnrunGiveback: true,
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(giveback): %v", err)
	}
	if repo.lastCloseOutcome != "RETURNED" {
		t.Fatalf("giveback closed copy with %q, want RETURNED", repo.lastCloseOutcome)
	}

	// Plain abandon → ABANDONED, unchanged.
	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volID.String(),
		Reason:      "prepare failed: no binary for platform",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(plain): %v", err)
	}
	if repo.lastCloseOutcome != "ABANDONED" {
		t.Fatalf("plain abandon closed copy with %q, want ABANDONED", repo.lastCloseOutcome)
	}
}
