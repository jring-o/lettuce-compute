package validation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Repair-specific fakes (extend the engine_test.go fixtures the package already ships) ---

// conflictCreditRepo makes the shared mockCreditRepo return the TYPED apierror.Conflict on a
// duplicate result — the real repo's uq_credit_ledger_result behavior (23505 -> 409), which the
// repair grant treats as "already granted" (a no-op), not a hard error. The bare mockCreditRepo
// returns a plain error on duplicate, which the repair path (correctly) would abort on.
type conflictCreditRepo struct {
	*mockCreditRepo
}

func newConflictCreditRepo() *conflictCreditRepo {
	return &conflictCreditRepo{mockCreditRepo: newMockCreditRepo()}
}

func (m *conflictCreditRepo) Create(ctx context.Context, entry *credit.LedgerEntry) error {
	if _, exists := m.byRes[entry.ResultID]; exists {
		return apierror.Conflict("credit already granted for this result", nil)
	}
	return m.mockCreditRepo.Create(ctx, entry)
}

var _ credit.Repository = (*conflictCreditRepo)(nil)

// repairAttRepo enforces one AGREED attestation per result (the uq_attestations_result_agreed
// partial index), returning apierror.Conflict on a second AGREED for the same result so a repair
// re-run mints nothing new. createAttestations swallows the error, so the row count stays put.
type repairAttRepo struct {
	atts        []*attestation.Attestation
	agreedByRes map[types.ID]bool
}

func newRepairAttRepo() *repairAttRepo {
	return &repairAttRepo{agreedByRes: make(map[types.ID]bool)}
}

func (m *repairAttRepo) Create(_ context.Context, att *attestation.Attestation) error {
	if att.ValidationOutcome == attestation.OutcomeAgreed && att.ResultID != nil {
		if m.agreedByRes[*att.ResultID] {
			return apierror.Conflict("attestation already exists",
				map[string]string{"constraint": "uq_attestations_result_agreed"})
		}
		m.agreedByRes[*att.ResultID] = true
	}
	att.ID = types.NewID()
	m.atts = append(m.atts, att)
	return nil
}

func (m *repairAttRepo) agreedFor(resultID types.ID) *attestation.Attestation {
	for _, a := range m.atts {
		if a.ValidationOutcome == attestation.OutcomeAgreed && a.ResultID != nil && *a.ResultID == resultID {
			return a
		}
	}
	return nil
}

func (m *repairAttRepo) countAgreedFor(resultID types.ID) int {
	n := 0
	for _, a := range m.atts {
		if a.ValidationOutcome == attestation.OutcomeAgreed && a.ResultID != nil && *a.ResultID == resultID {
			n++
		}
	}
	return n
}

var _ attestation.Creator = (*repairAttRepo)(nil)

// fakeRepairClaimer models the audit_repairs UNIQUE(result_id) claim: the FIRST claim for a
// result wins (claimed=true), every later claim loses (claimed=false). A forceErr surfaces an
// infrastructure failure so a test can assert the pass aborts.
type fakeRepairClaimer struct {
	claimed  map[types.ID]bool
	calls    []types.ID
	forceErr error
}

func newFakeRepairClaimer() *fakeRepairClaimer {
	return &fakeRepairClaimer{claimed: make(map[types.ID]bool)}
}

func (f *fakeRepairClaimer) ClaimRepair(_ context.Context, _ types.ID, resultID types.ID) (bool, error) {
	f.calls = append(f.calls, resultID)
	if f.forceErr != nil {
		return false, f.forceErr
	}
	if f.claimed[resultID] {
		return false, nil
	}
	f.claimed[resultID] = true
	return true, nil
}

// --- Test helpers ---

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// disagreed stamps a result DISAGREED (the repair candidate state — after the enforcement pass's
// fraud flip, every result on the unit is DISAGREED). Returns r for chaining.
func disagreed(r *result.Result) *result.Result {
	r.ValidationStatus = result.ValidationDisagreed
	return r
}

