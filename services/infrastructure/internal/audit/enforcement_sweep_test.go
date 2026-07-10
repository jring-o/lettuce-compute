package audit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- fakes over the EnforcementDeps seams -----------------------------------------------

// enfAuditsFake is a stateful AuditsRepository double for the enforcement pass. It embeds the
// handler-test fakeAuditsRepo for the methods the pass never calls, and overrides the four it
// does (ConfirmationsForRoot / EnqueueConfirmation / GetRunnerOutput / SetEnforcementState).
type enfAuditsFake struct {
	*fakeAuditsRepo

	confirms      []*Audit
	confirmsCalls int
	confirmsErr   error

	enqueueCalls int
	enqueueErr   error

	outputs   map[types.ID][]byte
	outputErr error

	setStates    []EnforcementState
	stateByID    map[types.ID]EnforcementState
	setErr       error
	setGuardMiss bool
}

func (f *enfAuditsFake) ConfirmationsForRoot(context.Context, types.ID) ([]*Audit, error) {
	f.confirmsCalls++
	return f.confirms, f.confirmsErr
}

func (f *enfAuditsFake) EnqueueConfirmation(_ context.Context, rootID types.ID) (*Audit, error) {
	f.enqueueCalls++
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	return &Audit{ID: types.NewID(), ConfirmsAuditID: &rootID, Status: StatusQueued}, nil
}

func (f *enfAuditsFake) GetRunnerOutput(_ context.Context, id types.ID) ([]byte, error) {
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	return f.outputs[id], nil
}

func (f *enfAuditsFake) SetEnforcementState(_ context.Context, id types.ID, s EnforcementState) (bool, error) {
	if f.setErr != nil {
		return false, f.setErr
	}
	f.setStates = append(f.setStates, s)
	if f.stateByID == nil {
		f.stateByID = map[types.ID]EnforcementState{}
	}
	f.stateByID[id] = s
	return !f.setGuardMiss, nil
}

type fakeSlasher struct {
	slashed []string
	err     error
}

func (f *fakeSlasher) Slash(_ context.Context, subject string) error {
	if f.err != nil {
		return f.err
	}
	f.slashed = append(f.slashed, subject)
	return nil
}

type fakeCredit struct {
	makeEntryAdj     bool
	unmaturedPerCall int

	entryErr     error
	unmaturedErr error
	racErr       error

	entryCalls     []types.ID
	entryReasons   []string
	unmaturedCalls []types.ID
	unmaturedDays  []int
	unmaturedReas  []string
	racApplied     []types.ID
}

func (f *fakeCredit) ClawbackEntryForAudit(_ context.Context, resultID, _ types.ID, reason string) (*EnforcementAdjustment, error) {
	f.entryCalls = append(f.entryCalls, resultID)
	f.entryReasons = append(f.entryReasons, reason)
	if f.entryErr != nil {
		return nil, f.entryErr
	}
	if !f.makeEntryAdj {
		return nil, nil
	}
	return &EnforcementAdjustment{ID: types.NewID(), VolunteerID: types.NewID(), LeafID: types.NewID(), Magnitude: 1.5}, nil
}

func (f *fakeCredit) ClawbackUnmaturedForAudit(_ context.Context, volID, _ types.ID, days int, reason string) ([]*EnforcementAdjustment, error) {
	f.unmaturedCalls = append(f.unmaturedCalls, volID)
	f.unmaturedDays = append(f.unmaturedDays, days)
	f.unmaturedReas = append(f.unmaturedReas, reason)
	if f.unmaturedErr != nil {
		return nil, f.unmaturedErr
	}
	var out []*EnforcementAdjustment
	for i := 0; i < f.unmaturedPerCall; i++ {
		out = append(out, &EnforcementAdjustment{ID: types.NewID(), VolunteerID: volID, LeafID: types.NewID(), Magnitude: 0.5})
	}
	return out, nil
}

