package validation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Wiring tests for attestation v2 (design §8.3): the engine threads the comparison verdict
// and resolved redundancy policy into signed v2 attestations, for both the accept and the
// reject effects paths.

func v2WiringEngine(t *testing.T, attRepo *mockAttestationRepo, fixtures func(rr *mockResultRepo, wr *mockWorkUnitRepo, lr *mockLeafRepo, vr *mockVolunteerRepo)) *Engine {
	t.Helper()
	resultRepo := newMockResultRepo()
	wuRepo := newMockWorkUnitRepo()
	leafRepo := newMockLeafRepo()
	volRepo := newMockVolunteerRepo()
	fixtures(resultRepo, wuRepo, leafRepo, volRepo)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer := attestation.NewSigner(priv)
	return NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo,
		newMockAssignmentRepo(), attRepo, nil, signer, testLogger(), nil, transition.TrustPolicy{})
}

// TestV2Attestation_FieldsAndDescriptor: a validated unit's AGREED and DISAGREED results all
// get schema_version-2 attestations carrying result binding, the head-computed output
// checksum, policy_version, the unit's shared quorum descriptor (delivered AND demanded
// numbers), and the fixed-scale canonical credit string ("1.000000" granted / "0.000000"
// rejected). Fails on pre-slice-4 code (BG-06a items 2: no result binding, no descriptor).
func TestV2Attestation_FieldsAndDescriptor(t *testing.T) {
	leafID, wuID := types.NewID(), types.NewID()
	vol1, vol2, vol3 := types.NewID(), types.NewID(), types.NewID()
	agreeSum := strings.Repeat("ab", 32) // 64 hex chars
	dissentSum := strings.Repeat("cd", 32)

	var r1ID, r2ID, r3ID types.ID
	attRepo := newMockAttestationRepo()
	engine := v2WiringEngine(t, attRepo, func(rr *mockResultRepo, wr *mockWorkUnitRepo, lr *mockLeafRepo, vr *mockVolunteerRepo) {
		lr.addLeaf(makeLeaf(leafID, 2, 0.6, "EXACT", nil, 1.0))
		wr.addWorkUnit(makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted))
		r1 := makeResult(wuID, vol1, agreeSum, nil)
		r2 := makeResult(wuID, vol2, agreeSum, nil)
		r3 := makeResult(wuID, vol3, dissentSum, nil)
		r1ID, r2ID, r3ID = r1.ID, r2.ID, r3.ID
		rr.addResult(r1)
		rr.addResult(r2)
		rr.addResult(r3)
		for i, id := range []types.ID{vol1, vol2, vol3} {
			v := makeVolunteer(id)
			v.PublicKey = make([]byte, ed25519.PublicKeySize)
			v.PublicKey[0] = byte(i + 1)
			vr.addVolunteer(v)
		}
	})

	vres, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vres.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vres.Outcome)
	}
	if len(attRepo.attestations) != 3 {
		t.Fatalf("attestations = %d, want 3 (2 agreed + 1 disagreed)", len(attRepo.attestations))
	}

	wantChecksum := map[types.ID]string{r1ID: agreeSum, r2ID: agreeSum, r3ID: dissentSum}
	wantOutcome := map[types.ID]string{
		r1ID: attestation.OutcomeAgreed,
		r2ID: attestation.OutcomeAgreed,
		r3ID: attestation.OutcomeDisagreed,
	}
	wantCanonical := map[types.ID]string{r1ID: "1.000000", r2ID: "1.000000", r3ID: "0.000000"}

	for _, att := range attRepo.attestations {
		if att.SchemaVersion != attestation.SchemaVersionV2 {
			t.Errorf("schema_version = %d, want 2 (hard cutover: no code path writes v1)", att.SchemaVersion)
		}
		if att.ResultID == nil {
			t.Fatal("result_id missing from v2 attestation")
		}
		rid := *att.ResultID
		if att.ValidationOutcome != wantOutcome[rid] {
			t.Errorf("outcome = %q, want %q for result %s", att.ValidationOutcome, wantOutcome[rid], rid)
		}
		if att.OutputChecksum == nil || *att.OutputChecksum != wantChecksum[rid] {
			t.Errorf("output_checksum = %v, want %q", att.OutputChecksum, wantChecksum[rid])
		}
		if att.CreditAmountCanonical != wantCanonical[rid] {
			t.Errorf("credit canonical = %q, want %q", att.CreditAmountCanonical, wantCanonical[rid])
		}
		if att.PolicyVersion == nil || *att.PolicyVersion != attestation.PolicyVersion {
			t.Errorf("policy_version = %v, want %d", att.PolicyVersion, attestation.PolicyVersion)
		}
		d := att.QuorumDescriptor
		if d == nil {
			t.Fatal("quorum_descriptor missing from v2 grant/reject attestation")
		}
		// Delivered: 2 coherent agreeing subjects of 3 compared; nobody trusted (scores 0,
		// resolved floor clamps to 1). Demanded: redundancy 2 => target == quorum == 2; the
		// trust gate is off (K 0); audits disabled => rate 0 ppm.
		if d.GroupSize != 2 || d.PendingSize != 3 || d.TrustedCorroborators != 0 {
			t.Errorf("delivered descriptor = %+v, want group 2 / pending 3 / trusted 0", d)
		}
		if d.MinQuorum != 2 || d.TargetCopies != 2 || d.MinTrustedCorroborators != 0 || d.TrustFloor != 1 {
			t.Errorf("demanded descriptor = %+v, want quorum 2 / target 2 / K 0 / floor 1", d)
		}
		if d.AuditRatePPM != 0 {
			t.Errorf("audit_rate_ppm = %d, want 0 (audits disabled)", d.AuditRatePPM)
		}
	}
}

