package validation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Ordering harness -------------------------------------------------------------------------
//
// These tests prove the design §4.1 split WITHOUT a database: a fake FinalizationTxRunner records
// the money-bearing tx phase (marks -> flip -> credit inside the closure) and the commit, while
// recording repo decorators and a recording slog handler record the post-commit effects (RAC,
// attestations, the suppression WARN). The single shared eventLog captures a total order, so a
// test can assert that every tx-phase write precedes the commit and every post-commit effect
// follows it.

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	e.ev = append(e.ev, s)
	e.mu.Unlock()
}

func (e *eventLog) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ev...)
}

func indexOf(events []string, s string) int {
	for i, e := range events {
		if e == s {
			return i
		}
	}
	return -1
}

// mustBefore asserts event a occurs strictly before event b in the recorded order (both present).
func mustBefore(t *testing.T, events []string, a, b string) {
	t.Helper()
	ia, ib := indexOf(events, a), indexOf(events, b)
	if ia < 0 {
		t.Fatalf("event %q not recorded: %v", a, events)
	}
	if ib < 0 {
		t.Fatalf("event %q not recorded: %v", b, events)
	}
	if ia >= ib {
		t.Fatalf("expected %q (idx %d) before %q (idx %d): %v", a, ia, b, ib, events)
	}
}

// recSlogHandler funnels every log message into the shared eventLog as "log:<message>".
type recSlogHandler struct{ log *eventLog }

func (h recSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.log.add("log:" + r.Message)
	return nil
}
func (h recSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recSlogHandler) WithGroup(string) slog.Handler      { return h }

// recordingRunner is a fake FinalizationTxRunner: it records begin/commit/rollback around the
// closure and hands it the pre-wired stores (which may themselves be recording decorators).
type recordingRunner struct {
	log    *eventLog
	stores FinalizationStores
}

func (r *recordingRunner) RunFinalization(_ context.Context, _ types.ID, _ int, fn func(FinalizationStores) error) error {
	r.log.add("tx:begin")
	if err := fn(r.stores); err != nil {
		r.log.add("tx:rollback")
		return err
	}
	r.log.add("tx:commit")
	return nil
}

// staleRunner aborts BEFORE calling the closure, exactly as the production runner does when its
// in-tx PENDING recheck disagrees with the snapshot (review #2a). It proves acceptResults
// propagates ErrStaleSnapshot unwrapped-compatibly and writes nothing.
type staleRunner struct{ log *eventLog }

func (r staleRunner) RunFinalization(_ context.Context, unitID types.ID, _ int, _ func(FinalizationStores) error) error {
	if r.log != nil {
		r.log.add("tx:stale-abort")
	}
	return fmt.Errorf("%w: unit %s", transition.ErrStaleSnapshot, unitID)
}

// --- Recording repo decorators (tx-phase writes) ---

type recResultRepo struct {
	result.Repository
	log *eventLog
}

func (r recResultRepo) BatchUpdateValidationStatus(ctx context.Context, ids []types.ID, status result.ValidationStatus) error {
	r.log.add("tx:mark:" + string(status))
	return r.Repository.BatchUpdateValidationStatus(ctx, ids, status)
}

type recWorkUnitRepo struct {
	workunit.WorkUnitRepository
	log *eventLog
}

func (r recWorkUnitRepo) UpdateState(ctx context.Context, id types.ID, from, to workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	r.log.add("tx:flip:" + string(to))
	return r.WorkUnitRepository.UpdateState(ctx, id, from, to)
}

func (r recWorkUnitRepo) Reassign(ctx context.Context, id types.ID) (*workunit.WorkUnit, bool, error) {
	r.log.add("tx:reassign")
	return r.WorkUnitRepository.Reassign(ctx, id)
}

type recCreditRepo struct {
	credit.Repository
	log *eventLog
}

func (r recCreditRepo) Create(ctx context.Context, entry *credit.LedgerEntry) error {
	r.log.add("tx:credit")
	return r.Repository.Create(ctx, entry)
}

// --- Recording repo decorators (post-commit effects) ---

type recRACRepo struct {
	credit.RACRepository
	log *eventLog
}

func (r recRACRepo) Upsert(ctx context.Context, volunteerID, leafID types.ID, amount float64) error {
	r.log.add("post:rac")
	return r.RACRepository.Upsert(ctx, volunteerID, leafID, amount)
}

type recAttestationRepo struct {
	attestation.Creator
	log *eventLog
}

func (r recAttestationRepo) Create(ctx context.Context, att *attestation.Attestation) error {
	r.log.add("post:attest")
	return r.Creator.Create(ctx, att)
}

// failReassignWURepo fails Reassign so the reject closure returns an error, exercising the
// requeue-inside-the-tx path (a Reassign error now aborts the whole reject — design §4.1).
type failReassignWURepo struct {
	*mockWorkUnitRepo
	err error
}

