package validation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// cappingCreditRepo EXTENDS the shared mockCreditRepo (engine_test.go) with the optional
// credit.CappedCreator capability so the emission-cap grant path can be exercised without a
// database. It embeds *mockCreditRepo (rather than editing that shared fixture, which other
// suites depend on) so every credit.Repository method is promoted unchanged and only
// CreateCapped is added — making *cappingCreditRepo satisfy both credit.Repository and
// credit.CappedCreator. CreateCapped suppresses (inserted=false, nil error) any result whose ID
// is in suppress; otherwise it delegates to the embedded Create and reports inserted=true. A
// non-nil createCappedErr forces the hard-error branch. cappedCalls records the result IDs
// passed, in order, so a test can assert CreateCapped was (or was not) reached.
type cappingCreditRepo struct {
	*mockCreditRepo
	suppress        map[types.ID]bool
	createCappedErr error
	cappedCalls     []types.ID
	lastCapArg      float64
}

func newCappingCreditRepo() *cappingCreditRepo {
	return &cappingCreditRepo{
		mockCreditRepo: newMockCreditRepo(),
		suppress:       make(map[types.ID]bool),
	}
}

func (m *cappingCreditRepo) CreateCapped(ctx context.Context, entry *credit.LedgerEntry, capPerDay float64) (bool, error) {
	m.cappedCalls = append(m.cappedCalls, entry.ResultID)
	m.lastCapArg = capPerDay
	if m.createCappedErr != nil {
		return false, m.createCappedErr
	}
	if m.suppress[entry.ResultID] {
		return false, nil
	}
	if err := m.mockCreditRepo.Create(ctx, entry); err != nil {
		return false, err
	}
	return true, nil
}

// compile-time proof the extended fake implements both contracts the engine relies on.
var (
	_ credit.Repository    = (*cappingCreditRepo)(nil)
	_ credit.CappedCreator = (*cappingCreditRepo)(nil)
)

// TestEmissionCap_UnsetIsInert proves the cap defaults to inert: with WithEmissionCap never
// called, the grant path takes the legacy Create branch for every agreed result and NEVER calls
// CreateCapped — even though the wired credit repo implements it.
func TestEmissionCap_UnsetIsInert(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
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

	repo := newCappingCreditRepo()

	// Note: no WithEmissionCap -> cap is the zero-value default (0), so the cap is off.
	engine := NewEngine(resultRepo, wuRepo, leafRepo, repo, nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(repo.cappedCalls) != 0 {
		t.Errorf("CreateCapped calls = %d, want 0 (cap unset must never route through the capped path)", len(repo.cappedCalls))
	}
	if len(repo.entries) != 2 {
		t.Errorf("ledger entries = %d, want 2 (Create called for every agreed result)", len(repo.entries))
	}
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2", len(vr.CreditEntries))
	}
}