type repairFixture struct {
	engine  *Engine
	wu      *workunit.WorkUnit
	proj    *leaf.Leaf
	results *mockResultRepo
	credit  *conflictCreditRepo
	rac     *mockRACRepo
	vol     *mockVolunteerRepo
	att     *repairAttRepo
	trust   *fakeTrustRepo
	rel     *fakeReliabilityRepo
	claimer *fakeRepairClaimer
	auditID types.ID
	wuID    types.ID
	leafID  types.ID
}

// newRepairFixture wires an engine with the full repair effect surface (credit, RAC, attestations
// + signer, volunteer counters, reliability, trust, and the repair claimer). Every passed result
// is repointed at the fixture's unit and given a backing volunteer with a public key.
func newRepairFixture(t *testing.T, mode string, tolerance *float64, ignoreFields []string, creditAmount float64, results ...*result.Result) *repairFixture {
	t.Helper()
	leafID := types.NewID()
	wuID := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, mode, tolerance, creditAmount)
	proj.ValidationConfig.IgnoreFields = ignoreFields
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateValidated)

	resultRepo := newMockResultRepo()
	volRepo := newMockVolunteerRepo()
	for _, r := range results {
		r.WorkUnitID = wuID
		resultRepo.addResult(r)
		if _, ok := volRepo.volunteers[r.VolunteerID]; !ok {
			v := makeVolunteer(r.VolunteerID)
			v.PublicKey = make([]byte, ed25519.PublicKeySize)
			volRepo.addVolunteer(v)
		}
	}

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newConflictCreditRepo()
	racRepo := newMockRACRepo()
	attRepo := newRepairAttRepo()
	trustRepo := newFakeTrustRepo()
	relRepo := &fakeReliabilityRepo{}
	claimer := newFakeRepairClaimer()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer := attestation.NewSigner(priv)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volRepo,
		newMockAssignmentRepo(), attRepo, relRepo, signer, testLogger(), trustRepo,
		transition.TrustPolicy{DefaultFloor: 25}).WithRepairSupport(claimer)

	return &repairFixture{
		engine: engine, wu: wu, proj: proj, results: resultRepo, credit: creditRepo,
		rac: racRepo, vol: volRepo, att: attRepo, trust: trustRepo, rel: relRepo,
		claimer: claimer, auditID: types.NewID(), wuID: wuID, leafID: leafID,
	}
}

func (f *repairFixture) snapshot() audit.ComparisonSnapshot {
	snap := audit.ComparisonSnapshot{
		ComparisonMode: f.proj.ValidationConfig.ComparisonMode,
		IgnoreFields:   f.proj.ValidationConfig.IgnoreFields,
		CompareFields:  f.proj.ValidationConfig.CompareFields,
	}
	if f.proj.ValidationConfig.NumericTolerance != nil {
		snap.NumericTolerance = *f.proj.ValidationConfig.NumericTolerance
	}
	return snap
}

func (f *repairFixture) run(groundTruths ...[]byte) (audit.RepairReport, error) {
	return f.engine.RepairUnit(context.Background(), audit.RepairRequest{
		RootAuditID:  f.auditID,
		WorkUnitID:   f.wuID,
		Snapshot:     f.snapshot(),
		GroundTruths: groundTruths,
	})
}