func (m *failReassignWURepo) Reassign(_ context.Context, _ types.ID) (*workunit.WorkUnit, bool, error) {
	return nil, false, m.err
}

// TestFinalization_AcceptTxThenPostCommitOrdering proves the accept split: marks -> flip -> credit
// commit inside the finalization closure; RAC upserts and attestations run only after it returns.
func TestFinalization_AcceptTxThenPostCommitOrdering(t *testing.T) {
	log := &eventLog{}
	logger := slog.New(recSlogHandler{log})

	leafID := types.NewID()
	wuID := types.NewID()
	volA, volB, volC := types.NewID(), types.NewID(), types.NewID()

	// target 3 / quorum 2: A,B agree (AGREED x2), C disagrees (DISAGREED x1).
	proj := makeLeaf(leafID, 3, 0.66, "EXACT", nil, 1.0)
	proj.ValidationConfig.MinQuorum = 2
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	rA := makeResult(wuID, volA, inlineAgreeCk, inlineAgreeData)
	rB := makeResult(wuID, volB, inlineAgreeCk, inlineAgreeData)
	rC := makeResult(wuID, volC, inlineDisagreeCk, inlineDisagreeData)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(rA)
	resultRepo.addResult(rB)
	resultRepo.addResult(rC)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	creditRepo := newMockCreditRepo()
	racRepo := newMockRACRepo()

	volRepo := newMockVolunteerRepo()
	for i, v := range []types.ID{volA, volB, volC} {
		vol := makeVolunteer(v)
		vol.PublicKey = make([]byte, ed25519.PublicKeySize)
		vol.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(vol)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	runner := &recordingRunner{
		log: log,
		stores: FinalizationStores{
			Results:   recResultRepo{resultRepo, log},
			WorkUnits: recWorkUnitRepo{wuRepo, log},
			Credits:   recCreditRepo{creditRepo, log},
		},
	}

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, recRACRepo{racRepo, log}, volRepo, newMockAssignmentRepo(), recAttestationRepo{attRepo, log}, nil, signer, logger, nil, transition.TrustPolicy{}).
		WithTxRunner(runner)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	ev := log.all()
	// Tx phase, in order.
	mustBefore(t, ev, "tx:begin", "tx:mark:AGREED")
	mustBefore(t, ev, "tx:mark:AGREED", "tx:flip:VALIDATED")
	mustBefore(t, ev, "tx:mark:DISAGREED", "tx:flip:VALIDATED")
	mustBefore(t, ev, "tx:flip:VALIDATED", "tx:credit")
	mustBefore(t, ev, "tx:credit", "tx:commit")
	// Post-commit effects follow the commit.
	mustBefore(t, ev, "tx:commit", "post:rac")
	mustBefore(t, ev, "tx:commit", "post:attest")

	if len(racRepo.upserts) != 2 {
		t.Errorf("RAC upserts = %d, want 2 (one per granted result)", len(racRepo.upserts))
	}
	if len(attRepo.attestations) != 3 {
		t.Errorf("attestations = %d, want 3 (2 AGREED + 1 DISAGREED)", len(attRepo.attestations))
	}
}

// TestFinalization_CapSuppression_WarnAfterCommit proves the suppression contract: CreateCapped
// returning (false, nil) leaves the result AGREED with no ledger row and no error, and the
// suppression WARN fires AFTER the commit (relocated out of the tx phase, §4.1).
func TestFinalization_CapSuppression_WarnAfterCommit(t *testing.T) {
	const leafCredit = 2.0
	const cap = 10.0

	log := &eventLog{}
	logger := slog.New(recSlogHandler{log})

	leafID := types.NewID()
	wuID := types.NewID()
	volGranted, volSuppressed := types.NewID(), types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, leafCredit)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	// Insertion order fixes agreedResults = [rGranted, rSuppressed].
	rGranted := makeResult(wuID, volGranted, inlineAgreeCk, inlineAgreeData)
	rSuppressed := makeResult(wuID, volSuppressed, inlineAgreeCk, inlineAgreeData)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(rGranted)
	resultRepo.addResult(rSuppressed)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(volGranted))
	volRepo.addVolunteer(makeVolunteer(volSuppressed))

	capRepo := newCappingCreditRepo()
	capRepo.suppress[rSuppressed.ID] = true

	// The runner hands the TX-SCOPED credits repo (the capping fake) to the closure directly, so
	// cappedCreatorFor resolves CreateCapped against it — no recording wrapper (which would hide
	// the CappedCreator capability behind the plain credit.Repository interface).
	runner := &recordingRunner{
		log: log,
		stores: FinalizationStores{
			Results:   recResultRepo{resultRepo, log},
			WorkUnits: recWorkUnitRepo{wuRepo, log},
			Credits:   capRepo,
		},
	}

	engine := NewEngine(resultRepo, wuRepo, leafRepo, capRepo, nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, logger, nil, transition.TrustPolicy{}).
		WithEmissionCap(cap).
		WithTxRunner(runner)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v (suppression must be a non-error branch)", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2 (both stay AGREED)", len(vr.AgreedResults))
	}
	if rSuppressed.ValidationStatus != result.ValidationAgreed {
		t.Errorf("suppressed result status = %s, want AGREED", rSuppressed.ValidationStatus)
	}
	if len(vr.CreditEntries) != 1 || vr.CreditEntries[0].ResultID != rGranted.ID {
		t.Errorf("CreditEntries = %+v, want exactly the granted result %v", vr.CreditEntries, rGranted.ID)
	}
	if _, granted := capRepo.byRes[rSuppressed.ID]; granted {
		t.Error("suppressed result must have NO ledger row")
	}
	// The suppression WARN is emitted post-commit.
	mustBefore(t, log.all(), "tx:commit", "log:credit suppressed by daily emission cap")
}