func (f *fakeCredit) ApplyRACAdjustment(_ context.Context, adjID types.ID) (bool, error) {
	if f.racErr != nil {
		return false, f.racErr
	}
	f.racApplied = append(f.racApplied, adjID)
	return true, nil
}

type fakeRevocations struct {
	emitted []types.ID
	err     error
}

func (f *fakeRevocations) EmitForAdjustment(_ context.Context, adjID types.ID) error {
	f.emitted = append(f.emitted, adjID)
	return f.err
}

type fakeResults struct {
	fraud    []FraudResult
	fraudErr error
	flipErr  error
	flipped  [][]types.ID
}

func (f *fakeResults) LoadFraudSet(context.Context, types.ID) ([]FraudResult, error) {
	return f.fraud, f.fraudErr
}

func (f *fakeResults) FlipToDisagreed(_ context.Context, ids []types.ID) error {
	f.flipped = append(f.flipped, ids)
	return f.flipErr
}

type fakeRepairer struct {
	report RepairReport
	err    error
	calls  []RepairRequest
}

func (f *fakeRepairer) RepairUnit(_ context.Context, req RepairRequest) (RepairReport, error) {
	f.calls = append(f.calls, req)
	return f.report, f.err
}

type fakeDisposer struct {
	calls []types.ID
	err   error
}

func (f *fakeDisposer) DemoteAndRequeue(_ context.Context, wuID types.ID) error {
	f.calls = append(f.calls, wuID)
	return f.err
}

// fakeLocker runs fn inline (the lock is a best-effort belt; correctness rests on the guards).
type fakeLocker struct{}

func (fakeLocker) WithUnitLock(_ context.Context, _ types.ID, fn func() error) error { return fn() }

// logCapture records emitted log records so a test can assert "the summary WARN fires once".
type logCapture struct {
	mu   sync.Mutex
	recs []capturedLog
}

type capturedLog struct {
	level slog.Level
	msg   string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, capturedLog{r.Level, r.Message})
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }
func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if r.msg == msg {
			n++
		}
	}
	return n
}

// --- deps harness -----------------------------------------------------------------------

type enfFakes struct {
	audits    *enfAuditsFake
	slash     *fakeSlasher
	credit    *fakeCredit
	revoke    *fakeRevocations
	results   *fakeResults
	repair    *fakeRepairer
	dispose   *fakeDisposer
	log       *logCapture
	agreement AgreementFunc
}

func newEnfFakes() *enfFakes {
	return &enfFakes{
		audits:  &enfAuditsFake{fakeAuditsRepo: &fakeAuditsRepo{}},
		slash:   &fakeSlasher{},
		credit:  &fakeCredit{},
		revoke:  &fakeRevocations{},
		results: &fakeResults{},
		// Default: nothing was repaired but the AGREED set survives, so disposition does
		// NOT fire unless a test opts in.
		repair:    &fakeRepairer{report: RepairReport{AgreedAfter: 1}},
		dispose:   &fakeDisposer{},
		log:       &logCapture{},
		agreement: func(ComparisonSnapshot, []byte, []byte) (bool, error) { return true, nil },
	}
}

func (f *enfFakes) worker() *EnforcementWorker {
	return NewEnforcementWorker(EnforcementDeps{
		Audits:         f.audits,
		Slasher:        f.slash,
		Credit:         f.credit,
		Revocations:    f.revoke,
		Results:        f.results,
		Repairer:       f.repair,
		Disposer:       f.dispose,
		Locker:         fakeLocker{},
		Agreement:      f.agreement,
		MaturationDays: 10,
		Logger:         slog.New(f.log),
	})
}

func vptr(v Verdict) *Verdict { return &v }

// mismatchRoot builds an eligible MISMATCH ORIGINAL in the given enforcement state.
func mismatchRoot(state EnforcementState) *Audit {
	runner := types.NewID()
	return &Audit{
		ID:                  types.NewID(),
		WorkUnitID:          types.NewID(),
		LeafID:              types.NewID(),
		ClaimedBy:           &runner,
		Verdict:             vptr(VerdictMismatch),
		EnforcementEligible: true,
		EnforcementState:    state,
	}
}