// TestEmissionCap_SuppressesOneGrantButKeepsWorkEffects is the core cap test: with a cap
// configured and the repo suppressing ONE of two agreed results, the unit still VALIDATES, both
// results stay AGREED, only the granted result gets a ledger row / RAC upsert / full-amount
// attestation, the suppressed one gets a credit-0 AGREED attestation, and every
// work-quality-denominated effect (completed counter, standing fold, reliability signal, trust
// accrual) fires for BOTH results (design §5.3 F10: the cap bounds emission, not merit).
func TestEmissionCap_SuppressesOneGrantButKeepsWorkEffects(t *testing.T) {
	const leafCredit = 2.5
	const cap = 10.0

	leafID := types.NewID()
	wuID := types.NewID()
	volGranted := types.NewID()
	volSuppressed := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, leafCredit)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	// Both agree (checksum "aaaa"); insertion order fixes agreedResults = [rGranted, rSuppressed].
	rGranted := stampSubject(makeResult(wuID, volGranted, "aaaa", nil), "did:plc:granted", 30)
	rSuppressed := stampSubject(makeResult(wuID, volSuppressed, "aaaa", nil), "did:plc:suppressed", 30)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(rGranted)
	resultRepo.addResult(rSuppressed)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	pkGranted := make([]byte, ed25519.PublicKeySize)
	pkGranted[0] = 1
	pkSuppressed := make([]byte, ed25519.PublicKeySize)
	pkSuppressed[0] = 2
	volRepo := newMockVolunteerRepo()
	vG := makeVolunteer(volGranted)
	vG.PublicKey = pkGranted
	volRepo.addVolunteer(vG)
	vS := makeVolunteer(volSuppressed)
	vS.PublicKey = pkSuppressed
	volRepo.addVolunteer(vS)

	repo := newCappingCreditRepo()
	repo.suppress[rSuppressed.ID] = true
	racRepo := newMockRACRepo()
	rec := &fakeStandingRecorder{}
	relRepo := &fakeReliabilityRepo{}
	trustRepo := newFakeTrustRepo()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, repo, racRepo, volRepo, newMockAssignmentRepo(), attRepo, relRepo, signer, testLogger(), trustRepo, transition.TrustPolicy{DefaultFloor: 25}).
		WithStandingBackpressure(rec).
		WithEmissionCap(cap)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}

	// The unit still validates (F3: suppression is a non-error branch).
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2 (both results agreed)", len(vr.AgreedResults))
	}
	if rGranted.ValidationStatus != result.ValidationAgreed || rSuppressed.ValidationStatus != result.ValidationAgreed {
		t.Errorf("result statuses = (%s, %s), want both AGREED", rGranted.ValidationStatus, rSuppressed.ValidationStatus)
	}

	// CreateCapped was consulted for both, with the configured cap.
	if len(repo.cappedCalls) != 2 {
		t.Errorf("CreateCapped calls = %d, want 2", len(repo.cappedCalls))
	}
	if repo.lastCapArg != cap {
		t.Errorf("CreateCapped capPerDay = %v, want %v", repo.lastCapArg, cap)
	}

	// Ledger: exactly the granted result got a row.
	if len(vr.CreditEntries) != 1 {
		t.Fatalf("CreditEntries = %d, want 1 (only the non-suppressed grant)", len(vr.CreditEntries))
	}
	if vr.CreditEntries[0].ResultID != rGranted.ID {
		t.Errorf("ledger row ResultID = %v, want the granted result %v", vr.CreditEntries[0].ResultID, rGranted.ID)
	}
	if len(repo.entries) != 1 {
		t.Errorf("repo ledger rows = %d, want 1", len(repo.entries))
	}

	// RAC: upserted only for the granted volunteer.
	if len(racRepo.upserts) != 1 {
		t.Fatalf("RAC upserts = %d, want 1 (RAC is credit-derived; suppressed grant skips it)", len(racRepo.upserts))
	}
	if racRepo.upserts[0].VolunteerID != volGranted {
		t.Errorf("RAC upsert volunteer = %v, want the granted volunteer %v", racRepo.upserts[0].VolunteerID, volGranted)
	}

	// Attestations: both AGREED, but credit is the amount ACTUALLY granted (full vs 0).
	if len(attRepo.attestations) != 2 {
		t.Fatalf("attestations = %d, want 2", len(attRepo.attestations))
	}
	gotGranted := attestationForKey(t, attRepo.attestations, pkGranted)
	gotSuppressed := attestationForKey(t, attRepo.attestations, pkSuppressed)
	if gotGranted.ValidationOutcome != attestation.OutcomeAgreed || gotGranted.CreditAmount != leafCredit {
		t.Errorf("granted attestation = (%s, %v), want (AGREED, %v)", gotGranted.ValidationOutcome, gotGranted.CreditAmount, leafCredit)
	}
	if gotSuppressed.ValidationOutcome != attestation.OutcomeAgreed || gotSuppressed.CreditAmount != 0 {
		t.Errorf("suppressed attestation = (%s, %v), want (AGREED, 0)", gotSuppressed.ValidationOutcome, gotSuppressed.CreditAmount)
	}

	// Work-quality effects fire for BOTH results (F10).
	if volRepo.completedInc[volGranted] != 1 || volRepo.completedInc[volSuppressed] != 1 {
		t.Errorf("completed increments = (granted %d, suppressed %d), want 1 each",
			volRepo.completedInc[volGranted], volRepo.completedInc[volSuppressed])
	}
	standingFolds := multiset(rec.calls)
	if standingFolds[bpCall{vol: volGranted, agreed: true}] != 1 || standingFolds[bpCall{vol: volSuppressed, agreed: true}] != 1 {
		t.Errorf("standing folds = %v, want one agreed=true fold for each volunteer", standingFolds)
	}
	if good, bad := relRepo.countGood(); good != 2 || bad != 0 {
		t.Errorf("reliability signals: good=%d bad=%d, want good=2 bad=0 (both are corroborated work)", good, bad)
	}
	if trustRepo.accrued["did:plc:granted"] != 1 || trustRepo.accrued["did:plc:suppressed"] != 1 {
		t.Errorf("trust accruals = (granted %d, suppressed %d), want 1 each (trust ramps on corroborated-clean work, not credit)",
			trustRepo.accrued["did:plc:granted"], trustRepo.accrued["did:plc:suppressed"])
	}
}

