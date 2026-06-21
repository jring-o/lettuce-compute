package validation

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

func TestSpotCheck_WaitsForTwoResults(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()

	// Project with redundancy=1 but spot-check enabled.
	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.SpotCheckEnabled = true
	proj.ValidationConfig.SpotCheckPercentage = 5.0

	// Work unit marked as spot-check.
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	wu.SpotCheck = true

	// Only one result so far.
	r1 := makeResult(wuID, vol1, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, nil, nil, nil, nil, nil, nil, nil, testLogger())

	// With only 1 result and effective redundancy=2, should return nil.
	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr != nil {
		t.Errorf("expected nil ValidationResult with only 1 result, got outcome=%q", vr.Outcome)
	}
}

func TestSpotCheck_BothAgree_BothGetCredit(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	checksum := "aaaa"

	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.SpotCheckEnabled = true
	proj.ValidationConfig.SpotCheckPercentage = 5.0

	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	wu.SpotCheck = true

	r1 := makeResult(wuID, vol1, checksum, nil)
	r2 := makeResult(wuID, vol2, checksum, nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2", len(vr.CreditEntries))
	}
	if wu.State != workunit.WorkUnitStateValidated {
		t.Errorf("work unit state = %s, want VALIDATED", wu.State)
	}
	// Both volunteers should have their completed count incremented.
	if volRepo.completedInc[vol1] != 1 {
		t.Errorf("vol1 completed increments = %d, want 1", volRepo.completedInc[vol1])
	}
	if volRepo.completedInc[vol2] != 1 {
		t.Errorf("vol2 completed increments = %d, want 1", volRepo.completedInc[vol2])
	}
}

func TestSpotCheck_Disagree_BothRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.SpotCheckEnabled = true
	proj.ValidationConfig.SpotCheckPercentage = 5.0

	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	wu.SpotCheck = true

	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil) // Different checksum

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()
	// No active assignments = all done.

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Errorf("Outcome = %q, want REJECTED", vr.Outcome)
	}
	if len(vr.RejectedResults) != 2 {
		t.Errorf("RejectedResults = %d, want 2", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 0 {
		t.Errorf("CreditEntries = %d, want 0", len(vr.CreditEntries))
	}
	// Both volunteers should have rejection incremented.
	if volRepo.rejectedInc[vol1] != 1 {
		t.Errorf("vol1 rejected increments = %d, want 1", volRepo.rejectedInc[vol1])
	}
	if volRepo.rejectedInc[vol2] != 1 {
		t.Errorf("vol2 rejected increments = %d, want 1", volRepo.rejectedInc[vol2])
	}
}

func TestSpotCheck_NonSpotCheckWU_UsesRedundancyFactor(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()

	// Project with redundancy=1, spot-check enabled, but WU is NOT spot-checked.
	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.SpotCheckEnabled = true
	proj.ValidationConfig.SpotCheckPercentage = 5.0

	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	wu.SpotCheck = false // NOT a spot-check WU

	r1 := makeResult(wuID, vol1, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, nil, testLogger())

	// With redundancy=1 and 1 result, should validate immediately.
	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 1 {
		t.Errorf("AgreedResults = %d, want 1", len(vr.AgreedResults))
	}
}

func TestSpotCheck_DisagreeWithActiveAssignment_Pending(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	wu.SpotCheck = true

	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()
	// One active assignment still in progress — should return PENDING.
	assignRepo.activeCount[wuID] = 1

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomePending {
		t.Errorf("Outcome = %q, want PENDING (active assignment exists)", vr.Outcome)
	}
}