// seedLedger backfills a ledger entry for a result (models a clawed fraud-set entry that the
// repair reads its uniform per-unit amount from — credit_amount is immutable under adjustment).
func (f *repairFixture) seedLedger(t *testing.T, r *result.Result, amount float64) {
	t.Helper()
	err := f.credit.Create(context.Background(), &credit.LedgerEntry{
		VolunteerID: r.VolunteerID, LeafID: f.leafID, WorkUnitID: f.wuID,
		ResultID: r.ID, CreditAmount: amount,
	})
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// --- (ii) AdjudicateGroundTruthAgreement: the symmetric raw-bytes mutual ground-truth check ---

func TestAdjudicateGroundTruthAgreement(t *testing.T) {
	exact := func(ignore ...string) audit.ComparisonSnapshot {
		return audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact, IgnoreFields: ignore}
	}
	numeric := func(tol float64) audit.ComparisonSnapshot {
		return audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: tol}
	}

	cases := []struct {
		name    string
		snap    audit.ComparisonSnapshot
		a, b    []byte
		want    bool
		wantErr bool
	}{
		{"raw exact equal", exact(), []byte(`{"x":1}`), []byte(`{"x":1}`), true, false},
		{"raw exact non-json equal", exact(), []byte{0x00, 0x01, 0x02}, []byte{0x00, 0x01, 0x02}, true, false},
		{"raw exact differ", exact(), []byte(`{"x":1}`), []byte(`{"x":2}`), false, false},
		// The load-bearing symmetric-channel case: exponent-form token vs decimal-expanded token,
		// both RAW runner bytes. A key-string compare of the two canon forms would MISMATCH; the
		// value-level compare parses both to the same float64 and AGREES.
		{"canon exponent value-level", exact("ts"), []byte(`{"x":1e-07}`), []byte(`{"x":0.0000001}`), true, false},
		{"canon ignore volatile field", exact("ts"), []byte(`{"x":1,"ts":1}`), []byte(`{"x":1,"ts":999}`), true, false},
		{"canon value differ", exact("ts"), []byte(`{"x":1}`), []byte(`{"x":2}`), false, false},
		{"numeric within eps", numeric(0.01), []byte(`{"x":1.0}`), []byte(`{"x":1.005}`), true, false},
		// Exactly-representable boundary: |1.5-1.0| == 0.5 == eps, and numericMatch uses a
		// STRICT "> eps" reject, so a difference equal to eps still agrees.
		{"numeric at eps boundary", numeric(0.5), []byte(`{"x":1.0}`), []byte(`{"x":1.5}`), true, false},
		{"numeric outside eps", numeric(0.01), []byte(`{"x":1.0}`), []byte(`{"x":1.5}`), false, false},
		{"canon unparseable A -> error", exact("ts"), []byte(`not json`), []byte(`{"x":1}`), false, true},
		{"numeric unparseable B -> error", numeric(0.01), []byte(`{"x":1}`), []byte(`bad`), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AdjudicateGroundTruthAgreement(tc.snap, tc.a, tc.b)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got agree=%v nil err", got)
				}
				if got {
					t.Errorf("agree=true on an error path; must never fabricate agreement")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("agree = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- (iii) Repair adjudication: which DISAGREED candidates match ground truth ---

func TestRepairUnit_RawKeyMatch(t *testing.T) {
	gt := []byte(`{"answer":42}`)
	cand := disagreed(makeResult(types.NewID(), types.NewID(), sha256Hex(gt), gt))
	f := newRepairFixture(t, leaf.ComparisonExact, nil, nil, 1.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1", rep.Repaired)
	}
	if rep.AgreedAfter != 1 {
		t.Errorf("AgreedAfter = %d, want 1", rep.AgreedAfter)
	}
	if f.results.results[cand.ID].ValidationStatus != result.ValidationAgreed {
		t.Errorf("candidate not flipped AGREED")
	}
}

func TestRepairUnit_CanonValueMatchAcrossJsonbBoundary(t *testing.T) {
	// The F-H3 heir: the candidate's STORED output is jsonb-normalized (0.0000001) while the
	// ground-truth bytes carry the exponent token (1e-07). A canon KEY-STRING compare would
	// MISMATCH; the value-level canon compare AGREES, so the honest dissenter is repaired.
	gt := []byte(`{"x":1e-07}`)
	cand := disagreed(makeResult(types.NewID(), types.NewID(), "ignored-on-canon-path", []byte(`{"x":0.0000001}`)))
	f := newRepairFixture(t, leaf.ComparisonExact, nil, []string{"ts"}, 1.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1 (value-level canon match across the jsonb boundary)", rep.Repaired)
	}
}

func TestRepairUnit_NumericMatchesSecondGroundTruthOnly(t *testing.T) {
	tol := 0.01
	cand := disagreed(makeResult(types.NewID(), types.NewID(), "x", []byte(`{"x":1.0}`)))
	f := newRepairFixture(t, leaf.ComparisonNumericTolerance, &tol, nil, 1.0, cand)

	gt1 := []byte(`{"x":1.5}`)   // outside tolerance
	gt2 := []byte(`{"x":1.005}`) // within tolerance of the second runner only
	rep, err := f.run(gt1, gt2)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1 (within eps of the second ground truth)", rep.Repaired)
	}
}

func TestRepairUnit_UnparseableCandidateSkipped(t *testing.T) {
	gt := []byte(`{"x":1}`)
	cand := disagreed(makeResult(types.NewID(), types.NewID(), "x", []byte(`not json`)))
	// ignore_fields forces the canon path, which must PARSE the candidate to build its key.
	f := newRepairFixture(t, leaf.ComparisonExact, nil, []string{"ts"}, 1.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", rep.Repaired)
	}
	if got := rep.Skipped[cand.ID]; got != repairSkipInconclusive {
		t.Errorf("Skipped[cand] = %q, want %q", got, repairSkipInconclusive)
	}
}

func TestRepairUnit_RefOnlyExactMatchesClaimedChecksum(t *testing.T) {
	// A ref-only EXACT candidate has NO inline bytes; comparisonKey falls back to its CLAIMED
	// checksum, which adjudicates raw against the re-executed ground-truth bytes — exactly the
	// validation-time semantics (BG-02b residual until slice 5).
	gt := []byte(`{"answer":42}`)
	cand := disagreed(makeResult(types.NewID(), types.NewID(), sha256Hex(gt), nil)) // nil OutputData = ref-only
	f := newRepairFixture(t, leaf.ComparisonExact, nil, nil, 1.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1 (ref-only claimed-checksum match)", rep.Repaired)
	}
}

func TestRepairUnit_CanonEmptySkipped(t *testing.T) {
	// ignore_fields strip EVERY comparable leaf, so the candidate key is canon-empty (embeds its
	// own UUID) and is unadjudicable against runner bytes.
	gt := []byte(`{"ts":1}`)
	cand := disagreed(makeResult(types.NewID(), types.NewID(), "x", []byte(`{"ts":123}`)))
	f := newRepairFixture(t, leaf.ComparisonExact, nil, []string{"ts"}, 1.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", rep.Repaired)
	}
	if got := rep.Skipped[cand.ID]; got != repairSkipCanonEmpty {
		t.Errorf("Skipped[cand] = %q, want %q", got, repairSkipCanonEmpty)
	}
}

func TestRepairUnit_RefOnlyNumericSkipped(t *testing.T) {
	tol := 0.01
	cand := disagreed(makeResult(types.NewID(), types.NewID(), "x", nil)) // ref-only: no inline bytes to flatten
	f := newRepairFixture(t, leaf.ComparisonNumericTolerance, &tol, nil, 1.0, cand)

	rep, err := f.run([]byte(`{"x":1.0}`))
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if got := rep.Skipped[cand.ID]; got != repairSkipRefOnly {
		t.Errorf("Skipped[cand] = %q, want %q", got, repairSkipRefOnly)
	}
}

// --- (iv) Repair effects: idempotency, the standing gate, and cap suppression ---

// TestRepairUnit_EffectsIdempotentAcrossReRun runs the pass twice (a simulated crash-and-resweep):
// the honest dissenter is repaired once, and a fraudster on the same unit never matches. The grant
// amount is READ from the unit's clawed ledger entry (uniform per unit), NOT the leaf default. On
// the second pass the grant Conflicts, the attestation unique-conflicts, and — because the
// audit_repairs claim now loses — the reputational trio fires exactly once total.
func TestRepairUnit_EffectsIdempotentAcrossReRun(t *testing.T) {
	gt := []byte(`{"answer":42}`)
	honestVol := types.NewID()
	honest := disagreed(makeResult(types.NewID(), honestVol, sha256Hex(gt), gt))
	fraud := disagreed(makeResult(types.NewID(), types.NewID(), "deadbeef", []byte(`{"answer":7}`)))

	f := newRepairFixture(t, leaf.ComparisonExact, nil, nil, 1.0 /* leaf default we must NOT use */, honest, fraud)
	// The unit granted 3.0 per result in its (now clawed) fraud era; repair reads that amount.
	f.seedLedger(t, fraud, 3.0)

	rep1, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit run 1: %v", err)
	}
	if rep1.Repaired != 1 || rep1.AlreadyRepaired != 0 {
		t.Fatalf("run 1 Repaired=%d AlreadyRepaired=%d, want 1/0", rep1.Repaired, rep1.AlreadyRepaired)
	}
	if got := rep1.Skipped[fraud.ID]; got != repairSkipNoMatch {
		t.Errorf("run 1 Skipped[fraud] = %q, want %q", got, repairSkipNoMatch)
	}
	if rep1.AgreedAfter != 1 {
		t.Errorf("run 1 AgreedAfter = %d, want 1", rep1.AgreedAfter)
	}
	entry := f.credit.byRes[honest.ID]
	if entry == nil {
		t.Fatalf("run 1: honest dissenter not granted")
	}
	if entry.CreditAmount != 3.0 {
		t.Errorf("run 1 grant amount = %v, want 3.0 (read from the unit's ledger, not the leaf default 1.0)", entry.CreditAmount)
	}
	if att := f.att.agreedFor(honest.ID); att == nil || att.CreditAmount != 3.0 {
		t.Errorf("run 1 AGREED attestation amount = %+v, want credit 3.0", att)
	}

	// Simulate a re-drive that re-selects the same candidate (flip-first ordering means a fully
	// repaired result is AGREED and normally would not be re-selected; resetting it to DISAGREED
	// forces re-processing so the per-effect idempotency guards — grant Conflict, attestation
	// unique, and the audit_repairs claim — are all exercised, exactly what defends a partial
	// re-run).
	honest.ValidationStatus = result.ValidationDisagreed

	rep2, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit run 2: %v", err)
	}
	if rep2.Repaired != 0 || rep2.AlreadyRepaired != 1 {
		t.Fatalf("run 2 Repaired=%d AlreadyRepaired=%d, want 0/1", rep2.Repaired, rep2.AlreadyRepaired)
	}

	// Idempotency: exactly ONE of each non-idempotent effect after two passes.
	if f.att.countAgreedFor(honest.ID) != 1 {
		t.Errorf("AGREED attestations for honest = %d, want 1", f.att.countAgreedFor(honest.ID))
	}
	if n := len(f.credit.entries); n != 2 { // the seeded fraud entry + the single honest grant
		t.Errorf("ledger entries = %d, want 2 (seeded fraud + one honest grant)", n)
	}
	if f.vol.completedInc[honestVol] != 1 {
		t.Errorf("IncrementWorkUnitsCompleted fired %d times, want 1", f.vol.completedInc[honestVol])
	}
	good, _ := f.rel.countGood()
	if good != 1 {
		t.Errorf("reliability good outcomes = %d, want 1", good)
	}
	if f.trust.totalAccruals() != 1 {
		t.Errorf("trust accruals = %d, want 1", f.trust.totalAccruals())
	}
	honestRAC := 0
	for _, u := range f.rac.upserts {
		if u.VolunteerID == honestVol {
			honestRAC++
		}
	}
	if honestRAC != 1 {
		t.Errorf("RAC upserts for honest = %d, want 1 (only the fresh grant)", honestRAC)
	}
}

