package validation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// bpCall is one recorded RecordAdjudicated fold: the volunteer whose signal was folded and
// whether the adjudicated result AGREED. Comparable (types.ID is a uuid.UUID array), so it
// doubles as a multiset key — folds are asserted order-independently.
type bpCall struct {
	vol    types.ID
	agreed bool
}

// fakeStandingRecorder is a standing.Recorder that records every fold and returns a
// configurable outcome/error, so a test can exercise the engine's best-effort handling
// (error swallowed, Applied=false ignored, transition tolerated) without a database. The
// engine's accept/reject loops are sequential (see acceptResults / rejectAll), so no mutex
// is needed.
type fakeStandingRecorder struct {
	calls []bpCall
	// outcome is returned when err is nil; a nil outcome defaults to an applied, no-op fold
	// (Applied=true, standing unchanged) so the common case records folds without side effects.
	outcome *standing.AdjudicationOutcome
	err     error
}

func (f *fakeStandingRecorder) RecordAdjudicated(_ context.Context, volunteerID types.ID, agreed bool) (*standing.AdjudicationOutcome, error) {
	f.calls = append(f.calls, bpCall{vol: volunteerID, agreed: agreed})
	if f.err != nil {
		return nil, f.err
	}
	if f.outcome != nil {
		return f.outcome, nil
	}
	return &standing.AdjudicationOutcome{Applied: true, OldStanding: "OK", NewStanding: "OK"}, nil
}

// multiset counts the recorded folds so assertions compare them order-independently.
func multiset(calls []bpCall) map[bpCall]int {
	m := make(map[bpCall]int, len(calls))
	for _, c := range calls {
		m[c]++
	}
	return m
}

// newValidateFixture wires an engine over a single COMPLETED unit that reaches a MIXED
// verdict: two authors agree (checksum "aaaa", the majority) and one dissents (checksum
// "bbbb", the minority). With redundancy 2 (so min_quorum resolves to 2) and a 0.6
// agreement threshold, the majority of 2 out of 3 subjects clears all four gates
// (ratio 0.66 >= 0.6, majority 2 >= quorum 2, strict majority 4 > 3, trust gate off), so
// acceptResults runs — folding agreed=true for each majority author and agreed=false for
// the minority. Only the standing recorder is wired (reliability/trust/attestation repos are
// nil) so the folds under test are isolated. Passing rec == nil leaves the default nil
// recorder (WithStandingBackpressure is not called), reproducing the machine-disabled path.
func newValidateFixture(rec standing.Recorder) (engine *Engine, wuID, vMaj1, vMaj2, vMin types.ID) {
	leafID := types.NewID()
	wuID = types.NewID()
	vMaj1 = types.NewID()
	vMaj2 = types.NewID()
	vMin = types.NewID()

	proj := makeLeaf(leafID, 2, 0.6, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vMaj1, "aaaa", nil)
	r2 := makeResult(wuID, vMaj2, "aaaa", nil)
	r3 := makeResult(wuID, vMin, "bbbb", nil) // dissents -> minority -> agreed=false

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	resultRepo.addResult(r3)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vMaj1))
	volRepo.addVolunteer(makeVolunteer(vMaj2))
	volRepo.addVolunteer(makeVolunteer(vMin))

	engine = NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
	if rec != nil {
		engine = engine.WithStandingBackpressure(rec)
	}
	return engine, wuID, vMaj1, vMaj2, vMin
}

// newRejectFixture wires an engine over a single COMPLETED unit whose two authors disagree
// (distinct checksums) with no active assignments, so no quorum forms and the unit routes to
// rejectAll — folding agreed=false for every pending author. As with newValidateFixture, only
// the standing recorder is wired, and rec == nil leaves the default nil recorder.
func newRejectFixture(rec standing.Recorder) (engine *Engine, wuID, v1, v2 types.ID) {
	leafID := types.NewID()
	wuID = types.NewID()
	v1 = types.NewID()
	v2 = types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, v1, "aaaa", nil)
	r2 := makeResult(wuID, v2, "bbbb", nil) // disagree -> no quorum -> rejectAll

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(v1))
	volRepo.addVolunteer(makeVolunteer(v2))

	// No active assignments => threshold unmet => rejectAll.
	engine = NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
	if rec != nil {
		engine = engine.WithStandingBackpressure(rec)
	}
	return engine, wuID, v1, v2
}

// TestStandingBackpressure_ValidatedMixedVerdict_RecordsPerAuthor verifies acceptResults folds
// exactly one adjudicated outcome per author: agreed=true for each majority author, agreed=false
// for the minority author. Folds are compared as a multiset (order-independent).
func TestStandingBackpressure_ValidatedMixedVerdict_RecordsPerAuthor(t *testing.T) {
	rec := &fakeStandingRecorder{}
	engine, wuID, vMaj1, vMaj2, vMin := newValidateFixture(rec)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 || len(vr.RejectedResults) != 1 {
		t.Fatalf("agreed=%d rejected=%d, want agreed=2 rejected=1", len(vr.AgreedResults), len(vr.RejectedResults))
	}

	want := map[bpCall]int{
		{vol: vMaj1, agreed: true}: 1,
		{vol: vMaj2, agreed: true}: 1,
		{vol: vMin, agreed: false}: 1,
	}
	if got := multiset(rec.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded folds = %v, want %v", got, want)
	}
}