// confirmRow builds a confirmation row (ConfirmsAuditID set) with the given status/verdict.
func confirmRow(rootID types.ID, status Status, verdict *Verdict) *Audit {
	runner := types.NewID()
	return &Audit{
		ID:              types.NewID(),
		ConfirmsAuditID: &rootID,
		ClaimedBy:       &runner,
		Status:          status,
		Verdict:         verdict,
	}
}

// twoResultFraudSet is a fraud set with two DISTINCT subjects and accounts.
func twoResultFraudSet() []FraudResult {
	return []FraudResult{
		{ResultID: types.NewID(), VolunteerID: types.NewID(), Subject: "subject-a"},
		{ResultID: types.NewID(), VolunteerID: types.NewID(), Subject: "subject-b"},
	}
}

const enforcedWarn = "audit enforcement ENFORCED"

// --- decision-table tests ---------------------------------------------------------------

// A confirmation that reproduced the ACCEPTED output (MATCH) → CONTRADICTED, zero
// consequences: a single runner's MISMATCH must never slash under Q1-B.
func TestEnforce_ConfirmationMatch_Contradicted(t *testing.T) {
	f := newEnfFakes()
	root := mismatchRoot(EnforcementAwaitingConfirmation)
	f.audits.confirms = []*Audit{confirmRow(root.ID, StatusCompleted, vptr(VerdictMatch))}

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}
	if got := f.audits.stateByID[root.ID]; got != EnforcementContradicted {
		t.Errorf("state = %q, want CONTRADICTED", got)
	}
	assertNoConsequences(t, f)
}

// Two independently-refuting runners whose ground truths AGREE → the full consequence pass.
func TestEnforce_AgreeingDoubleMismatch_FullPass(t *testing.T) {
	f := newEnfFakes()
	f.credit.makeEntryAdj = true
	f.credit.unmaturedPerCall = 1
	f.results.fraud = twoResultFraudSet()

	root := mismatchRoot(EnforcementAwaitingConfirmation)
	confirm := confirmRow(root.ID, StatusCompleted, vptr(VerdictMismatch))
	f.audits.confirms = []*Audit{confirm}
	f.audits.outputs = map[types.ID][]byte{root.ID: []byte("gt"), confirm.ID: []byte("gt")}

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}

	// Slash both subjects.
	if len(f.slash.slashed) != 2 {
		t.Errorf("slashed = %v, want 2 subjects", f.slash.slashed)
	}
	// Clawback: one per fraud result + one unmatured sweep per account.
	if len(f.credit.entryCalls) != 2 {
		t.Errorf("entry clawbacks = %d, want 2", len(f.credit.entryCalls))
	}
	for _, r := range f.credit.entryReasons {
		if r != ReasonAuditMismatch {
			t.Errorf("entry reason = %q, want %q", r, ReasonAuditMismatch)
		}
	}
	if len(f.credit.unmaturedCalls) != 2 {
		t.Errorf("unmatured clawbacks = %d, want 2 (per account)", len(f.credit.unmaturedCalls))
	}
	for _, r := range f.credit.unmaturedReas {
		if r != ReasonAuditMismatchUnmatured {
			t.Errorf("unmatured reason = %q, want %q", r, ReasonAuditMismatchUnmatured)
		}
	}
	for _, d := range f.credit.unmaturedDays {
		if d != 10 {
			t.Errorf("unmatured maturationDays = %d, want 10 (threaded from deps)", d)
		}
	}
	// 2 entry + 2 unmatured = 4 adjustments; each gets a revocation + a RAC decrement.
	if len(f.revoke.emitted) != 4 {
		t.Errorf("revocations emitted = %d, want 4", len(f.revoke.emitted))
	}
	if len(f.credit.racApplied) != 4 {
		t.Errorf("RAC applied = %d, want 4", len(f.credit.racApplied))
	}
	// Fraud flip: both results in one call.
	if len(f.results.flipped) != 1 || len(f.results.flipped[0]) != 2 {
		t.Errorf("flipped = %v, want one flip of 2 results", f.results.flipped)
	}
	// Repair invoked with the root's ids + both ground truths.
	if len(f.repair.calls) != 1 {
		t.Fatalf("repair calls = %d, want 1", len(f.repair.calls))
	}
	if req := f.repair.calls[0]; req.RootAuditID != root.ID || req.WorkUnitID != root.WorkUnitID || len(req.GroundTruths) != 2 {
		t.Errorf("repair req = %+v, want root ids + 2 ground truths", req)
	}
	// AgreedAfter > 0 → no disposition.
	if len(f.dispose.calls) != 0 {
		t.Errorf("disposition fired (%v) with a non-empty AGREED set", f.dispose.calls)
	}
	// Closed ENFORCED with exactly one summary WARN.
	if got := f.audits.stateByID[root.ID]; got != EnforcementEnforced {
		t.Errorf("state = %q, want ENFORCED", got)
	}
	if n := f.log.count(enforcedWarn); n != 1 {
		t.Errorf("ENFORCED summary WARN fired %d times, want exactly 1", n)
	}
}