// TestRejectAll_DescriptorCarriesLosingClique (audit F-M1): on a rejected unit the verdict
// carries the LOSING clique — the largest coherent agreeing group that failed the gates —
// and the DISAGREED attestations' descriptor reports its size honestly (group_size 2 of 3
// here, NOT 0; outcome DISAGREED + credit 0 state the consequence).
func TestRejectAll_DescriptorCarriesLosingClique(t *testing.T) {
	leafID, wuID := types.NewID(), types.NewID()
	vol1, vol2, vol3 := types.NewID(), types.NewID(), types.NewID()
	cliqueSum := strings.Repeat("ab", 32)
	dissentSum := strings.Repeat("cd", 32)

	attRepo := newMockAttestationRepo()
	engine := v2WiringEngine(t, attRepo, func(rr *mockResultRepo, wr *mockWorkUnitRepo, lr *mockLeafRepo, vr *mockVolunteerRepo) {
		// threshold 1.0: the 2-of-3 clique fails the agreement ratio; no active assignments
		// remain, so the unit rejects this round.
		lr.addLeaf(makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0))
		wr.addWorkUnit(makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted))
		rr.addResult(makeResult(wuID, vol1, cliqueSum, nil))
		rr.addResult(makeResult(wuID, vol2, cliqueSum, nil))
		rr.addResult(makeResult(wuID, vol3, dissentSum, nil))
		for i, id := range []types.ID{vol1, vol2, vol3} {
			v := makeVolunteer(id)
			v.PublicKey = make([]byte, ed25519.PublicKeySize)
			v.PublicKey[0] = byte(i + 1)
			vr.addVolunteer(v)
		}
	})

	vres, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vres.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED", vres.Outcome)
	}
	if len(attRepo.attestations) != 3 {
		t.Fatalf("attestations = %d, want 3", len(attRepo.attestations))
	}
	for _, att := range attRepo.attestations {
		if att.ValidationOutcome != attestation.OutcomeDisagreed {
			t.Errorf("outcome = %q, want DISAGREED", att.ValidationOutcome)
		}
		if att.CreditAmountCanonical != "0.000000" {
			t.Errorf("credit canonical = %q, want 0.000000", att.CreditAmountCanonical)
		}
		d := att.QuorumDescriptor
		if d == nil {
			t.Fatal("quorum_descriptor missing")
		}
		if d.GroupSize != 2 || d.PendingSize != 3 {
			t.Errorf("descriptor = %+v, want losing-clique group_size 2 / pending 3", d)
		}
	}
}

// TestEffectiveAuditRatePPM pins the descriptor's integer rate rendering: MAX(head, leaf)
// when enabled, 0 when disabled, and parts-per-million granularity (a sub-ppm rate rounds
// to 0 — documented in the verification recipe).
func TestEffectiveAuditRatePPM(t *testing.T) {
	leafID := types.NewID()
	attRepo := newMockAttestationRepo()
	engine := v2WiringEngine(t, attRepo, func(rr *mockResultRepo, wr *mockWorkUnitRepo, lr *mockLeafRepo, vr *mockVolunteerRepo) {})

	cases := []struct {
		name     string
		enabled  bool
		headRate float64
		leafRate float64
		wantPPM  int
	}{
		{"disabled", false, 0.01, 0.5, 0},
		{"head only", true, 0.01, 0, 10000},
		{"leaf raises", true, 0.01, 0.5, 500000},
		{"leaf below head cannot lower", true, 0.01, 0.005, 10000},
		{"full rate", true, 1.0, 0, 1000000},
		{"sub-ppm rounds to zero", true, 1e-7, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := engine.WithResultAudits(nil, tc.enabled, tc.headRate, nil)
			proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
			proj.ValidationConfig.AuditRate = tc.leafRate
			if got := e.effectiveAuditRatePPM(proj); got != tc.wantPPM {
				t.Errorf("effectiveAuditRatePPM = %d, want %d", got, tc.wantPPM)
			}
		})
	}
}
