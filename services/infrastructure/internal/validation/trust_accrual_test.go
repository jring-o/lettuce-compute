package validation

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// fakeTrustRepo records AccrueCleanUnit calls per subject for the accrual assertions. Every
// other method is an inert stub — acceptResults only ever accrues.
type fakeTrustRepo struct {
	accrued map[string]int
}

func newFakeTrustRepo() *fakeTrustRepo { return &fakeTrustRepo{accrued: map[string]int{}} }

func (f *fakeTrustRepo) AccrueCleanUnit(_ context.Context, subject string) error {
	f.accrued[subject]++
	return nil
}
func (f *fakeTrustRepo) GetScore(context.Context, string) (int, error)      { return 0, nil }
func (f *fakeTrustRepo) Get(context.Context, string) (*trust.Entry, error)  { return nil, nil }
func (f *fakeTrustRepo) SetScore(context.Context, string, int) error        { return nil }
func (f *fakeTrustRepo) Slash(context.Context, string) error                { return nil }
func (f *fakeTrustRepo) List(context.Context, int, int) ([]*trust.Entry, error) {
	return nil, nil
}
func (f *fakeTrustRepo) AllScores(context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}

func (f *fakeTrustRepo) totalAccruals() int {
	n := 0
	for _, c := range f.accrued {
		n += c
	}
	return n
}

// stampSubject stamps a result with an explicit trust subject + submission-time score.
func stampSubject(r *result.Result, subject string, score int) *result.Result {
	s := subject
	sc := score
	r.TrustSubject = &s
	r.TrustScoreAtSubmit = &sc
	return r
}

// accrualEngine wires an engine with a fake trust repo and a floor-25 policy. The gate is left
// OFF (K == 0), so validation is not gated but accrual still resolves the real floor — exactly
// the "accumulate trust before enforcement" configuration.
func accrualEngine(t *testing.T, trustRepo trust.Repository, results ...*result.Result) (*Engine, types.ID) {
	t.Helper()
	leafID := types.NewID()
	wuID := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)

	resultRepo := newMockResultRepo()
	volRepo := newMockVolunteerRepo()
	for _, r := range results {
		r.WorkUnitID = wuID // makeResult stamped a throwaway unit id; point it at this unit
		resultRepo.addResult(r)
		volRepo.addVolunteer(makeVolunteer(r.VolunteerID))
	}
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	tp := transition.TrustPolicy{DefaultFloor: 25} // gate off, floor 25
	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo,
		newMockAssignmentRepo(), nil, nil, nil, testLogger(), trustRepo, tp)
	return engine, wuID
}

// (i) two agreed subjects, one trusted -> the untrusted subject accrues (its trusted peer is
// the witness) but the trusted subject does NOT (it has no OTHER trusted witness). The exact
// asymmetry the Sybil rule requires.
func TestTrustAccrual_TrustedWitnessAsymmetry(t *testing.T) {
	trustedRes := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:trusted", 30)
	untrustedRes := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	engine, wuID := accrualEngine(t, repo, trustedRes, untrustedRes)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if repo.accrued["did:plc:newcomer"] != 1 {
		t.Errorf("newcomer accruals = %d, want 1 (corroborated by a trusted witness)", repo.accrued["did:plc:newcomer"])
	}
	if repo.accrued["did:plc:trusted"] != 0 {
		t.Errorf("trusted accruals = %d, want 0 (no OTHER trusted witness)", repo.accrued["did:plc:trusted"])
	}
}

// (ii) no trusted subject -> zero accruals (a Sybil ring cannot bootstrap itself).
func TestTrustAccrual_NoTrustedWitnessNoAccrual(t *testing.T) {
	r1 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:a", 0)
	r2 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:b", 10) // below floor 25
	repo := newFakeTrustRepo()
	engine, wuID := accrualEngine(t, repo, r1, r2)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if n := repo.totalAccruals(); n != 0 {
		t.Errorf("total accruals = %d, want 0 (no trusted witness)", n)
	}
}

// (iii) a subject running two devices (same DID, two results) accrues at most ONCE per unit.
func TestTrustAccrual_MultiDeviceSubjectAccruesOnce(t *testing.T) {
	// Subject A on two devices (distinct volunteers, one DID), plus trusted subject B — both
	// trusted (score 30 >= floor 25), so both have a trusted OTHER and accrue.
	aDev1 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:a", 30)
	aDev2 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:a", 30)
	b := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:b", 30)
	repo := newFakeTrustRepo()
	engine, wuID := accrualEngine(t, repo, aDev1, aDev2, b)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if repo.accrued["did:plc:a"] != 1 {
		t.Errorf("subject A accruals = %d, want 1 (two devices, one principal)", repo.accrued["did:plc:a"])
	}
	if repo.accrued["did:plc:b"] != 1 {
		t.Errorf("subject B accruals = %d, want 1", repo.accrued["did:plc:b"])
	}
}

// (iv) a nil trust repo (feature off) never panics.
func TestTrustAccrual_NilRepoNoPanic(t *testing.T) {
	r1 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:a", 30)
	r2 := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:b", 30)
	engine, wuID := accrualEngine(t, nil, r1, r2)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
}