// TestFinalization_StaleSnapshot_PropagatesErrorsIs proves a runner abort on the in-tx recheck
// surfaces as transition.ErrStaleSnapshot (errors.Is-compatible) and writes nothing — the
// transitioner keys its single retry on exactly this.
func TestFinalization_StaleSnapshot_PropagatesErrorsIs(t *testing.T) {
	log := &eventLog{}

	leafID := types.NewID()
	wuID := types.NewID()
	volA, volB := types.NewID(), types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	rA := makeResult(wuID, volA, inlineAgreeCk, inlineAgreeData)
	rB := makeResult(wuID, volB, inlineAgreeCk, inlineAgreeData)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(rA)
	resultRepo.addResult(rB)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(volA))
	volRepo.addVolunteer(makeVolunteer(volB))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{}).
		WithTxRunner(staleRunner{log: log})

	pending := []*result.Result{rA, rB}
	verdict := transition.BuildComparisonVerdict(pending, pending, 0)
	policy := transition.ResolvePolicyWithTrust(proj, wu, transition.TrustPolicy{})

	err := engine.ApplyAccept(context.Background(), wu, proj, pending, pending, verdict, policy, len(pending))
	if !errors.Is(err, transition.ErrStaleSnapshot) {
		t.Fatalf("ApplyAccept error = %v, want errors.Is(ErrStaleSnapshot)", err)
	}
	// Nothing was written: fn was never called.
	if wu.State != workunit.WorkUnitStateCompleted {
		t.Errorf("unit state = %s, want COMPLETED (unchanged — recheck aborted before fn)", wu.State)
	}
	if len(resultRepo.updated) != 0 {
		t.Errorf("result status writes = %d, want 0 (recheck aborted before fn)", len(resultRepo.updated))
	}
	if idx := indexOf(log.all(), "tx:stale-abort"); idx < 0 {
		t.Error("expected the runner to abort on the recheck")
	}
}

// TestFinalization_RejectReassignFailure_PropagatesAndSkipsPostCommit proves the reject split:
// Reassign runs inside the closure, and a Reassign error propagates out (was log-and-continue),
// so the post-commit effects (attestations, rejected-counter bumps) are skipped.
func TestFinalization_RejectReassignFailure_PropagatesAndSkipsPostCommit(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	volA, volB := types.NewID(), types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	rA := makeResult(wuID, volA, inlineAgreeCk, inlineAgreeData)
	rB := makeResult(wuID, volB, inlineDisagreeCk, inlineDisagreeData) // disagree -> rejectAll

	resultRepo := newMockResultRepo()
	resultRepo.addResult(rA)
	resultRepo.addResult(rB)

	baseWU := newMockWorkUnitRepo()
	baseWU.addWorkUnit(wu)
	failWU := &failReassignWURepo{mockWorkUnitRepo: baseWU, err: errors.New("reassign boom")}

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	for i, v := range []types.ID{volA, volB} {
		vol := makeVolunteer(v)
		vol.PublicKey = make([]byte, ed25519.PublicKeySize)
		vol.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(vol)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	// Passthrough runner (no WithTxRunner): the closure runs over the engine's own repos, so the
	// failing Reassign surfaces directly. (Rollback of the marks/flip is the integration test's
	// job — a mock has no transaction — here we assert error propagation and post-commit skip.)
	engine := NewEngine(resultRepo, failWU, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), attRepo, nil, signer, testLogger(), nil, transition.TrustPolicy{})

	_, err := engine.TryValidate(context.Background(), wuID)
	if err == nil {
		t.Fatal("expected a reassign error to propagate out of TryValidate")
	}
	if !errors.Is(err, failWU.err) {
		t.Fatalf("error = %v, want it to wrap the Reassign error", err)
	}
	// Post-commit effects skipped because the closure errored.
	if len(attRepo.attestations) != 0 {
		t.Errorf("attestations = %d, want 0 (post-commit skipped on tx failure)", len(attRepo.attestations))
	}
	if len(volRepo.rejectedInc) != 0 {
		t.Errorf("rejected-counter bumps = %d, want 0 (post-commit skipped on tx failure)", len(volRepo.rejectedInc))
	}
}
