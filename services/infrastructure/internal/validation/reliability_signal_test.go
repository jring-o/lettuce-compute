package validation

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// fakeReliabilityRepo records RecordOutcome calls for the #54 signal assertions.
type fakeReliabilityRepo struct {
	recorded []relCall
}

type relCall struct {
	host types.ID
	good bool
}

func (f *fakeReliabilityRepo) RecordOutcome(_ context.Context, hostID types.ID, good bool) error {
	f.recorded = append(f.recorded, relCall{host: hostID, good: good})
	return nil
}

func (f *fakeReliabilityRepo) ListBudgetInputs(_ context.Context) ([]reliability.BudgetInput, error) {
	return nil, nil
}

func (f *fakeReliabilityRepo) countGood() (good, bad int) {
	for _, c := range f.recorded {
		if c.good {
			good++
		} else {
			bad++
		}
	}
	return good, bad
}

func (f *fakeReliabilityRepo) outcomeFor(host types.ID) (good bool, found bool) {
	for _, c := range f.recorded {
		if c.host == host {
			return c.good, true
		}
	}
	return false, false
}

// TestReliabilitySignal_AgreedRecordsGood verifies acceptResults feeds a GOOD reliability
// signal for each agreed result, keyed on the producing MACHINE (host_id when present, else
// the account id — the per-account fallback).
func TestReliabilitySignal_AgreedRecordsGood(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	host1 := types.NewID() // vol1 reports a host; vol2 reports none

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r1.HostID = &host1
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	relRepo := &fakeReliabilityRepo{}
	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, relRepo, nil, testLogger(), nil, transition.TrustPolicy{})

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	good, bad := relRepo.countGood()
	if good != 2 || bad != 0 {
		t.Fatalf("reliability signals: good=%d bad=%d, want good=2 bad=0", good, bad)
	}
	// vol1's signal is keyed on its host id; vol2 (no host) folds onto its account id.
	if g, ok := relRepo.outcomeFor(host1); !ok || !g {
		t.Errorf("host1 outcome = (%v, found=%v), want good=true", g, ok)
	}
	if g, ok := relRepo.outcomeFor(vol2); !ok || !g {
		t.Errorf("vol2 (account fallback) outcome = (%v, found=%v), want good=true", g, ok)
	}
}

// TestReliabilitySignal_RejectedRecordsBad verifies rejectAll feeds a BAD reliability signal
// for every wasted result.
func TestReliabilitySignal_RejectedRecordsBad(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil) // disagree -> no quorum -> rejectAll

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	relRepo := &fakeReliabilityRepo{}
	// No active assignments => threshold unmet => rejectAll.
	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, relRepo, nil, testLogger(), nil, transition.TrustPolicy{})

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
	}

	good, bad := relRepo.countGood()
	if good != 0 || bad != 2 {
		t.Fatalf("reliability signals: good=%d bad=%d, want good=0 bad=2", good, bad)
	}
}
