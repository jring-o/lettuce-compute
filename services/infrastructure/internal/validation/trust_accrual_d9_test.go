package validation

import (
	"context"
	"errors"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// fakeRunnerSubjects is the trusted-runner registry provider stub for the D9 accrual-witness gate.
// It records how many times the engine queried it (the rule must query at most once per accrual).
type fakeRunnerSubjects struct {
	subjects []string
	err      error
	calls    int
}

func (f *fakeRunnerSubjects) ActiveRunnerSubjects(_ context.Context) ([]string, error) {
	f.calls++
	return f.subjects, f.err
}

// audit.RunnersRepository is the production implementation of the engine's consumer-side provider.
var _ runnerSubjectsProvider = (audit.RunnersRepository)(nil)

// The D9 witness rule (§7.6 / F-H1) gated on trusted-runner registry state. Floor is 25 (the
// accrualEngine default); trusted subjects score 30, newcomers 0.

// registry EMPTY -> the legacy single-trusted-witness rule still fires (guards the G2 default
// path; a head whose registry is empty on deploy day must not freeze newcomer accrual). Fails
// against an un-gated D9 upgrade that would demand a runner or two witnesses.
func TestTrustAccrualD9_RegistryEmptyLegacyRuleFires(t *testing.T) {
	trusted := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:trusted", 30)
	newcomer := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	prov := &fakeRunnerSubjects{subjects: nil} // registry empty
	engine, wuID := accrualEngine(t, repo, trusted, newcomer)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if repo.accrued["did:plc:newcomer"] != 1 {
		t.Errorf("newcomer accruals = %d, want 1 (legacy rule under an empty registry)", repo.accrued["did:plc:newcomer"])
	}
	if prov.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (queried once per accrual with a candidate)", prov.calls)
	}
}

// registry ACTIVE, the sole witness is a trusted NON-runner -> NO accrual. THE D9 regression: the
// legacy rule would accrue the newcomer here.
func TestTrustAccrualD9_ActiveRegistryNonRunnerWitnessNoAccrual(t *testing.T) {
	trusted := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:trusted", 30)
	newcomer := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	// A DIFFERENT active runner exists (registry active), but it is not a witness on this unit — and
	// an inactive/unregistered runner would likewise be absent from this set, counting only as a
	// plain trusted subject (rule (b)).
	prov := &fakeRunnerSubjects{subjects: []string{"did:plc:unrelated-runner"}}
	engine, wuID := accrualEngine(t, repo, trusted, newcomer)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if n := repo.totalAccruals(); n != 0 {
		t.Errorf("total accruals = %d, want 0 (one trusted non-runner witness is insufficient under D9)", n)
	}
	if prov.calls != 1 {
		t.Errorf("provider calls = %d, want 1", prov.calls)
	}
}

// registry ACTIVE, the trusted witness IS an active runner -> the newcomer accrues under rule (a)
// (the head's own corroborator single-witnesses newcomer bootstrap, preserving G2).
func TestTrustAccrualD9_ActiveRunnerWitnessAccrues(t *testing.T) {
	runner := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:runner", 30)
	newcomer := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	prov := &fakeRunnerSubjects{subjects: []string{"did:plc:runner"}} // the witness is an active runner
	engine, wuID := accrualEngine(t, repo, runner, newcomer)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if repo.accrued["did:plc:newcomer"] != 1 {
		t.Errorf("newcomer accruals = %d, want 1 (rule (a): a trusted active-runner witness)", repo.accrued["did:plc:newcomer"])
	}
	if repo.accrued["did:plc:runner"] != 0 {
		t.Errorf("runner accruals = %d, want 0 (it has no OTHER trusted witness)", repo.accrued["did:plc:runner"])
	}
}

// registry ACTIVE, TWO trusted non-runner witnesses -> the newcomer accrues under rule (b) (an
// inactive/unregistered runner counts as a plain trusted subject exactly like these).
func TestTrustAccrualD9_TwoTrustedNonRunnersAccrue(t *testing.T) {
	trustedA := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:a", 30)
	trustedB := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:b", 30)
	newcomer := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	prov := &fakeRunnerSubjects{subjects: []string{"did:plc:unrelated-runner"}} // active registry, no witness is a runner
	engine, wuID := accrualEngine(t, repo, trustedA, trustedB, newcomer)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if repo.accrued["did:plc:newcomer"] != 1 {
		t.Errorf("newcomer accruals = %d, want 1 (rule (b): >= 2 distinct trusted others)", repo.accrued["did:plc:newcomer"])
	}
	// Each trusted subject has only ONE trusted other (the sibling), which is not a runner -> under
	// D9 neither of them accrues.
	if repo.accrued["did:plc:a"] != 0 || repo.accrued["did:plc:b"] != 0 {
		t.Errorf("trusted A/B accruals = %d/%d, want 0/0 (one non-runner witness each)", repo.accrued["did:plc:a"], repo.accrued["did:plc:b"])
	}
}

// registry query ERROR -> fall back to the legacy rule + WARN (G2: a transient DB blip must not
// freeze newcomer bootstrap).
func TestTrustAccrualD9_RegistryErrorFallsBackToLegacy(t *testing.T) {
	trusted := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:trusted", 30)
	newcomer := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:newcomer", 0)
	repo := newFakeTrustRepo()
	prov := &fakeRunnerSubjects{err: errors.New("registry query blip")}
	engine, wuID := accrualEngine(t, repo, trusted, newcomer)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if repo.accrued["did:plc:newcomer"] != 1 {
		t.Errorf("newcomer accruals = %d, want 1 (legacy rule on a registry-query error)", repo.accrued["did:plc:newcomer"])
	}
	if prov.calls != 1 {
		t.Errorf("provider calls = %d, want 1", prov.calls)
	}
}

// No trusted subject at all -> nobody accrues and the registry is never queried (there is no
// candidate to decide, so the query is skipped).
func TestTrustAccrualD9_NoCandidateSkipsRegistryQuery(t *testing.T) {
	r1 := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:a", 0)
	r2 := stampSubject(makeResult(types.NewID(), types.NewID(), "aaaa", nil), "did:plc:b", 0)
	repo := newFakeTrustRepo()
	prov := &fakeRunnerSubjects{subjects: []string{"did:plc:runner"}}
	engine, wuID := accrualEngine(t, repo, r1, r2)
	engine.WithTrustedRunners(prov)

	if _, err := engine.TryValidate(context.Background(), wuID); err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if n := repo.totalAccruals(); n != 0 {
		t.Errorf("total accruals = %d, want 0 (no trusted witness)", n)
	}
	if prov.calls != 0 {
		t.Errorf("provider calls = %d, want 0 (no accrual candidate -> registry query skipped)", prov.calls)
	}
}