// TestEmissionCap_RepoWithoutCapabilityFallsBackAndWarnsOnce proves the misconfiguration path:
// a cap configured against a credit repo that does NOT implement CappedCreator falls back to
// uncapped Create for the whole batch (validation still succeeds), and the loud WARN fires at
// most ONCE per engine lifetime even across multiple validations.
func TestEmissionCap_RepoWithoutCapabilityFallsBackAndWarnsOnce(t *testing.T) {
	// Shared plain credit repo (mockCreditRepo does NOT implement credit.CappedCreator) and a
	// captured-log engine, exercised over two independent units.
	leafID := types.NewID()
	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)

	resultRepo := newMockResultRepo()
	wuRepo := newMockWorkUnitRepo()
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()

	type unit struct {
		wuID types.ID
	}
	var units []unit
	for i := 0; i < 2; i++ {
		wuID := types.NewID()
		v1 := types.NewID()
		v2 := types.NewID()
		wuRepo.addWorkUnit(makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted))
		resultRepo.addResult(makeResult(wuID, v1, "aaaa", nil))
		resultRepo.addResult(makeResult(wuID, v2, "aaaa", nil))
		volRepo.addVolunteer(makeVolunteer(v1))
		volRepo.addVolunteer(makeVolunteer(v2))
		units = append(units, unit{wuID: wuID})
	}

	creditRepo := newMockCreditRepo() // no CappedCreator capability
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, logger, nil, transition.TrustPolicy{}).
		WithEmissionCap(10.0)

	for _, u := range units {
		vr, err := engine.TryValidate(context.Background(), u.wuID)
		if err != nil {
			t.Fatalf("TryValidate(%v): %v", u.wuID, err)
		}
		if vr.Outcome != OutcomeValidated {
			t.Fatalf("Outcome = %q, want VALIDATED (misconfiguration must not fail validation)", vr.Outcome)
		}
	}

	// Both units credited via the Create fallback: 2 units x 2 results = 4 rows.
	if len(creditRepo.entries) != 4 {
		t.Errorf("ledger entries = %d, want 4 (uncapped Create fallback for both units)", len(creditRepo.entries))
	}
	const warnMsg = "emission cap configured but credit repository does not support capped creation"
	if n := strings.Count(buf.String(), warnMsg); n != 1 {
		t.Errorf("misconfiguration WARN count = %d, want exactly 1 (once per engine lifetime)", n)
	}
}

// TestEmissionCap_CreateCappedHardErrorFailsValidation proves a REAL error from CreateCapped is
// not swallowed: it propagates out exactly like a Create error would today, failing the
// validation (only cap SUPPRESSION — inserted=false, nil error — is the non-error branch).
func TestEmissionCap_CreateCappedHardErrorFailsValidation(t *testing.T) {
	errBoom := errors.New("capped insert failed")

	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
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

	repo := newCappingCreditRepo()
	repo.createCappedErr = errBoom

	engine := NewEngine(resultRepo, wuRepo, leafRepo, repo, nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{}).
		WithEmissionCap(10.0)

	_, err := engine.TryValidate(context.Background(), wuID)
	if err == nil {
		t.Fatal("expected TryValidate to return the CreateCapped error, got nil")
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("error = %v, want it to wrap %v", err, errBoom)
	}
}

// TestEmissionCap_RejectAllAttestsZero is a regression on the createAttestations signature
// change: the rejectAll path passes a nil amounts map, so every DISAGREED attestation must still
// carry credit 0.
func TestEmissionCap_RejectAllAttestsZero(t *testing.T) {
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
	for i, vid := range []types.ID{vol1, vol2} {
		v := makeVolunteer(vid)
		v.PublicKey = make([]byte, ed25519.PublicKeySize)
		v.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(v)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), attRepo, nil, signer, testLogger(), nil, transition.TrustPolicy{}).
		WithEmissionCap(10.0)

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
	}
	if len(attRepo.attestations) != 2 {
		t.Fatalf("attestations = %d, want 2", len(attRepo.attestations))
	}
	for _, att := range attRepo.attestations {
		if att.ValidationOutcome != attestation.OutcomeDisagreed {
			t.Errorf("outcome = %q, want DISAGREED", att.ValidationOutcome)
		}
		if att.CreditAmount != 0 {
			t.Errorf("credit_amount = %v, want 0 (nil amounts map)", att.CreditAmount)
		}
	}
}

// attestationForKey returns the single attestation whose VolunteerPublicKey matches pk.
func attestationForKey(t *testing.T, atts []*attestation.Attestation, pk []byte) *attestation.Attestation {
	t.Helper()
	for _, att := range atts {
		if bytes.Equal(att.VolunteerPublicKey, pk) {
			return att
		}
	}
	t.Fatalf("no attestation for public key %x", pk)
	return nil
}