// Step 1 runs regardless of enforcement_state (audit H1): a NONE-state root with an agreeing
// confirmation still proceeds to consequences — the state column is bookkeeping, not the gate.
func TestEnforce_NoneStateWithAgreeingConfirmation_Proceeds(t *testing.T) {
	f := newEnfFakes()
	f.results.fraud = twoResultFraudSet()
	root := mismatchRoot(EnforcementNone)
	confirm := confirmRow(root.ID, StatusCompleted, vptr(VerdictMismatch))
	f.audits.confirms = []*Audit{confirm}
	f.audits.outputs = map[types.ID][]byte{root.ID: []byte("gt"), confirm.ID: []byte("gt")}

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}
	if got := f.audits.stateByID[root.ID]; got != EnforcementEnforced {
		t.Errorf("state = %q, want ENFORCED (step 1 is not state-gated)", got)
	}
	if len(f.slash.slashed) != 2 {
		t.Errorf("slashed = %v, want 2", f.slash.slashed)
	}
}

// Two runners that BOTH MISMATCH but whose ground truths DISAGREE → CONTRADICTED (evidence of
// non-determinism; never fabricate agreement into a slash). Also covers an Agreement error.
func TestEnforce_DisagreeingGroundTruths_Contradicted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agree AgreementFunc
	}{
		{"disagree", func(ComparisonSnapshot, []byte, []byte) (bool, error) { return false, nil }},
		{"agreement error", func(ComparisonSnapshot, []byte, []byte) (bool, error) {
			return false, errors.New("compare boom")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnfFakes()
			f.agreement = tc.agree
			f.results.fraud = twoResultFraudSet()
			root := mismatchRoot(EnforcementAwaitingConfirmation)
			confirm := confirmRow(root.ID, StatusCompleted, vptr(VerdictMismatch))
			f.audits.confirms = []*Audit{confirm}
			f.audits.outputs = map[types.ID][]byte{root.ID: []byte("a"), confirm.ID: []byte("b")}

			if err := f.worker().enforceRoot(context.Background(), root); err != nil {
				t.Fatalf("enforceRoot: %v", err)
			}
			if got := f.audits.stateByID[root.ID]; got != EnforcementContradicted {
				t.Errorf("state = %q, want CONTRADICTED", got)
			}
			assertNoConsequences(t, f)
		})
	}
}