// TestRepairUnit_ProbationCandidateNoTrust: a candidate whose submission-time standing was
// PROBATION is not standing-countable, so it accrues NO trust (audit M5) — but it still gets the
// flip, grant, attestation, counter, standing, and reliability compensation (M5 semantics).
func TestRepairUnit_ProbationCandidateNoTrust(t *testing.T) {
	gt := []byte(`{"answer":42}`)
	vol := types.NewID()
	cand := disagreed(makeResult(types.NewID(), vol, sha256Hex(gt), gt))
	probation := volunteer.StandingProbation
	cand.StandingAtSubmit = &probation

	f := newRepairFixture(t, leaf.ComparisonExact, nil, nil, 2.0, cand)

	rep, err := f.run(gt)
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1", rep.Repaired)
	}
	if f.trust.totalAccruals() != 0 {
		t.Errorf("trust accruals = %d, want 0 (PROBATION is not standing-countable)", f.trust.totalAccruals())
	}
	// Everything else still fires.
	if f.credit.byRes[cand.ID] == nil {
		t.Errorf("probation candidate not granted")
	}
	if f.att.countAgreedFor(cand.ID) != 1 {
		t.Errorf("AGREED attestations = %d, want 1", f.att.countAgreedFor(cand.ID))
	}
	if f.vol.completedInc[vol] != 1 {
		t.Errorf("completed increments = %d, want 1", f.vol.completedInc[vol])
	}
	if good, _ := f.rel.countGood(); good != 1 {
		t.Errorf("reliability good = %d, want 1", good)
	}
}

