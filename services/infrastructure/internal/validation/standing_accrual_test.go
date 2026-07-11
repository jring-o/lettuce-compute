package validation

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// stampProbation marks a result's submit-time standing PROBATION — non-countable, so trust
// accrual must treat it as invisible: it neither accrues trust nor witnesses another subject's
// accrual (BG-24b).
func stampProbation(r *result.Result) *result.Result {
	st := volunteer.StandingProbation
	r.StandingAtSubmit = &st
	return r
}

// A probation-stamped agreeing result earns no trust AND does not enable others' accrual.
//
// Two OK-standing but UNTRUSTED subjects (a, b; stamped score 0) meet the quorum and validate
// the unit — the trust gate is off, so untrusted agreement still validates. A third agreeing
// subject t is TRUSTED (stamped score 30 >= floor 25) but PROBATION-stamped. If t were counted,
// it would be the trusted witness that lets a and b each accrue; because a probation result is
// skipped in accrual, t witnesses nothing, so NOBODY accrues — and t itself earns nothing.
func TestTrustAccrual_ProbationNeitherAccruesNorWitnesses(t *testing.T) {
	okA := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:a", 0)
	okB := stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:b", 0)
	probationTrusted := stampProbation(stampSubject(makeResult(types.NewID(), types.NewID(), inlineAgreeCk, inlineAgreeData), "did:plc:t", 30))
	repo := newFakeTrustRepo()
	engine, wuID := accrualEngine(t, repo, okA, okB, probationTrusted)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (probation results ride along in the accepted set)", vr.Outcome)
	}
	if got := repo.accrued["did:plc:t"]; got != 0 {
		t.Errorf("probation subject accruals = %d, want 0 (a non-countable result earns nothing)", got)
	}
	if got := repo.accrued["did:plc:a"]; got != 0 {
		t.Errorf("subject a accruals = %d, want 0 (its only would-be witness is a skipped probation subject)", got)
	}
	if got := repo.accrued["did:plc:b"]; got != 0 {
		t.Errorf("subject b accruals = %d, want 0 (same skipped-witness reason)", got)
	}
	if n := repo.totalAccruals(); n != 0 {
		t.Errorf("total accruals = %d, want 0", n)
	}
}