// INCONCLUSIVE/EXPIRED confirmations re-enqueue a fresh runner below the cap, then STALL at it.
func TestEnforce_InconclusiveExpiredCycle_ReenqueueThenStall(t *testing.T) {
	for _, latest := range []struct {
		name    string
		status  Status
		verdict *Verdict
	}{
		{"inconclusive", StatusCompleted, vptr(VerdictInconclusive)},
		{"expired", StatusExpired, nil},
	} {
		t.Run(latest.name+"/below cap re-enqueues", func(t *testing.T) {
			f := newEnfFakes()
			root := mismatchRoot(EnforcementAwaitingConfirmation)
			f.audits.confirms = []*Audit{confirmRow(root.ID, latest.status, latest.verdict)} // count 1 < 3
			if err := f.worker().enforceRoot(context.Background(), root); err != nil {
				t.Fatalf("enforceRoot: %v", err)
			}
			if f.audits.enqueueCalls != 1 {
				t.Errorf("enqueue calls = %d, want 1 (fresh confirmation)", f.audits.enqueueCalls)
			}
			if got := f.audits.stateByID[root.ID]; got != EnforcementAwaitingConfirmation {
				t.Errorf("state = %q, want AWAITING_CONFIRMATION", got)
			}
			assertNoConsequences(t, f)
		})

		t.Run(latest.name+"/at cap stalls", func(t *testing.T) {
			f := newEnfFakes()
			root := mismatchRoot(EnforcementAwaitingConfirmation)
			// MaxConfirmationAttempts rows, newest is INCONCLUSIVE/EXPIRED.
			var confirms []*Audit
			for i := 0; i < MaxConfirmationAttempts; i++ {
				confirms = append(confirms, confirmRow(root.ID, latest.status, latest.verdict))
			}
			f.audits.confirms = confirms
			if err := f.worker().enforceRoot(context.Background(), root); err != nil {
				t.Fatalf("enforceRoot: %v", err)
			}
			if f.audits.enqueueCalls != 0 {
				t.Errorf("enqueue calls = %d, want 0 at the cap", f.audits.enqueueCalls)
			}
			if got := f.audits.stateByID[root.ID]; got != EnforcementStalled {
				t.Errorf("state = %q, want STALLED", got)
			}
			assertNoConsequences(t, f)
		})
	}
}

// H1 regression: a NONE-state eligible MISMATCH root with ZERO confirmations never reaches
// consequences — it only enqueues a confirmation and awaits.
func TestEnforce_NoneStateZeroConfirmations_OnlyEnqueues(t *testing.T) {
	f := newEnfFakes()
	root := mismatchRoot(EnforcementNone)
	f.audits.confirms = nil // no confirmation rows

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}
	if f.audits.enqueueCalls != 1 {
		t.Errorf("enqueue calls = %d, want 1", f.audits.enqueueCalls)
	}
	if got := f.audits.stateByID[root.ID]; got != EnforcementAwaitingConfirmation {
		t.Errorf("state = %q, want AWAITING_CONFIRMATION", got)
	}
	assertNoConsequences(t, f)
}

// A QUEUED/CLAIMED confirmation means the second verdict is still in flight → wait, no change.
func TestEnforce_InFlightConfirmation_Waits(t *testing.T) {
	for _, st := range []Status{StatusQueued, StatusClaimed} {
		t.Run(string(st), func(t *testing.T) {
			f := newEnfFakes()
			root := mismatchRoot(EnforcementAwaitingConfirmation)
			f.audits.confirms = []*Audit{confirmRow(root.ID, st, nil)}
			if err := f.worker().enforceRoot(context.Background(), root); err != nil {
				t.Fatalf("enforceRoot: %v", err)
			}
			if len(f.audits.setStates) != 0 {
				t.Errorf("state was changed to %v while waiting", f.audits.setStates)
			}
			if f.audits.enqueueCalls != 0 {
				t.Errorf("enqueue calls = %d, want 0 while waiting", f.audits.enqueueCalls)
			}
			assertNoConsequences(t, f)
		})
	}
}