// TestRepairUnit_SuppressedByCapAttestsZero: with a per-account emission cap that suppresses the
// grant, the repaired result carries NO ledger row and its AGREED attestation attests credit 0 —
// while the work-quality trio still fires (the cap bounds emission, not merit).
func TestRepairUnit_SuppressedByCapAttestsZero(t *testing.T) {
	gt := []byte(`{"answer":42}`)
	vol := types.NewID()
	cand := disagreed(makeResult(types.NewID(), vol, sha256Hex(gt), gt))

	leafID := types.NewID()
	wuID := types.NewID()
	proj := makeLeaf(leafID, 2, 1.0, leaf.ComparisonExact, nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateValidated)
	cand.WorkUnitID = wuID

	resultRepo := newMockResultRepo()
	resultRepo.addResult(cand)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	v := makeVolunteer(vol)
	v.PublicKey = make([]byte, ed25519.PublicKeySize)
	volRepo.addVolunteer(v)

	creditRepo := newCappingCreditRepo()
	creditRepo.suppress[cand.ID] = true // this repair grant is over the cap
	attRepo := newRepairAttRepo()
	trustRepo := newFakeTrustRepo()
	relRepo := &fakeReliabilityRepo{}
	claimer := newFakeRepairClaimer()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, newMockRACRepo(), volRepo,
		newMockAssignmentRepo(), attRepo, relRepo, signer, testLogger(), trustRepo,
		transition.TrustPolicy{DefaultFloor: 25}).WithEmissionCap(100).WithRepairSupport(claimer)

	rep, err := engine.RepairUnit(context.Background(), audit.RepairRequest{
		RootAuditID:  types.NewID(),
		WorkUnitID:   wuID,
		Snapshot:     audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
		GroundTruths: [][]byte{gt},
	})
	if err != nil {
		t.Fatalf("RepairUnit: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1 (a suppressed grant still repairs)", rep.Repaired)
	}
	if len(creditRepo.cappedCalls) != 1 {
		t.Errorf("CreateCapped calls = %d, want 1", len(creditRepo.cappedCalls))
	}
	if creditRepo.byRes[cand.ID] != nil {
		t.Errorf("suppressed grant must leave NO ledger row")
	}
	att := attRepo.agreedFor(cand.ID)
	if att == nil {
		t.Fatalf("no AGREED attestation for the suppressed repair")
	}
	if att.CreditAmount != 0 {
		t.Errorf("suppressed-repair attestation credit = %v, want 0", att.CreditAmount)
	}
	// Work-quality effects still fire under suppression.
	if volRepo.completedInc[vol] != 1 {
		t.Errorf("completed increments = %d, want 1", volRepo.completedInc[vol])
	}
}

// TestRepairUnit_NotWiredErrors: without a repair claimer wired, RepairUnit refuses to run rather
// than apply the non-idempotent effects unguarded.
func TestRepairUnit_NotWiredErrors(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	proj := makeLeaf(leafID, 2, 1.0, leaf.ComparisonExact, nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateValidated)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(newMockResultRepo(), wuRepo, leafRepo, newMockCreditRepo(), nil,
		newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil,
		transition.TrustPolicy{})

	_, err := engine.RepairUnit(context.Background(), audit.RepairRequest{
		WorkUnitID: wuID,
		Snapshot:   audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
	})
	if err == nil {
		t.Fatal("expected error when RepairUnit is called without a repair claimer wired")
	}
}
