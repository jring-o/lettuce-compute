package attestation

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// These tests pin the WORKED EXAMPLE published in guides/attestation-verification.md §9 to
// the real canonicalizer and verifier: the guide's canonical byte strings must be exactly
// what CanonicalJSON produces for those field values, and its signatures must verify against
// its illustrative public key. If either drifts — a canonical-form change, or an edit to the
// guide's literals — this test fails and the two must be updated TOGETHER. The guide is the
// public verification contract; it must never describe bytes the head would not produce.

const (
	recipePublicKey = "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"

	recipeGrantCanonical = `{"attestation_timestamp":"2026-07-10T12:34:56.000000Z","context":"lettuce/credit-attestation/v2","credit_amount":"1.500000","leaf_id":"11111111-1111-1111-1111-111111111111","output_checksum":"3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea","policy_version":1,"quorum_descriptor":{"audit_rate_ppm":1000000,"group_size":3,"min_quorum":3,"min_trusted_corroborators":1,"pending_size":3,"target_copies":5,"trust_floor":100,"trusted_corroborators":2},"result_id":"33333333-3333-3333-3333-333333333333","schema_version":2,"validation_outcome":"AGREED","volunteer_public_key":"Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc","work_unit_id":"22222222-2222-2222-2222-222222222222"}`
	recipeGrantSignature = "wQ2Dx18WtjksEmKkGZMQCwRLvXrp4xXwUwnXw4FSEVVcXXCvGwibgdMS1BQifxb4BpiJi0Bt9WCZRcQdsq_cCg"

	recipeRevocationCanonical = `{"adjustment_id":"55555555-5555-5555-5555-555555555555","attestation_timestamp":"2026-07-10T13:00:00.000000Z","context":"lettuce/credit-attestation-revocation/v2","credit_amount":"1.500000","leaf_id":"11111111-1111-1111-1111-111111111111","reason":"OPERATOR_CLAWBACK","result_id":"33333333-3333-3333-3333-333333333333","revokes_attestation_id":"44444444-4444-4444-4444-444444444444","schema_version":2,"volunteer_public_key":"Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc","work_unit_id":"22222222-2222-2222-2222-222222222222"}`
	recipeRevocationSignature = "6gzkqfjEjlDi03G2VKKvRdbQqzxhlnuo3pxTFob_Cn49ZASkrvSvfH3b6jfv2srg0t0sDkuEVUyxhF3BRHcBAg"
)

func recipeID(t *testing.T, s string) types.ID {
	t.Helper()
	id, err := types.ParseID(s)
	if err != nil {
		t.Fatalf("parse id %q: %v", s, err)
	}
	return id
}

func recipeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64url %q: %v", s, err)
	}
	return b
}

func TestRecipeWorkedExample_GrantMatchesCanonicalizer(t *testing.T) {
	resultID := recipeID(t, "33333333-3333-3333-3333-333333333333")
	checksum := "3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea"
	policyVersion := 1
	att := &Attestation{
		SchemaVersion:      SchemaVersionV2,
		LeafID:             recipeID(t, "11111111-1111-1111-1111-111111111111"),
		VolunteerPublicKey: recipeB64(t, "Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc"),
		WorkUnitID:         recipeID(t, "22222222-2222-2222-2222-222222222222"),
		ResultID:           &resultID,
		OutputChecksum:     &checksum,
		QuorumDescriptor: &QuorumDescriptor{
			AuditRatePPM: 1000000, GroupSize: 3, MinQuorum: 3, MinTrustedCorroborators: 1,
			PendingSize: 3, TargetCopies: 5, TrustFloor: 100, TrustedCorroborators: 2,
		},
		PolicyVersion:         &policyVersion,
		ValidationOutcome:     OutcomeAgreed,
		CreditAmount:          1.5,
		CreditAmountCanonical: "1.500000",
		AttestationTimestamp:  time.Date(2026, 7, 10, 12, 34, 56, 0, time.UTC),
	}

	got, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != recipeGrantCanonical {
		t.Errorf("guide grant example drifted from the canonicalizer\n got: %s\nwant: %s", got, recipeGrantCanonical)
	}

	pub := ed25519.PublicKey(recipeB64(t, recipePublicKey))
	if !ed25519.Verify(pub, got, recipeB64(t, recipeGrantSignature)) {
		t.Error("guide grant signature does not verify over the canonicalizer's bytes")
	}
}

func TestRecipeWorkedExample_RevocationMatchesCanonicalizer(t *testing.T) {
	resultID := recipeID(t, "33333333-3333-3333-3333-333333333333")
	revokes := recipeID(t, "44444444-4444-4444-4444-444444444444")
	adjustment := recipeID(t, "55555555-5555-5555-5555-555555555555")
	reason := "OPERATOR_CLAWBACK"
	att := &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                recipeID(t, "11111111-1111-1111-1111-111111111111"),
		VolunteerPublicKey:    recipeB64(t, "Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc"),
		WorkUnitID:            recipeID(t, "22222222-2222-2222-2222-222222222222"),
		ResultID:              &resultID,
		RevokesAttestationID:  &revokes,
		AdjustmentID:          &adjustment,
		Reason:                &reason,
		ValidationOutcome:     OutcomeRevoked,
		CreditAmount:          1.5,
		CreditAmountCanonical: "1.500000",
		AttestationTimestamp:  time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC),
	}

	got, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != recipeRevocationCanonical {
		t.Errorf("guide revocation example drifted from the canonicalizer\n got: %s\nwant: %s", got, recipeRevocationCanonical)
	}

	pub := ed25519.PublicKey(recipeB64(t, recipePublicKey))
	if !ed25519.Verify(pub, got, recipeB64(t, recipeRevocationSignature)) {
		t.Error("guide revocation signature does not verify over the canonicalizer's bytes")
	}
}