// TestStandingBackpressure_RejectAll_RecordsAllDisagreed verifies rejectAll folds agreed=false
// for every pending author and never folds an agreed=true.
func TestStandingBackpressure_RejectAll_RecordsAllDisagreed(t *testing.T) {
	rec := &fakeStandingRecorder{}
	engine, wuID, v1, v2 := newRejectFixture(rec)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
	}

	want := map[bpCall]int{
		{vol: v1, agreed: false}: 1,
		{vol: v2, agreed: false}: 1,
	}
	if got := multiset(rec.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded folds = %v, want %v", got, want)
	}
	for _, c := range rec.calls {
		if c.agreed {
			t.Errorf("rejectAll folded agreed=true for %v; want every author folded agreed=false", c.vol)
		}
	}
}

// TestStandingBackpressure_BestEffortErrorDoesNotAffectOutcome verifies a recorder that always
// errors is swallowed: the validation outcome is unchanged and every author is still folded (the
// error is logged and skipped, not propagated). Reaching the assertions without the test process
// dying is the no-panic proof.
func TestStandingBackpressure_BestEffortErrorDoesNotAffectOutcome(t *testing.T) {
	t.Run("validate", func(t *testing.T) {
		rec := &fakeStandingRecorder{err: errors.New("recorder unavailable")}
		engine, wuID, _, _, _ := newValidateFixture(rec)

		vr, err := engine.TryValidate(context.Background(), wuID)
		if err != nil {
			t.Fatalf("TryValidate: %v", err)
		}
		if vr.Outcome != OutcomeValidated {
			t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
		}
		if len(vr.AgreedResults) != 2 || len(vr.RejectedResults) != 1 {
			t.Fatalf("agreed=%d rejected=%d, want agreed=2 rejected=1", len(vr.AgreedResults), len(vr.RejectedResults))
		}
		if len(rec.calls) != 3 {
			t.Fatalf("recorder folds = %d, want 3 (error swallowed, every author still folded)", len(rec.calls))
		}
	})

	t.Run("reject", func(t *testing.T) {
		rec := &fakeStandingRecorder{err: errors.New("recorder unavailable")}
		engine, wuID, _, _ := newRejectFixture(rec)

		vr, err := engine.TryValidate(context.Background(), wuID)
		if err != nil {
			t.Fatalf("TryValidate: %v", err)
		}
		if vr.Outcome != OutcomeRejected {
			t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
		}
		if len(rec.calls) != 2 {
			t.Fatalf("recorder folds = %d, want 2 (error swallowed, every author still folded)", len(rec.calls))
		}
	})
}

// TestStandingBackpressure_NilRecorderPreservesOutcome verifies an engine built WITHOUT
// WithStandingBackpressure (the default nil recorder — machine disabled) validates and rejects
// exactly as before. recordAdjudicated falls back to the legacy lifetime-rate path for rejected
// results; the template's testLogger writes to stderr rather than capturing logs, so only the
// validation outcome is asserted (no log assertion). No panic on the nil recorder path.
func TestStandingBackpressure_NilRecorderPreservesOutcome(t *testing.T) {
	t.Run("validate", func(t *testing.T) {
		engine, wuID, _, _, _ := newValidateFixture(nil)

		vr, err := engine.TryValidate(context.Background(), wuID)
		if err != nil {
			t.Fatalf("TryValidate: %v", err)
		}
		if vr.Outcome != OutcomeValidated {
			t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
		}
		if len(vr.AgreedResults) != 2 || len(vr.RejectedResults) != 1 {
			t.Fatalf("agreed=%d rejected=%d, want agreed=2 rejected=1", len(vr.AgreedResults), len(vr.RejectedResults))
		}
	})

	t.Run("reject", func(t *testing.T) {
		engine, wuID, _, _ := newRejectFixture(nil)

		vr, err := engine.TryValidate(context.Background(), wuID)
		if err != nil {
			t.Fatalf("TryValidate: %v", err)
		}
		if vr.Outcome != OutcomeRejected {
			t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
		}
		if len(vr.RejectedResults) != 2 {
			t.Fatalf("rejected=%d, want 2", len(vr.RejectedResults))
		}
	})
}

// TestStandingBackpressure_AppliedFalseToleratedSilently verifies an Applied=false outcome (an
// OPERATOR-owned row, or a vanished volunteer, that the machine never touches) is tolerated: the
// unit still validates and every author is still folded.
func TestStandingBackpressure_AppliedFalseToleratedSilently(t *testing.T) {
	rec := &fakeStandingRecorder{outcome: &standing.AdjudicationOutcome{Applied: false}}
	engine, wuID, _, _, _ := newValidateFixture(rec)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(rec.calls) != 3 {
		t.Fatalf("recorder folds = %d, want 3", len(rec.calls))
	}
}

// TestStandingBackpressure_TransitionOutcomeTolerated verifies an applied standing transition
// (OK -> PROBATION with a rate/sample set) is tolerated and does not change the validation
// outcome. The template's testLogger writes to stderr rather than capturing logs, so the
// transition WARN itself is not asserted here.
func TestStandingBackpressure_TransitionOutcomeTolerated(t *testing.T) {
	rec := &fakeStandingRecorder{outcome: &standing.AdjudicationOutcome{
		Applied:     true,
		OldStanding: "OK",
		NewStanding: "PROBATION",
		Rate:        0.35,
		Sample:      12,
	}}
	engine, wuID, _, _, _ := newValidateFixture(rec)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(rec.calls) != 3 {
		t.Fatalf("recorder folds = %d, want 3", len(rec.calls))
	}
}