// Defensive: a confirmation row (ConfirmsAuditID set) is never an enforcement root — the pass
// refuses it without even resolving confirmations.
func TestEnforce_ConfirmationRowRejectedDefensively(t *testing.T) {
	f := newEnfFakes()
	parent := types.NewID()
	root := confirmRow(parent, StatusCompleted, vptr(VerdictMismatch))
	root.WorkUnitID = types.NewID()
	root.EnforcementEligible = true

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}
	if f.audits.confirmsCalls != 0 {
		t.Errorf("resolved confirmations for a confirmation row (calls=%d)", f.audits.confirmsCalls)
	}
	assertNoConsequences(t, f)
	if len(f.audits.setStates) != 0 {
		t.Errorf("confirmation-row root mutated state: %v", f.audits.setStates)
	}
}

// A step error before the ENFORCED close returns an error and leaves the state unchanged
// (the next sweep re-runs the whole pass).
func TestEnforce_StepError_StateUnchanged(t *testing.T) {
	f := newEnfFakes()
	f.slash.err = errors.New("slash boom")
	f.results.fraud = twoResultFraudSet()
	root := mismatchRoot(EnforcementAwaitingConfirmation)
	confirm := confirmRow(root.ID, StatusCompleted, vptr(VerdictMismatch))
	f.audits.confirms = []*Audit{confirm}
	f.audits.outputs = map[types.ID][]byte{root.ID: []byte("gt"), confirm.ID: []byte("gt")}

	err := f.worker().enforceRoot(context.Background(), root)
	if err == nil {
		t.Fatal("expected an error from the failed slash step")
	}
	// No ENFORCED transition was attempted.
	for _, s := range f.audits.setStates {
		if s == EnforcementEnforced {
			t.Errorf("state advanced to ENFORCED despite a step error")
		}
	}
	if f.log.count(enforcedWarn) != 0 {
		t.Errorf("summary WARN fired despite a step error")
	}
}

// Q2-C disposition: when NO result matched ground truth (post-repair AGREED set empty), the
// unit is demoted + requeued.
func TestEnforce_EmptyAgreedSet_DemotesAndRequeues(t *testing.T) {
	f := newEnfFakes()
	f.repair.report = RepairReport{AgreedAfter: 0}
	f.results.fraud = twoResultFraudSet()
	root := mismatchRoot(EnforcementAwaitingConfirmation)
	confirm := confirmRow(root.ID, StatusCompleted, vptr(VerdictMismatch))
	f.audits.confirms = []*Audit{confirm}
	f.audits.outputs = map[types.ID][]byte{root.ID: []byte("gt"), confirm.ID: []byte("gt")}

	if err := f.worker().enforceRoot(context.Background(), root); err != nil {
		t.Fatalf("enforceRoot: %v", err)
	}
	if len(f.dispose.calls) != 1 || f.dispose.calls[0] != root.WorkUnitID {
		t.Errorf("disposition calls = %v, want [%v]", f.dispose.calls, root.WorkUnitID)
	}
	if got := f.audits.stateByID[root.ID]; got != EnforcementEnforced {
		t.Errorf("state = %q, want ENFORCED", got)
	}
}

// assertNoConsequences fails if any sanction/repair/disposition effect fired.
func assertNoConsequences(t *testing.T, f *enfFakes) {
	t.Helper()
	if len(f.slash.slashed) != 0 {
		t.Errorf("slashed %v, want none", f.slash.slashed)
	}
	if len(f.credit.entryCalls) != 0 || len(f.credit.unmaturedCalls) != 0 {
		t.Errorf("clawbacks fired (entry=%v unmatured=%v), want none", f.credit.entryCalls, f.credit.unmaturedCalls)
	}
	if len(f.revoke.emitted) != 0 || len(f.credit.racApplied) != 0 {
		t.Errorf("revocations/RAC fired, want none")
	}
	if len(f.results.flipped) != 0 {
		t.Errorf("flips fired %v, want none", f.results.flipped)
	}
	if len(f.repair.calls) != 0 {
		t.Errorf("repair fired, want none")
	}
	if len(f.dispose.calls) != 0 {
		t.Errorf("disposition fired, want none")
	}
}
